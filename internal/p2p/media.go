package p2p

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"watchfilms/internal/media"
	"watchfilms/internal/room"
)

const (
	chunkSize       = 32 * 1024 // 32 KB per SCTP packet (guaranteed <= 65535 byte WebRTC limit)
	maxRangeChunk   = 2 * 1024 * 1024 // 2 MB max chunk per Range request (prevents bufferbloat)
	sctpBufferLimit = 128 * 1024      // 128 KB max in-flight SCTP buffer for ultra-low latency control packets
	flagData  byte  = 0
	flagDone  byte  = 1
	flagError byte  = 2
)

// MediaRequest is received from the browser's Service Worker / WebRTC client.
type MediaRequest struct {
	ID    uint32 `json:"id"`
	Type  string `json:"type"` // "range" | "subs" | "cancel"
	Start int64  `json:"start,omitempty"`
	End   int64  `json:"end,omitempty"`
	N     int    `json:"n,omitempty"` // subtitle index
}

// MediaMeta is the response header sent before binary streaming.
type MediaMeta struct {
	ID     uint32 `json:"id"`
	Type   string `json:"type"` // "meta"
	Status int    `json:"status"`
	Start  int64  `json:"start"`
	End    int64  `json:"end"`
	Total  int64  `json:"total"`
	Mime   string `json:"mime"`
	Length int64  `json:"length"`
}

// MediaResponse is a JSON response for subtitles or errors.
type MediaResponse struct {
	ID      uint32 `json:"id"`
	Type    string `json:"type"`
	Status  int    `json:"status"`
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

// MediaHandler processes video range and subtitle requests over WebRTC.
type MediaHandler struct {
	room       *room.Room
	dc         *webrtc.DataChannel
	mu         sync.Mutex
	cancels    map[uint32]context.CancelFunc
	closed     bool
}

// NewMediaHandler creates a handler bound to a WebRTC media DataChannel.
func NewMediaHandler(rm *room.Room, dc *webrtc.DataChannel) *MediaHandler {
	h := &MediaHandler{
		room:    rm,
		dc:      dc,
		cancels: make(map[uint32]context.CancelFunc),
	}
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if !msg.IsString {
			return
		}
		var req MediaRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			return
		}
		h.handleRequest(req)
	})
	dc.OnClose(func() {
		h.Close()
	})
	return h
}

func (h *MediaHandler) handleRequest(req MediaRequest) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}

	log.Printf("[p2p] запрос медиа: id=%d тип=%s старт=%d конец=%d", req.ID, req.Type, req.Start, req.End)

	if req.Type == "cancel" {
		if cancel, ok := h.cancels[req.ID]; ok {
			cancel()
			delete(h.cancels, req.ID)
		}
		h.mu.Unlock()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.cancels[req.ID] = cancel
	h.mu.Unlock()

	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.cancels, req.ID)
			h.mu.Unlock()
		}()

		switch req.Type {
		case "range", "blob":
			h.serveRange(ctx, req)
		case "subs":
			h.serveSubs(ctx, req)
		}
	}()
}

func (h *MediaHandler) serveRange(ctx context.Context, req MediaRequest) {
	m := h.room.Media()
	if m == nil || m.Path == "" {
		h.sendJSON(MediaResponse{ID: req.ID, Type: "error", Status: 404, Error: "файл фильма не выбран"})
		return
	}

	f, err := os.Open(m.Path)
	if err != nil {
		h.sendJSON(MediaResponse{ID: req.ID, Type: "error", Status: 500, Error: err.Error()})
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		h.sendJSON(MediaResponse{ID: req.ID, Type: "error", Status: 500, Error: err.Error()})
		return
	}

	size := st.Size()
	start := req.Start
	end := req.End

	if start < 0 {
		start = 0
	}
	if end <= 0 || end >= size {
		end = size - 1
	}
	if start > end {
		start = end
	}

	// For Range streaming: cap to maxRangeChunk (2 MB) so single requests don't choke the connection.
	// For Blob direct download (req.Type == "blob"): allow full file streaming.
	if req.Type != "blob" && end-start+1 > maxRangeChunk {
		end = start + maxRangeChunk - 1
	}
	length := end - start + 1

	meta := MediaMeta{
		ID:     req.ID,
		Type:   "meta",
		Status: 206,
		Start:  start,
		End:    end,
		Total:  size,
		Mime:   media.ContentType(m.Path),
		Length: length,
	}
	b, _ := json.Marshal(meta)
	if err := h.dc.SendText(string(b)); err != nil {
		log.Printf("[p2p] ошибка отправки meta: %v", err)
		return
	}

	log.Printf("[p2p] передача видео другу: %d байт (диапазон %d-%d из %d)", length, start, end, size)

	var offset int64
	buf := make([]byte, 5+chunkSize)
	binary.BigEndian.PutUint32(buf[0:4], req.ID)
	buf[4] = flagData

	for offset < length {
		select {
		case <-ctx.Done():
			log.Printf("[p2p] передача видео отменена (context done)")
			return
		default:
		}

		// Backpressure handling: wait if SCTP send buffer is high
		for h.dc.BufferedAmount() > sctpBufferLimit {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}

		toRead := int64(chunkSize)
		if length-offset < toRead {
			toRead = length - offset
		}

		n, err := f.ReadAt(buf[5:5+toRead], start+offset)
		if n > 0 {
			if err := h.dc.Send(buf[:5+n]); err != nil {
				log.Printf("[p2p] ошибка отправки видео чанка: %v", err)
				return
			}
			offset += int64(n)
		}
		if err != nil {
			break
		}
	}

	doneBuf := make([]byte, 5)
	binary.BigEndian.PutUint32(doneBuf[0:4], req.ID)
	doneBuf[4] = flagDone
	_ = h.dc.Send(doneBuf)
	log.Printf("[p2p] передача видео завершена: отправлено %d байт", offset)
}

func (h *MediaHandler) serveSubs(ctx context.Context, req MediaRequest) {
	m := h.room.Media()
	if m == nil || req.N < 0 || req.N >= len(m.Subtitles) {
		h.sendJSON(MediaResponse{ID: req.ID, Type: "subs", Status: 404, Error: "субтитры не найдены"})
		return
	}

	sub := m.Subtitles[req.N]
	raw, err := os.ReadFile(sub.Path)
	if err != nil {
		h.sendJSON(MediaResponse{ID: req.ID, Type: "subs", Status: 500, Error: err.Error()})
		return
	}

	vtt := media.ToVTT(raw)
	h.sendJSON(MediaResponse{
		ID:      req.ID,
		Type:    "subs",
		Status:  200,
		Content: string(vtt),
	})
}

func (h *MediaHandler) sendJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = h.dc.SendText(string(b))
}

// Close cancels all ongoing streams.
func (h *MediaHandler) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for _, cancel := range h.cancels {
		cancel()
	}
	h.cancels = nil
	log.Printf("[p2p] media handler closed")
}
