package p2p

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestSignalingClient(t *testing.T) {
	var mu sync.Mutex
	rooms := make(map[string]map[string]string)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if r.Method == http.MethodPost && r.URL.Path == "/api/signal" {
			var p SignalPayload
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if rooms[p.Room] == nil {
				rooms[p.Room] = make(map[string]string)
			}
			rooms[p.Room][p.Action] = p.SDP
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			return
		}

		if r.Method == http.MethodGet && r.URL.Path == "/api/signal" {
			q := r.URL.Query()
			room := q.Get("room")
			action := q.Get("action")
			if rooms[room] != nil && rooms[room][action] != "" {
				_ = json.NewEncoder(w).Encode(SignalPayload{
					Action: action,
					Room:   room,
					SDP:    rooms[room][action],
				})
				return
			}
			http.Error(w, "not found", 404)
			return
		}
	}))
	defer ts.Close()

	client, err := NewSignalingClient(ts.URL)
	if err != nil {
		t.Fatalf("NewSignalingClient failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 1. Post Offer
	testOffer := "v=0\r\no=- 123 2 IN IP4 127.0.0.1\r\ns=-\r\n"
	if err := client.PostOffer(ctx, "test-room", testOffer); err != nil {
		t.Fatalf("PostOffer failed: %v", err)
	}

	// 2. Get Offer
	offer, err := client.GetOffer(ctx, "test-room")
	if err != nil || offer != testOffer {
		t.Fatalf("GetOffer returned '%s', err: %v", offer, err)
	}

	// 3. Post Answer
	testAnswer := "v=0\r\no=- 456 2 IN IP4 127.0.0.1\r\ns=-\r\n"
	if err := client.PostAnswer(ctx, "test-room", testAnswer); err != nil {
		t.Fatalf("PostAnswer failed: %v", err)
	}

	// 4. Poll Answer
	answer, err := client.PollAnswer(ctx, "test-room", 50*time.Millisecond)
	if err != nil || answer != testAnswer {
		t.Fatalf("PollAnswer returned '%s', err: %v", answer, err)
	}
}
