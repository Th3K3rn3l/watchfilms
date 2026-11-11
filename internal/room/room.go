package room

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	historyLimit = 200
	sendBuffer   = 128
)

// Client is a connected participant. Messages for it are queued on Send;
// the transport layer drains the channel.
type Client struct {
	ID        string
	Name      string
	Host      bool
	Buffering bool
	Send      chan Out
}

// Room is the single shared watch room.
type Room struct {
	mu      sync.Mutex
	clients map[string]*Client
	order   []string
	state   State
	media   *Media
	history []ChatMsg
	chatSeq int64
	tunnel  Tunnel
}

// New creates an empty, paused room.
func New() *Room {
	return &Room{
		clients: map[string]*Client{},
		state:   State{Paused: true, Rate: 1, UpdatedAt: nowMs()},
		tunnel:  Tunnel{Status: "off"},
	}
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func cleanName(n string) string {
	n = strings.TrimSpace(n)
	if n == "" {
		return "Гость"
	}
	if utf8.RuneCountInString(n) > 24 {
		n = string([]rune(n)[:24])
	}
	return n
}

// Join registers a new client and sends it the full snapshot.
func (r *Room) Join(name string, host bool) *Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := &Client{ID: newID(), Name: cleanName(name), Host: host, Send: make(chan Out, sendBuffer)}
	r.clients[c.ID] = c
	r.order = append(r.order, c.ID)

	st := r.state
	welcome := Out{Type: "welcome", You: c.ID, Host: host, State: &st, Media: r.media,
		Peers: r.peersLocked(), History: append([]ChatMsg(nil), r.history...), Server: nowMs()}
	if host {
		t := r.tunnel
		welcome.Tunnel = &t
	}
	r.sendLocked(c, welcome)
	r.systemLocked(c.Name + " подключился")
	r.broadcastLocked(Out{Type: "peers", Peers: r.peersLocked()})
	return c
}

// Leave removes a client. A disconnect also clears its buffering flag so the
// other side never stays frozen waiting for a ghost.
func (r *Room) Leave(c *Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.clients[c.ID]; !ok {
		return
	}
	delete(r.clients, c.ID)
	for i, id := range r.order {
		if id == c.ID {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	close(c.Send)
	r.systemLocked(c.Name + " отключился")
	r.broadcastLocked(Out{Type: "peers", Peers: r.peersLocked()})
	r.reevaluateWaitLocked(c)
}

func (r *Room) peersLocked() []Peer {
	ps := make([]Peer, 0, len(r.order))
	for _, id := range r.order {
		c := r.clients[id]
		ps = append(ps, Peer{ID: c.ID, Name: c.Name, Host: c.Host, Buffering: c.Buffering})
	}
	return ps
}

// sendLocked queues a message; a client that cannot keep up is dropped
// (its connection will be closed by the transport when Send closes).
func (r *Room) sendLocked(c *Client, m Out) {
	select {
	case c.Send <- m:
	default:
		go r.Leave(c)
	}
}

func (r *Room) broadcastLocked(m Out) {
	for _, id := range r.order {
		r.sendLocked(r.clients[id], m)
	}
}

func (r *Room) systemLocked(text string) {
	r.addChatLocked(ChatMsg{Text: text, System: true})
}

func (r *Room) addChatLocked(m ChatMsg) {
	r.chatSeq++
	m.ID = r.chatSeq
	m.TS = nowMs()
	r.history = append(r.history, m)
	if len(r.history) > historyLimit {
		r.history = r.history[len(r.history)-historyLimit:]
	}
	r.broadcastLocked(Out{Type: "chat", Chat: &m})
}

func (r *Room) broadcastStateLocked(by *Client, action string) {
	st := r.state
	m := Out{Type: "state", State: &st, Action: action, Server: nowMs()}
	if by != nil {
		m.By, m.ByName = by.ID, by.Name
	}
	r.broadcastLocked(m)
}
