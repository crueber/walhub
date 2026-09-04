// http.go — the Seam 1 surface (server.ExtraRoutes, both lanes): the
// top-level twins POST /api/v1/repos/imports and GET
// /api/v1/repos/imports/{id} (+ /api-browser/v1 twins).
//
// Wire conventions (07 §2, same as internal/api and internal/pulls): JSON
// success, plain-text errors, arrays [] never null, RFC 3339 UTC,
// per-segment decoding, no-store on task starts, both lanes everywhere.
// Anonymous-denied reads get a real 401 with WWW-Authenticate: Bearer
// (never a 200 with an in-band error). SSE attach speaks the frozen
// notice|progress|task|result|error envelope (07 §9.3/§12.4:
// replay-then-live, terminal exactly once).
package repoimport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/wal"
)

// ExposedTemplates lists the discovery endpoints[] entries this surface
// serves (14 §14.12 lane rule) — registered from composition via
// api.RegisterExposed in the same change (law 12).
var ExposedTemplates = []string{
	"/api/v1/repos/imports",
	"/api/v1/repos/imports/{id}",
}

// Handler is the Seam 1 surface. Composition chains it in front of the
// core mux: Handle reports false for non-import paths so the core mux
// answers (the server.ExtraRoutes chain, exactly like internal/pulls).
type Handler struct {
	Svc  *Service
	Auth Authenticator
}

// Authenticator resolves the request principal through Seam 2 (the
// server's AuthService, injected by composition). Nil falls back to
// anonymous.
type Authenticator func(r *http.Request) (auth.Principal, *auth.AuthError)

// principal resolves the request principal (nil Authenticator →
// anonymous; production always injects the server chain).
func (h *Handler) principal(r *http.Request) (auth.Principal, *auth.AuthError) {
	if h.Auth != nil {
		return h.Auth(r)
	}
	return auth.Anonymous(), nil
}

// Handle answers one request; false when the path is not an import route.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r)
	if len(segs) < 4 || (segs[0] != "api" && segs[0] != "api-browser") {
		return false
	}
	if segs[1] != "v1" || segs[2] != "repos" || segs[3] != "imports" {
		return false
	}
	switch {
	case len(segs) == 4 && r.Method == http.MethodPost:
		h.post(w, r)
		return true
	case len(segs) == 5 && segs[4] != "" && r.Method == http.MethodGet:
		h.get(w, r, segs[4])
		return true
	case len(segs) == 4 || len(segs) == 5:
		writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
		return true
	default:
		return false
	}
}

// ServeHTTP answers import routes and 404s otherwise (httptest surface).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.Handle(w, r) {
		writePlain(w, http.StatusNotFound, "not found")
	}
}

func splitPath(r *http.Request) []string {
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		if d, err := url.PathUnescape(s); err == nil {
			out = append(out, d)
		} else {
			out = append(out, s)
		}
	}
	return out
}

// --- POST /repos/imports ---------------------------------------------------------------

// post starts (or joins, or no-ops) an import: 202 {task, target} on
// start/join, 200 {repo, import} on the idempotent no-op.
func (h *Handler) post(w http.ResponseWriter, r *http.Request) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeAuthErr(w, aerr)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writePlain(w, http.StatusBadRequest, "unreadable body")
		return
	}
	if h.Svc == nil {
		writePlain(w, http.StatusServiceUnavailable, "import service not configured")
		return
	}
	params, token, perr := ParseRequest(body, h.svcCfg())
	if perr != nil {
		writeStatusErr(w, perr)
		return
	}
	res, rec, berr := h.Svc.Begin(r.Context(), p, params, token)
	if berr != nil {
		writeStatusErr(w, berr)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if res.NoOp != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"repo":   params.target(),
			"import": noOpDoc(res.NoOp),
		})
		return
	}
	task := map[string]any{"id": res.TaskID}
	if rec != nil {
		task = taskJSON(*rec)
	}
	out := map[string]any{"task": task, "target": params.target()}
	if res.Joined {
		out["joined"] = true
	}
	writeJSON(w, http.StatusAccepted, out)
}

// svcCfg returns the service config (nil-safe; ParseRequest falls back
// to compiled-in defaults on nil).
func (h *Handler) svcCfg() *config.Config {
	if h.Svc == nil {
		return nil
	}
	return h.Svc.cfg
}

// --- GET /repos/imports/{id} -------------------------------------------------------------

// get serves the task record as JSON, or attaches the SSE stream
// (replay-then-live, terminal result|error exactly once) on
// Accept: text/event-stream. Unknown ids are 404 on this instance.
func (h *Handler) get(w http.ResponseWriter, r *http.Request, id string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeAuthErr(w, aerr)
		return
	}
	if h.Svc == nil {
		writePlain(w, http.StatusServiceUnavailable, "import service not configured")
		return
	}
	st, rec, ok := h.Svc.Lookup(id)
	if !ok {
		writePlain(w, http.StatusNotFound, "unknown import: "+id)
		return
	}
	owner, repo := namespaceOf(st, rec)
	if gerr := h.Svc.checkRead(r.Context(), p, owner, repo); gerr != nil {
		writeStatusErr(w, gerr)
		return
	}
	if acceptsSSE(r) {
		h.attach(w, r, id, st, rec)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if rec == nil {
		writeJSON(w, http.StatusOK, map[string]any{"id": id})
		return
	}
	writeJSON(w, http.StatusOK, taskJSON(*rec))
}

// namespaceOf resolves the (owner, repo) pair gating a task-status read:
// the ring carries the target; pruned-but-remembered table records carry
// it as rec.Repo ("owner/name").
func namespaceOf(st *stream, rec *wal.TaskRecord) (string, string) {
	if st != nil && st.target != "" {
		if o, n, ok := strings.Cut(st.target, "/"); ok {
			return o, n
		}
	}
	if rec != nil {
		if o, n, ok := strings.Cut(rec.Repo, "/"); ok {
			return o, n
		}
	}
	return "", ""
}

func acceptsSSE(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream")
}

// attach streams replay → live → terminal for id (st != nil here: pruned
// records serve JSON only — same rule as 07 §12.4 attach semantics).
func (h *Handler) attach(w http.ResponseWriter, r *http.Request, id string, st *stream, rec *wal.TaskRecord) {
	if st == nil {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, taskJSON(*rec))
		return
	}
	s, ok := newSSE(w, r)
	if !ok {
		w.Header().Set("Cache-Control", "no-store")
		if rec != nil {
			writeJSON(w, http.StatusOK, taskJSON(*rec))
		} else {
			writeJSON(w, http.StatusOK, map[string]any{"id": id})
		}
		return
	}
	defer s.close()
	subID, updates, replay := st.subscribe()
	defer st.unsubscribe(subID)
	for _, rp := range replay {
		if !s.packet(rp) {
			return
		}
	}
	// Watch done directly: the terminal may land with no further packet
	// in flight (only waiting on updates would miss it — the sender is
	// gone after finish).
	done := st.doneChan()
	_, outcome, finished := st.snapshot()
	if finished {
		s.terminal(outcome)
		return
	}
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			// Drain packets that arrived before done closed, then the
			// terminal exactly once.
			for {
				select {
				case rp := <-updates:
					if !s.packet(rp) {
						return
					}
				default:
					_, outcome, _ := st.snapshot()
					s.terminal(outcome)
					return
				}
			}
		case rp, ok := <-updates:
			if !ok {
				return
			}
			if !s.packet(rp) {
				return
			}
		}
	}
}

// --- wire shapes --------------------------------------------------------------------------

// taskJSON renders the frozen TaskRecord shape (07 §12.3): plain-text
// errors elsewhere, []-not-null here, RFC3339, full SHAs.
func taskJSON(t wal.TaskRecord) map[string]any {
	logTail := t.LogTail
	if logTail == nil {
		logTail = []string{}
	}
	out := map[string]any{
		"id": t.ID, "kind": t.Kind, "repo": t.Repo, "hostname": t.Hostname,
		"started": t.Started, "elapsed_ms": t.ElapsedMS,
		"summary": t.Summary, "log_tail": logTail,
	}
	if t.Finished != "" {
		out["finished"] = t.Finished
	}
	if t.OK != nil {
		out["ok"] = *t.OK
	}
	if t.Progress != nil {
		out["progress"] = progressJSON(*t.Progress)
	}
	if len(t.Params) > 0 {
		out["params"] = t.Params
	}
	return out
}

func progressJSON(p wal.Progress) map[string]any {
	out := map[string]any{"label": p.Label, "done": p.Done, "unit": p.Unit}
	if p.Text != "" {
		out["text"] = p.Text
	}
	if p.Total != nil {
		out["total"] = *p.Total
	}
	if p.Percent != nil {
		out["percent"] = *p.Percent
	}
	if p.Task != nil {
		out["task"] = taskJSON(*p.Task)
	}
	return out
}

// noOpDoc renders the idempotent 200 body (import.json verbatim,
// []-not-null enforced on read).
func noOpDoc(doc *ImportDoc) map[string]any {
	refs := doc.RequestedRefs
	if refs == nil {
		refs = []string{}
	}
	heads := doc.HeadSHAs
	if heads == nil {
		heads = map[string]string{}
	}
	return map[string]any{
		"version": doc.Version, "source_url": doc.SourceURL,
		"source_kind": doc.SourceKind, "requested_refs": refs,
		"imported_at": doc.ImportedAt, "head_shas": heads,
		"importer": doc.Importer, "format": doc.Format,
	}
}

// --- writers ----------------------------------------------------------------------------------

func writePlain(w http.ResponseWriter, status int, msg string) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	hdr.Del("ETag")
	if status == http.StatusUnauthorized {
		hdr.Set("WWW-Authenticate", `Bearer realm="walgit"`)
	}
	if status == http.StatusServiceUnavailable {
		hdr.Set("Retry-After", "15")
	}
	hdr.Set("Content-Length", strconv.Itoa(len(msg)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		writePlain(w, http.StatusInternalServerError, "encode error")
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json; charset=utf-8")
	hdr.Del("ETag")
	hdr.Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeStatusErr(w http.ResponseWriter, err error) {
	if se, ok := err.(*StatusError); ok {
		writePlain(w, se.Status, se.Message)
		return
	}
	writePlain(w, http.StatusInternalServerError, scrubError(err.Error()))
}

func writeAuthErr(w http.ResponseWriter, aerr *auth.AuthError) {
	switch aerr.Kind {
	case auth.ErrForbidden:
		writePlain(w, http.StatusForbidden, aerr.Why)
	case auth.ErrUnavailable:
		writePlain(w, http.StatusServiceUnavailable, aerr.Why)
	default:
		writePlain(w, http.StatusUnauthorized, aerr.Why)
	}
}

// --- SSE envelope (07 §9.3 shape, notify stream.go precedent) ---------------------------

type sseWriter struct {
	w     http.ResponseWriter
	fl    http.Flusher
	ctx   context.Context
	ka    *time.Ticker
	mu    sync.Mutex // packet writes and keepalives share one mutex so
	ended bool       // they cannot tear (the api.SSE rule)
}

func newSSE(w http.ResponseWriter, r *http.Request) (*sseWriter, bool) {
	return newSSEWithTicker(w, r, 10*time.Second)
}

// newSSEWithTicker starts the envelope with a caller-chosen keepalive
// interval (production: 10 s per 07 §9.3; tests use milliseconds).
func newSSEWithTicker(w http.ResponseWriter, r *http.Request, keepalive time.Duration) (*sseWriter, bool) {
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
	s := &sseWriter{w: w, fl: fl, ctx: r.Context()}
	s.ka = time.NewTicker(keepalive)
	go func() {
		// 13 §1: every goroutine exits via context. (A bare
		// `for range s.ka.C` would park forever after Stop — the
		// ticker channel is never closed — leaking one goroutine
		// per attach. Stops elsewhere stay: Stop is idempotent.)
		defer s.ka.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-s.ka.C:
				if !s.comment(": keepalive") {
					return
				}
			}
		}
	}()
	return s, true
}

func (s *sseWriter) close() { s.ka.Stop() }

func (s *sseWriter) comment(c string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return false
	}
	select {
	case <-s.ctx.Done():
		s.ka.Stop()
		return false
	default:
	}
	_, err := io.WriteString(s.w, c+"\n\n")
	if err == nil {
		s.fl.Flush()
	} else {
		s.ka.Stop()
	}
	return err == nil
}

// event writes one packet; result|error are terminal (exactly once).
func (s *sseWriter) event(name, data string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return false
	}
	select {
	case <-s.ctx.Done():
		s.ka.Stop()
		return false
	default:
	}
	if _, err := io.WriteString(s.w, "event: "+name+"\ndata: "+data+"\n\n"); err != nil {
		s.ka.Stop()
		return false
	}
	s.fl.Flush()
	if name == "result" || name == "error" {
		s.ended = true
		s.ka.Stop()
	}
	return true
}

// packet forwards one ring packet (notice|progress|task).
func (s *sseWriter) packet(rp wal.Progress) bool {
	switch rp.Kind {
	case "notice":
		raw, _ := json.Marshal(map[string]any{"text": rp.Text})
		return s.event("notice", string(raw))
	case "progress":
		return s.event("progress", mustJSON(progressJSON(rp)))
	case "task":
		if rp.Task == nil {
			return true
		}
		return s.event("task", mustJSON(taskJSON(*rp.Task)))
	default:
		return true
	}
}

// terminal emits the outcome exactly once (result value | error status).
func (s *sseWriter) terminal(o *Outcome) {
	if o == nil {
		s.event("error", `{"status":500,"message":"import outcome missing"}`)
		return
	}
	if o.Err != nil {
		raw, _ := json.Marshal(map[string]any{"status": o.Err.Status, "message": o.Err.Message})
		s.event("error", string(raw))
		return
	}
	heads := o.HeadSHAs
	if heads == nil {
		heads = map[string]string{}
	}
	raw, _ := json.Marshal(map[string]any{
		"repo": o.Repo, "source_url": o.SourceURL, "head_shas": heads,
		"format": o.Format, "imported_at": o.ImportedAt,
	})
	s.event("result", string(raw))
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return `{}`
	}
	return string(raw)
}
