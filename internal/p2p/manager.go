package p2p

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"watchfilms/internal/room"
)

// DefaultSTUNServers provide fast and reliable STUN discovery in Russia and globally.
var DefaultSTUNServers = []string{
	"stun:stun.yandex.ru:3478",
	"stun:stun.l.google.com:19302",
	"stun:stun1.l.google.com:19302",
	"stun:stun.cloudflare.com:3478",
}

// Config configures the P2P connection manager.
type Config struct {
	Room      *room.Room
	SignalURL string
	WebURL    string
	Update    func(status, url, errText, provider string)
}

// Manager orchestrates WebRTC PeerConnection and Yandex Cloud Functions signaling.
type Manager struct {
	cfg       Config
	mu        sync.Mutex
	sigClient *SignalingClient
	pc        *webrtc.PeerConnection
	active    bool
	roomID    string
}

// NewManager creates a P2P manager.
func NewManager(cfg Config) *Manager {
	return &Manager{cfg: cfg}
}

func randomRoomID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "wf-" + hex.EncodeToString(b)
}

func boolPtr(b bool) *bool { return &b }

// Run connects to Yandex Cloud Functions signaling and establishes P2P WebRTC.
func (m *Manager) Run(ctx context.Context) {
	sigURL := strings.TrimSpace(m.cfg.SignalURL)
	if sigURL == "" {
		m.cfg.Update("error", "", "Не указан URL Yandex Cloud Function для P2P", "P2P (Yandex Cloud)")
		return
	}

	sigClient, err := NewSignalingClient(sigURL)
	if err != nil {
		m.cfg.Update("error", "", err.Error(), "P2P (Yandex Cloud)")
		return
	}
	m.sigClient = sigClient

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := m.runSession(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			log.Printf("[p2p] сессия завершилась: %v, перезапуск через 3 с...", err)
			m.cfg.Update("starting", "", fmt.Sprintf("Переподключение P2P: %v", err), "P2P (Yandex Cloud)")
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}
}

func (m *Manager) runSession(ctx context.Context) error {
	m.cfg.Update("starting", "", "Подготовка P2P и сбор STUN-адресов…", "P2P (Yandex Cloud)")

	webrtcCfg := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: DefaultSTUNServers},
		},
	}

	pc, err := webrtc.NewPeerConnection(webrtcCfg)
	if err != nil {
		return fmt.Errorf("ошибка создания WebRTC PeerConnection: %w", err)
	}
	defer pc.Close()

	m.mu.Lock()
	m.pc = pc
	m.mu.Unlock()

	// 1. Control DataChannel (carries play/pause/seek/chat/peers)
	controlDC, err := pc.CreateDataChannel("control", &webrtc.DataChannelInit{
		Ordered: boolPtr(true),
	})
	if err != nil {
		return fmt.Errorf("ошибка создания control DataChannel: %w", err)
	}

	// 2. Media DataChannel (carries video range requests and subtitles)
	mediaDC, err := pc.CreateDataChannel("media", &webrtc.DataChannelInit{
		Ordered: boolPtr(true),
	})
	if err != nil {
		return fmt.Errorf("ошибка создания media DataChannel: %w", err)
	}
	_ = NewMediaHandler(m.cfg.Room, mediaDC)

	// Bind Room client when control channel opens
	var (
		clientMu      sync.Mutex
		c             *room.Client
		sessDone      = make(chan struct{})
		sessCloseOnce sync.Once
	)
	safeCloseSess := func() {
		sessCloseOnce.Do(func() {
			close(sessDone)
		})
	}

	controlDC.OnOpen(func() {
		clientMu.Lock()
		client := m.cfg.Room.Join("Друг (P2P)", false)
		c = client
		clientMu.Unlock()

		log.Printf("[p2p] control channel открыт, подключаю участника к комнате")

		// Goroutine: stream Room updates -> controlDC
		go func() {
			for {
				select {
				case <-sessDone:
					return
				case out, ok := <-client.Send:
					if !ok {
						return
					}
					b, err := json.Marshal(out)
					if err == nil {
						_ = controlDC.SendText(string(b))
					}
				}
			}
		}()
	})

	controlDC.OnMessage(func(msg webrtc.DataChannelMessage) {
		if !msg.IsString {
			return
		}
		var in room.In
		if err := json.Unmarshal(msg.Data, &in); err != nil {
			return
		}
		clientMu.Lock()
		activeClient := c
		clientMu.Unlock()
		if activeClient != nil {
			m.cfg.Room.Handle(activeClient, in)
		}
	})

	controlDC.OnClose(func() {
		log.Printf("[p2p] control channel закрыт")
		clientMu.Lock()
		if c != nil {
			m.cfg.Room.Leave(c)
			c = nil
		}
		clientMu.Unlock()
		safeCloseSess()
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("[p2p] состояние ICE: %s", state)
		if state == webrtc.ICEConnectionStateConnected {
			log.Printf("[p2p] 🔥 Прямое P2P соединение установлено!")
		} else if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateClosed {
			pc.Close()
		}
	})

	// 3. Create Offer and wait for ICE gathering
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("ошибка создания SDP Offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("ошибка SetLocalDescription: %w", err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	select {
	case <-gatherComplete:
	case <-time.After(2500 * time.Millisecond):
		// Proceed with whatever candidates have been gathered so far
	case <-ctx.Done():
		return ctx.Err()
	}

	m.mu.Lock()
	if m.roomID == "" {
		m.roomID = randomRoomID()
	}
	roomID := m.roomID
	m.mu.Unlock()
	localDesc := pc.LocalDescription()
	if localDesc == nil {
		return errors.New("локальное описание SDP отсутствует")
	}

	normOffer := NormalizeSDP(localDesc.SDP)

	// 4. Post Offer to Yandex Cloud Function & ntfy
	if err := m.sigClient.PostOffer(ctx, roomID, normOffer); err != nil {
		log.Printf("[p2p] предупреждение: ошибка отправки Offer в YCF: %v", err)
	}

	// Embed URL-safe base64 offer in the invite link hash!
	encodedOffer := base64.RawURLEncoding.EncodeToString([]byte(normOffer))
	baseURL := strings.TrimRight(m.cfg.WebURL, "/")
	var inviteURL string
	if baseURL != "" {
		inviteURL = fmt.Sprintf("%s/?signal=%s&room=%s#o=%s", baseURL, url.QueryEscape(m.sigClient.BaseURL), roomID, encodedOffer)
		log.Printf("[p2p] комната %s создана! Ссылка для друга (GitHub Pages): %s", roomID, inviteURL)
		m.cfg.Update("ready", inviteURL, "", "P2P (GitHub Pages)")
	} else {
		inviteURL = fmt.Sprintf("%s/?room=%s#o=%s", m.sigClient.BaseURL, roomID, encodedOffer)
		log.Printf("[p2p] комната %s создана! Ссылка для друга: %s", roomID, inviteURL)
		m.cfg.Update("ready", inviteURL, "", "P2P (Yandex Cloud)")
	}

	// 5. Poll for Answer from Friend
	answerSDP, err := m.sigClient.PollAnswer(ctx, roomID, 1*time.Second)
	if err != nil {
		return fmt.Errorf("ошибка получения Answer от друга: %w", err)
	}

	normAnswer := NormalizeSDP(answerSDP)
	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  normAnswer,
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("ошибка SetRemoteDescription: %w", err)
	}

	log.Printf("[p2p] SDP Answer получен от друга, соединяюсь по UDP (STUN hole-punching)...")

	// Wait until session is closed or context cancelled
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-sessDone:
		return errors.New("соединение с другом закрыто")
	}
}

// SetSignalURL updates the signaling URL dynamically at runtime.
func (m *Manager) SetSignalURL(url string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.SignalURL = url
	client, err := NewSignalingClient(url)
	if err != nil {
		return err
	}
	m.sigClient = client
	if m.pc != nil {
		_ = m.pc.Close()
	}
	return nil
}

// SetWebURL updates the frontend web UI URL (e.g. GitHub Pages) at runtime.
func (m *Manager) SetWebURL(webURL string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.WebURL = strings.TrimSpace(webURL)
	if m.pc != nil {
		_ = m.pc.Close()
	}
}
