package p2p

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var sdpLineRegex = regexp.MustCompile(` ([vosieupcbtzkam]=)`)

// NormalizeSDP fixes any newline corruption in SDP text.
// WebRTC requires strict \r\n line endings.
func NormalizeSDP(sdp string) string {
	if sdp == "" {
		return ""
	}
	// If newlines were turned into spaces (e.g. "v=0 o=- ..."):
	if !strings.Contains(sdp, "\n") && !strings.Contains(sdp, "\r") {
		// Demangle lines using standard SDP line prefixes
		sdp = sdpLineRegex.ReplaceAllString(sdp, "\r\n$1")
	}
	// Normalize all newlines to CRLF
	lines := strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n")
	var cleaned []string
	for _, l := range lines {
		l = strings.TrimRight(l, "\r ")
		if l != "" {
			cleaned = append(cleaned, l)
		}
	}
	if len(cleaned) == 0 {
		return ""
	}
	return strings.Join(cleaned, "\r\n") + "\r\n"
}

// SignalingClient talks to the Yandex Cloud Function signaling endpoint.
type SignalingClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

// SignalPayload is the JSON structure sent to/from the signaling endpoint.
type SignalPayload struct {
	Action string `json:"action"` // "offer" | "answer"
	Room   string `json:"room"`
	SDP    string `json:"sdp,omitempty"`
	Error  string `json:"error,omitempty"`
}

// NewSignalingClient creates a new client for Yandex Cloud Functions signaling.
func NewSignalingClient(rawURL string) (*SignalingClient, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, errors.New("пустой URL сигнального сервера")
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		rawURL = "https://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("неверный URL сигнального сервера: %w", err)
	}
	return &SignalingClient{
		BaseURL: strings.TrimRight(u.String(), "/"),
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}, nil
}

// PostOffer sends the WebRTC SDP offer to Yandex Cloud Function.
func (c *SignalingClient) PostOffer(ctx context.Context, room, sdp string) error {
	norm := NormalizeSDP(sdp)
	p := SignalPayload{
		Action: "offer",
		Room:   room,
		SDP:    norm,
	}
	if !strings.Contains(c.BaseURL, "127.0.0.1") && !strings.Contains(c.BaseURL, "localhost") {
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = c.postNtfy(bgCtx, room, "offer", norm)
		}()
	}
	return c.post(ctx, p)
}

// PostAnswer sends the WebRTC SDP answer to Yandex Cloud Function.
func (c *SignalingClient) PostAnswer(ctx context.Context, room, sdp string) error {
	norm := NormalizeSDP(sdp)
	p := SignalPayload{
		Action: "answer",
		Room:   room,
		SDP:    norm,
	}
	if !strings.Contains(c.BaseURL, "127.0.0.1") && !strings.Contains(c.BaseURL, "localhost") {
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = c.postNtfy(bgCtx, room, "answer", norm)
		}()
	}
	return c.post(ctx, p)
}

func (c *SignalingClient) postNtfy(ctx context.Context, room, action, sdp string) error {
	b64 := base64.StdEncoding.EncodeToString([]byte(sdp))
	topicURL := fmt.Sprintf("https://ntfy.sh/wf-%s-%s", url.QueryEscape(room), url.QueryEscape(action))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, topicURL, strings.NewReader(b64))
	if err != nil {
		return err
	}
	req.Header.Set("Title", action)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func (c *SignalingClient) getNtfy(ctx context.Context, room, action string) (string, error) {
	if strings.Contains(c.BaseURL, "127.0.0.1") || strings.Contains(c.BaseURL, "localhost") {
		return "", errors.New("disabled in local tests")
	}
	topicURL := fmt.Sprintf("https://ntfy.sh/wf-%s-%s/raw?poll=1&since=10m", url.QueryEscape(room), url.QueryEscape(action))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, topicURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("not found on ntfy")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil || len(body) == 0 {
		return "", errors.New("empty ntfy body")
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) == 0 {
		return "", errors.New("no lines")
	}
	latest := strings.TrimSpace(lines[len(lines)-1])
	if latest == "" {
		return "", errors.New("empty line")
	}
	if data, err := base64.StdEncoding.DecodeString(latest); err == nil && strings.Contains(string(data), "v=") {
		return string(data), nil
	}
	if data, err := base64.RawURLEncoding.DecodeString(latest); err == nil && strings.Contains(string(data), "v=") {
		return string(data), nil
	}
	if strings.Contains(latest, "v=") {
		return latest, nil
	}
	return "", errors.New("invalid sdp in ntfy")
}

func (c *SignalingClient) postURL() string {
	base := strings.TrimRight(c.BaseURL, "/")
	if strings.Contains(base, "functions.yandexcloud.net") {
		return base
	}
	return base + "/api/signal"
}

func (c *SignalingClient) getURL(room, action string) string {
	base := strings.TrimRight(c.BaseURL, "/")
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	if strings.Contains(base, "functions.yandexcloud.net") {
		return fmt.Sprintf("%s%sroom=%s&action=%s", base, sep, url.QueryEscape(room), url.QueryEscape(action))
	}
	return fmt.Sprintf("%s/api/signal?room=%s&action=%s", base, url.QueryEscape(room), url.QueryEscape(action))
}

func (c *SignalingClient) post(ctx context.Context, payload SignalPayload) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	reqURL := c.postURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("ошибка запроса к YCF: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("YCF ответил кодом %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// GetOffer retrieves the SDP offer for the specified room.
func (c *SignalingClient) GetOffer(ctx context.Context, room string) (string, error) {
	sdp, err := c.get(ctx, room, "offer")
	if err == nil && sdp != "" {
		return NormalizeSDP(sdp), nil
	}
	sdp, err = c.getNtfy(ctx, room, "offer")
	if err == nil && sdp != "" {
		return NormalizeSDP(sdp), nil
	}
	return "", errors.New("offer не найден")
}

// PollAnswer polls the Yandex Cloud Function until an answer is available or ctx is cancelled.
func (c *SignalingClient) PollAnswer(ctx context.Context, room string, pollInterval time.Duration) (string, error) {
	if pollInterval <= 0 {
		pollInterval = 1 * time.Second
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			// 1. Try YCF directly
			if sdp, err := c.get(ctx, room, "answer"); err == nil && sdp != "" {
				return NormalizeSDP(sdp), nil
			}
			// 2. Try ntfy fallback directly
			if sdp, err := c.getNtfy(ctx, room, "answer"); err == nil && sdp != "" {
				return NormalizeSDP(sdp), nil
			}
			// 404 or empty is expected while the friend has not answered yet
		}
	}
}

func (c *SignalingClient) get(ctx context.Context, room, action string) (string, error) {
	reqURL := c.getURL(room, action)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", errors.New("not found")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("код %d: %s", resp.StatusCode, string(body))
	}

	var p SignalPayload
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return "", err
	}
	if p.Error != "" {
		return "", errors.New(p.Error)
	}
	return p.SDP, nil
}
