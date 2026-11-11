package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"watchfilms/internal/room"
)

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	host := s.isHost(r)
	// Origin is checked by the cookie (SameSite=Lax) + token; tunnels change
	// hostnames, so accept any origin here.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	conn.SetReadLimit(16 << 10)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	c := s.Room.Join(r.URL.Query().Get("name"), host)
	defer s.Room.Leave(c)

	// Writer: drains the client's queue, sends keepalive pings so proxies
	// (Cloudflare drops idle sockets after ~100 s) keep the socket open.
	go func() {
		defer cancel()
		ping := time.NewTicker(20 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-c.Send:
				if !ok {
					conn.Close(websocket.StatusPolicyViolation, "slow client")
					return
				}
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				err := wsjson.Write(wctx, conn, m)
				wcancel()
				if err != nil {
					return
				}
			case <-ping.C:
				pctx, pcancel := context.WithTimeout(ctx, 15*time.Second)
				err := conn.Ping(pctx)
				pcancel()
				if err != nil {
					return
				}
			}
		}
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var m room.In
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		s.Room.Handle(c, m)
	}
}
