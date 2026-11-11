package room

import (
	"math"
	"strings"
	"unicode/utf8"
)

var allowedEmoji = map[string]bool{"❤️": true, "😂": true, "😱": true, "🔥": true, "👏": true, "🍿": true, "😭": true, "🤯": true}

func sanePos(p float64) float64 {
	if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 {
		return 0
	}
	return p
}

// Handle applies a client message.
func (r *Room) Handle(c *Client, m In) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.clients[c.ID]; !ok {
		return
	}
	now := nowMs()
	switch m.Type {
	case "ping":
		r.sendLocked(c, Out{Type: "pong", T: m.T, Server: now})

	case "play":
		if r.media == nil {
			return
		}
		r.state.Position = sanePos(m.Position)
		r.state.Paused = false
		r.state.Waiting = r.anyBufferingLocked()
		r.state.UpdatedAt = now
		r.broadcastStateLocked(c, "play")

	case "pause":
		if r.media == nil {
			return
		}
		r.state.Position = sanePos(m.Position)
		r.state.Paused = true
		r.state.Waiting = false
		r.state.UpdatedAt = now
		r.broadcastStateLocked(c, "pause")

	case "seek":
		if r.media == nil {
			return
		}
		r.state.Position = sanePos(m.Position)
		r.state.UpdatedAt = now
		r.broadcastStateLocked(c, "seek")

	case "rate":
		if r.media == nil || m.Rate < 0.25 || m.Rate > 2 {
			return
		}
		r.state.Position = r.state.PositionAt(now)
		r.state.Rate = m.Rate
		r.state.UpdatedAt = now
		r.broadcastStateLocked(c, "rate")

	case "buffering":
		if c.Buffering == m.Value {
			return
		}
		c.Buffering = m.Value
		r.broadcastLocked(Out{Type: "peers", Peers: r.peersLocked()})
		r.reevaluateWaitLocked(c)

	case "chat":
		text := strings.TrimSpace(m.Text)
		if text == "" {
			return
		}
		if utf8.RuneCountInString(text) > 1000 {
			text = string([]rune(text)[:1000])
		}
		r.addChatLocked(ChatMsg{From: c.ID, Name: c.Name, Text: text})

	case "reaction":
		if !allowedEmoji[m.Emoji] {
			return
		}
		r.broadcastLocked(Out{Type: "reaction", Emoji: m.Emoji, By: c.ID, ByName: c.Name})

	case "name":
		n := cleanName(m.Name)
		if n == c.Name {
			return
		}
		old := c.Name
		c.Name = n
		r.systemLocked(old + " теперь " + n)
		r.broadcastLocked(Out{Type: "peers", Peers: r.peersLocked()})
	}
}

func (r *Room) anyBufferingLocked() bool {
	for _, c := range r.clients {
		if c.Buffering {
			return true
		}
	}
	return false
}

// reevaluateWaitLocked freezes playback for everyone while somebody buffers
// and resumes it once all participants are ready again.
func (r *Room) reevaluateWaitLocked(by *Client) {
	if r.media == nil || r.state.Paused {
		r.state.Waiting = false
		return
	}
	want := r.anyBufferingLocked()
	if want == r.state.Waiting {
		return
	}
	now := nowMs()
	r.state.Position = r.state.PositionAt(now)
	r.state.Waiting = want
	r.state.UpdatedAt = now
	if want {
		r.broadcastStateLocked(by, "wait")
	} else {
		r.broadcastStateLocked(by, "resume")
	}
}

// SetMedia opens a new movie and resets playback to the start (paused).
func (r *Room) SetMedia(m *Media) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.media = m
	for _, c := range r.clients {
		c.Buffering = false
	}
	r.state = State{Paused: true, Rate: 1, UpdatedAt: nowMs()}
	r.broadcastLocked(Out{Type: "media", Media: m, Server: nowMs()})
	r.broadcastStateLocked(nil, "media")
	r.broadcastLocked(Out{Type: "peers", Peers: r.peersLocked()})
	r.systemLocked("Фильм: " + m.Name)
}

// Media returns the currently opened movie (nil if none).
func (r *Room) Media() *Media {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.media
}

// State returns a copy of the playback state.
func (r *Room) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// SetTunnel updates the public link status and pushes it to hosts.
func (r *Room) SetTunnel(t Tunnel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tunnel = t
	for _, c := range r.clients {
		if c.Host {
			tt := t
			r.sendLocked(c, Out{Type: "tunnel", Tunnel: &tt})
		}
	}
}

// Tunnel returns the current tunnel status.
func (r *Room) Tunnel() Tunnel {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tunnel
}
