package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// SSE is the §9.3 envelope writer (07_api.md §6, stdlib only): headers first,
// the ": walgit" opener flushed immediately, "event:"/"data:" packets, a
// ": keepalive" comment every 10 s while idle, and exactly one terminal
// packet (result|error). Packet writes and keepalives share one mutex so they
// cannot tear; every write carries a 15 s deadline so a stalled client cannot
// pin the goroutine. Work continues after client disconnect — cancellation
// stops only the writing.
type SSE struct {
	w     http.ResponseWriter
	fl    http.Flusher
	rc    *http.ResponseController
	ctx   context.Context
	ka    *time.Ticker
	mu    sync.Mutex
	ended bool
}

// NewSSE starts the envelope: headers, opener, keepalive ticker. ok=false
// when the ResponseWriter cannot flush (caller falls back to plain JSON).
func NewSSE(w http.ResponseWriter, r *http.Request) (*SSE, bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": walgit\n\n")
	fl.Flush()
	s := &SSE{
		w:   w,
		fl:  fl,
		rc:  http.NewResponseController(w),
		ctx: r.Context(),
	}
	s.ka = time.NewTicker(10 * time.Second)
	go func() {
		for range s.ka.C {
			if !s.comment(": keepalive") {
				s.ka.Stop()
				return
			}
		}
	}()
	return s, true
}

// Event writes one packet. Returns false when the client is gone or a
// terminal packet was already sent (terminal-once).
func (s *SSE) Event(name, dataJSON string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return false
	}
	if s.ctx.Err() != nil {
		s.ka.Stop()
		return false
	}
	if s.write("event: "+name+"\ndata: "+dataJSON+"\n\n") != nil {
		s.ka.Stop()
		return false
	}
	if name == "result" || name == "error" {
		s.ended = true
		s.ka.Stop()
	}
	return true
}

// Send marshals v and writes it as one packet.
func (s *SSE) Send(name string, v any) bool {
	return s.Event(name, mustJSON(v))
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"encode"}`
	}
	return string(b)
}

// comment writes a keepalive-style comment line.
func (s *SSE) comment(c string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return false
	}
	select {
	case <-s.ctx.Done():
		return false
	default:
	}
	return s.write(c+"\n\n") == nil
}

// write emits one chunk under a 15 s deadline and flushes.
func (s *SSE) write(p string) error {
	_ = s.rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
	_, err := io.WriteString(s.w, p)
	if err == nil {
		s.fl.Flush()
	}
	return err
}

// Close stops the keepalive ticker; callers defer it (hazard 3: the keepalive
// goroutine must not outlive the request).
func (s *SSE) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ka.Stop()
}

// packetJSON renders one Progress as its §9.3 envelope data (the event name
// is Kind; the JSON shape depends on the kind).
func packetJSON(p Progress) string {
	switch p.Kind {
	case "notice":
		return mustJSON(struct {
			Text string `json:"text"`
		}{Text: p.Text})
	case "task":
		if p.Task != nil {
			return mustJSON(p.Task)
		}
		return mustJSON(struct{}{})
	default: // "progress"
		return mustJSON(p)
	}
}

// pump streams a TaskStream over the envelope: the current task record as a
// `task` packet, the replay buffer, then live packets, then the terminal
// result/error. Returns when the stream ends or the client goes away.
func (s *SSE) pump(st TaskStream) {
	s.Send("task", st.Record)
	for _, p := range st.Replay {
		if !s.Event(p.Kind, packetJSON(p)) {
			return
		}
	}
	if st.Other != nil {
		// §12.2/§14: a same-(repo,kind) task running on another host is not
		// joinable — the record, then the terminal 409 error.
		s.Send("task", *st.Other)
		s.Event("error", mustJSON(sseError{Status: 409, Message: "task runs on " + st.Other.Hostname}))
		return
	}
	for p := range st.Updates {
		if !s.Event(p.Kind, packetJSON(p)) {
			return
		}
	}
	done, ok := <-st.Done
	if !ok {
		return
	}
	if done.Err != nil {
		s.Event("error", mustJSON(sseError{Status: done.Err.Status, Message: done.Err.Message}))
		return
	}
	s.Send("result", resultEnvelope{Task: done.Record, Value: done.Value})
}

type sseError struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
}

type resultEnvelope struct {
	Task  TaskRecord `json:"task"`
	Value string     `json:"value"`
}

// newID is a random UUID-shaped id for task records.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := make([]byte, 36)
	hex.Encode(h, b[:4])
	h[8] = '-'
	hex.Encode(h[9:], b[4:6])
	h[14] = '-'
	hex.Encode(h[15:], b[6:8])
	h[18] = '-'
	hex.Encode(h[19:], b[8:10])
	h[23] = '-'
	hex.Encode(h[24:], b[10:16])
	return string(h)
}

// --- ref-list dialect (07_api.md §7) --------------------------------------------

// refStream is the older, byte-compatible ref-list dialect: `event: ref` per
// match, terminal `event: done` with {"more":<bool>}. Written unbuffered
// (flush after every packet), never compressed, no ": walgit" opener and no
// keepalives — this dialect predates the envelope.
type refStream struct {
	w  http.ResponseWriter
	fl http.Flusher
	rc *http.ResponseController
}

func newRefStream(w http.ResponseWriter) (*refStream, bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	return &refStream{w: w, fl: fl, rc: http.NewResponseController(w)}, true
}

func (s *refStream) packet(event, data string) bool {
	_ = s.rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
	_, err := io.WriteString(s.w, "event: "+event+"\ndata: "+data+"\n\n")
	if err == nil {
		s.fl.Flush()
	}
	return err == nil
}
