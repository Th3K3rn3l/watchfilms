package p2p

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"watchfilms/internal/room"
)

func TestP2PEndToEndLoopback(t *testing.T) {
	var mu sync.Mutex
	rooms := make(map[string]map[string]string)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if r.Method == http.MethodPost {
			var p SignalPayload
			_ = json.NewDecoder(r.Body).Decode(&p)
			if rooms[p.Room] == nil {
				rooms[p.Room] = make(map[string]string)
			}
			rooms[p.Room][p.Action] = p.SDP
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}

		if r.Method == http.MethodGet {
			room := r.URL.Query().Get("room")
			action := r.URL.Query().Get("action")
			if sdp, ok := rooms[room][action]; ok && sdp != "" {
				_ = json.NewEncoder(w).Encode(SignalPayload{Action: action, Room: room, SDP: sdp})
				return
			}
			http.NotFound(w, r)
			return
		}
	}))
	defer ts.Close()

	rm := room.New()
	urlCh := make(chan string, 1)

	mgr := NewManager(Config{
		Room:      rm,
		SignalURL: ts.URL,
		Update: func(status, url, msg, provider string) {
			if status == "ready" && strings.Contains(url, "#o=") {
				select {
				case urlCh <- url:
				default:
				}
			}
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 2. Start Host Manager in goroutine
	go mgr.Run(ctx)

	// 3. Wait for invite URL
	var inviteURL string
	select {
	case inviteURL = <-urlCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("Invite URL was not generated in time")
	}

	// 4. Friend parses invite URL
	parts := strings.Split(inviteURL, "#o=")
	if len(parts) != 2 {
		t.Fatalf("Invite URL has no #o= hash: %s", inviteURL)
	}
	b64Offer := parts[1]
	rawOffer, err := base64.RawURLEncoding.DecodeString(b64Offer)
	if err != nil {
		t.Fatalf("Failed to decode base64 offer: %v", err)
	}

	normOffer := NormalizeSDP(string(rawOffer))
	if !strings.HasPrefix(normOffer, "v=0") {
		t.Fatalf("Decoded offer does not start with v=0: %s", normOffer)
	}

	// 5. Friend creates PeerConnection and Answer
	friendPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("Friend NewPeerConnection: %v", err)
	}
	defer friendPC.Close()

	connectedCh := make(chan struct{})
	friendPC.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() == "control" {
			dc.OnOpen(func() {
				close(connectedCh)
			})
		}
	})

	err = friendPC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  normOffer,
	})
	if err != nil {
		t.Fatalf("Friend SetRemoteDescription failed: %v", err)
	}

	answer, err := friendPC.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("Friend CreateAnswer failed: %v", err)
	}
	if err := friendPC.SetLocalDescription(answer); err != nil {
		t.Fatalf("Friend SetLocalDescription failed: %v", err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(friendPC)
	select {
	case <-gatherComplete:
	case <-time.After(2 * time.Second):
	}

	mu.Lock()
	var roomID string
	for rID := range rooms {
		roomID = rID
		break
	}
	mu.Unlock()

	sigClient, _ := NewSignalingClient(ts.URL)
	friendDesc := friendPC.LocalDescription()
	if err := sigClient.PostAnswer(ctx, roomID, NormalizeSDP(friendDesc.SDP)); err != nil {
		t.Fatalf("Friend PostAnswer failed: %v", err)
	}

	// 6. Verify connection established!
	select {
	case <-connectedCh:
		t.Log("SUCCESS! P2P DataChannel connected via offer/answer loopback!")
	case <-time.After(5 * time.Second):
		t.Fatalf("Timed out waiting for DataChannel to connect")
	}
}
