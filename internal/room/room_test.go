package room

import (
	"testing"
)

func drain(c *Client) []Out {
	var out []Out
	for {
		select {
		case m := <-c.Send:
			out = append(out, m)
		default:
			return out
		}
	}
}

func lastState(t *testing.T, c *Client) State {
	t.Helper()
	var st *State
	for _, m := range drain(c) {
		if m.State != nil {
			st = m.State
		}
	}
	if st == nil {
		t.Fatal("no state message")
	}
	return *st
}

func withClock(t *testing.T) *int64 {
	now := int64(1_000_000)
	old := nowMs
	nowMs = func() int64 { return now }
	t.Cleanup(func() { nowMs = old })
	return &now
}

func TestPlayPauseSeekSync(t *testing.T) {
	now := withClock(t)
	r := New()
	a, b := r.Join("A", true), r.Join("B", false)
	r.SetMedia(&Media{ID: "m", Name: "x.mp4"})
	drain(a)

	r.Handle(a, In{Type: "play", Position: 10})
	st := lastState(t, b)
	if st.Paused || st.Position != 10 {
		t.Fatalf("play not propagated: %+v", st)
	}
	*now += 5000
	if p := st.PositionAt(*now); p != 15 {
		t.Fatalf("expected 15s, got %v", p)
	}
	r.Handle(b, In{Type: "pause", Position: 15.2})
	if st := lastState(t, a); !st.Paused || st.Position != 15.2 {
		t.Fatalf("pause not propagated: %+v", st)
	}
	r.Handle(a, In{Type: "seek", Position: 3600})
	if st := lastState(t, b); st.Position != 3600 || !st.Paused {
		t.Fatalf("seek wrong: %+v", st)
	}
}

func TestBufferingFreezesAndResumes(t *testing.T) {
	now := withClock(t)
	r := New()
	a, b := r.Join("A", true), r.Join("B", false)
	r.SetMedia(&Media{ID: "m"})
	r.Handle(a, In{Type: "play", Position: 0})
	*now += 2000
	r.Handle(b, In{Type: "buffering", Value: true})
	st := lastState(t, a)
	if !st.Waiting || st.Position != 2 {
		t.Fatalf("expected frozen at 2s: %+v", st)
	}
	*now += 10000
	if st.PositionAt(*now) != 2 {
		t.Fatal("position must not advance while waiting")
	}
	r.Handle(b, In{Type: "buffering", Value: false})
	st = lastState(t, a)
	if st.Waiting || st.Paused || st.Position != 2 {
		t.Fatalf("expected resume from 2s: %+v", st)
	}
}

func TestLeaveClearsWaiting(t *testing.T) {
	withClock(t)
	r := New()
	a, b := r.Join("A", true), r.Join("B", false)
	r.SetMedia(&Media{ID: "m"})
	r.Handle(a, In{Type: "play"})
	r.Handle(b, In{Type: "buffering", Value: true})
	r.Leave(b)
	if st := lastState(t, a); st.Waiting {
		t.Fatal("waiting must clear when the buffering peer leaves")
	}
}

func TestPlayWhileSomeoneBuffering(t *testing.T) {
	withClock(t)
	r := New()
	a, b := r.Join("A", true), r.Join("B", false)
	r.SetMedia(&Media{ID: "m"})
	r.Handle(b, In{Type: "buffering", Value: true})
	r.Handle(a, In{Type: "play"})
	if st := lastState(t, a); !st.Waiting {
		t.Fatal("play must start in waiting mode if a peer is buffering")
	}
}

func TestChatAndReactionValidation(t *testing.T) {
	r := New()
	a := r.Join("A", true)
	drain(a)
	r.Handle(a, In{Type: "reaction", Emoji: "<script>"})
	r.Handle(a, In{Type: "chat", Text: "   "})
	if got := drain(a); len(got) != 0 {
		t.Fatalf("invalid input must be ignored, got %+v", got)
	}
	r.Handle(a, In{Type: "reaction", Emoji: "🔥"})
	r.Handle(a, In{Type: "chat", Text: "привет"})
	if got := drain(a); len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
}
