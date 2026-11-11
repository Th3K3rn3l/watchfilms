// Package room holds the shared playback state, participants, chat and
// the synchronisation rules for a single watch room.
package room

import "time"

// nowMs returns the server clock in Unix milliseconds.
var nowMs = func() int64 { return time.Now().UnixMilli() }

// State is the authoritative playback state. Clients compute the expected
// position as Position + (serverNow - UpdatedAt) * Rate while playing.
type State struct {
	Paused    bool    `json:"paused"`
	Waiting   bool    `json:"waiting"` // someone is buffering, playback frozen for everyone
	Position  float64 `json:"position"`
	Rate      float64 `json:"rate"`
	UpdatedAt int64   `json:"updatedAt"`
}

// Running reports whether the playhead is actually advancing.
func (s State) Running() bool { return !s.Paused && !s.Waiting }

// PositionAt returns the playhead position at server time t (ms).
func (s State) PositionAt(t int64) float64 {
	if !s.Running() {
		return s.Position
	}
	p := s.Position + float64(t-s.UpdatedAt)/1000*s.Rate
	if p < 0 {
		p = 0
	}
	return p
}

// Subtitle describes a subtitle track found next to the movie.
type Subtitle struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
	Path  string `json:"-"`
}

// Media describes the currently opened movie.
type Media struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Size      int64      `json:"size"`
	Path      string     `json:"-"`
	Subtitles []Subtitle `json:"subtitles"`
}

// Peer is the public view of a participant.
type Peer struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Host      bool   `json:"host"`
	Buffering bool   `json:"buffering"`
}

// ChatMsg is a chat line or a system notice.
type ChatMsg struct {
	ID     int64  `json:"id"`
	From   string `json:"from"`
	Name   string `json:"name"`
	Text   string `json:"text"`
	TS     int64  `json:"ts"`
	System bool   `json:"system,omitempty"`
}

// Out is any message sent from the server to a client.
type Out struct {
	Type    string    `json:"type"`
	State   *State    `json:"state,omitempty"`
	Media   *Media    `json:"media,omitempty"`
	Peers   []Peer    `json:"peers,omitempty"`
	Chat    *ChatMsg  `json:"chat,omitempty"`
	History []ChatMsg `json:"history,omitempty"`
	By      string    `json:"by,omitempty"`     // participant id that caused the change
	ByName  string    `json:"byName,omitempty"` // their display name
	Action  string    `json:"action,omitempty"` // play | pause | seek | wait | resume | media
	Emoji   string    `json:"emoji,omitempty"`
	You     string    `json:"you,omitempty"`
	Host    bool      `json:"host,omitempty"`
	T       int64     `json:"t,omitempty"`      // echoed client time (pong)
	Server  int64     `json:"server,omitempty"` // server time (pong / every state)
	Tunnel  *Tunnel   `json:"tunnel,omitempty"`
}

// Tunnel is the public-link status shown to the host.
type Tunnel struct {
	Status   string   `json:"status"` // off | starting | ready | error
	URL      string   `json:"url,omitempty"`
	Error    string   `json:"error,omitempty"`
	Provider string   `json:"provider,omitempty"` // Cloudflare | Pinggy
	LAN      []string `json:"lan,omitempty"`
}

// In is any message received from a client.
type In struct {
	Type     string  `json:"type"`
	Position float64 `json:"position"`
	Value    bool    `json:"value"`
	Text     string  `json:"text"`
	Emoji    string  `json:"emoji"`
	Name     string  `json:"name"`
	Rate     float64 `json:"rate"`
	T        int64   `json:"t"`
}
