// http.go — the Seam 1 surface: every 06 §6 endpoint on both lanes.
// Composition chains it in front of the core api mux: Handle reports
// false for non-notify paths so the core mux answers (the landed
// server.ExtraRoutes pattern, per the Wave A amendment in
// 14_extensibility.md Decisions).
//
// Wire conventions (07 §2): JSON success, plain-text errors, arrays []
// never null, RFC 3339 UTC, per-segment decoding, no-store on
// user-private reads. Anonymous on authenticated routes gets a real 401
// with WWW-Authenticate: Bearer. Foreign notification ids are 404 (never
// 403, which would leak existence). Webhook secrets are never logged or
// returned (secret_set instead).
package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// Handler is the Seam 1 surface: top-level /api/v1/notifications* twins
// plus repo-scoped watch/webhook routes via both lanes.
type Handler struct {
	Svc  *Service
	Auth Authenticator
}

// principal resolves the request principal via the injected Authenticator
// (Seam 2); nil Authenticator falls back to anonymous (production always
// injects the server chain).
func (h *Handler) principal(r *http.Request) (auth.Principal, *auth.AuthError) {
	if h.Auth != nil {
		return h.Auth(r)
	}
	return auth.Anonymous(), nil
}

// Handle answers one request; false when the path is not a notify route.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r)
	if len(segs) >= 3 && (segs[0] == "api" || segs[0] == "api-browser") && segs[1] == "v1" && segs[2] == "notifications" {
		h.handleUser(w, r, segs[3:])
		return true
	}
	if len(segs) >= 4 && (segs[2] == "api" || segs[2] == "api-browser") {
		owner, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
		if _, err := git.ParseRepoId(owner + "/" + repo); err != nil {
			return false
		}
		if len(segs[3:]) >= 1 && (segs[3] == "watch" || segs[3] == "webhooks") {
			h.handleRepo(w, r, owner, repo, segs[3:])
			return true
		}
	}
	return false
}

// ServeHTTP answers notify routes and 404s otherwise (httptest surface).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.Handle(w, r) {
		writePlain(w, http.StatusNotFound, "not found")
	}
}

// --- user routes ---------------------------------------------------------------

// notifIDRe validates the 32-hex deterministic id (malformed → 404).
var notifIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (h *Handler) handleUser(w http.ResponseWriter, r *http.Request, rest []string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := requireAuth(p); err != nil {
		writePlain(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	who := normPrincipal(p.Name)
	switch {
	case len(rest) == 0 && r.Method == "GET":
		h.tray(w, r, who)
	case len(rest) == 1 && rest[0] == "unread_count" && r.Method == "GET":
		h.unreadCount(w, r, who)
	case len(rest) == 1 && rest[0] == "read_all" && r.Method == "POST":
		h.readAll(w, r, who)
	case len(rest) == 1 && rest[0] == "stream" && r.Method == "GET":
		h.stream(w, r, who)
	case len(rest) == 2 && notifIDRe.MatchString(rest[0]) && (rest[1] == "read" || rest[1] == "unread") && r.Method == "POST":
		h.flip(w, r, who, rest[0], rest[1] == "read")
	case len(rest) == 1 && r.Method == "GET":
		// Malformed id read — 404, never 403 (existence is not leaked).
		writePlain(w, http.StatusNotFound, "not found")
	default:
		writePlain(w, http.StatusNotFound, "not found")
	}
}

// tray serves GET /api/v1/notifications (index-first, LIST overflow,
// newest-first, page size n default 50 max 200).
func (h *Handler) tray(w http.ResponseWriter, r *http.Request, who string) {
	q := r.URL.Query()
	state := q.Get("state")
	if state != "" && state != StateRead && state != StateUnread {
		writePlain(w, http.StatusBadRequest, "bad state")
		return
	}
	n := TrayPageSize
	if raw := q.Get("n"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			writePlain(w, http.StatusBadRequest, "bad n")
			return
		}
		n = v
		if n > TrayMaxPage {
			n = TrayMaxPage
		}
	}
	after := q.Get("after")
	notifs, more := h.Svc.Tray(r.Context(), who, state, after, n)
	writeNoStore(w)
	writeJSON(w, http.StatusOK, map[string]any{"notifications": notifs, "more": more})
}

// unreadCount serves GET /api/v1/notifications/unread_count (O(1) index).
func (h *Handler) unreadCount(w http.ResponseWriter, r *http.Request, who string) {
	writeNoStore(w)
	writeJSON(w, http.StatusOK, map[string]any{"count": h.Svc.UnreadCount(r.Context(), who)})
}

// flip serves POST /api/v1/notifications/{id}/read|unread (CAS state flip).
func (h *Handler) flip(w http.ResponseWriter, r *http.Request, who, id string, read bool) {
	n, err := h.Svc.SetState(r.Context(), who, id, read)
	if err != nil {
		writePlain(w, statusFor(err), err.Error())
		return
	}
	writeNoStore(w)
	writeJSON(w, http.StatusOK, n)
}

// readAll serves POST /api/v1/notifications/read_all (index window).
func (h *Handler) readAll(w http.ResponseWriter, r *http.Request, who string) {
	updated := h.Svc.ReadAll(r.Context(), who)
	writeNoStore(w)
	writeJSON(w, http.StatusOK, map[string]any{"updated": updated})
}

// stream serves GET /api/v1/notifications/stream (§5.1: per-user SSE,
// `notification` frames until client cancel).
func (h *Handler) stream(w http.ResponseWriter, r *http.Request, who string) {
	s, ok := newSSEWriter(w, r)
	if !ok {
		writePlain(w, http.StatusNotAcceptable, "streaming unsupported")
		return
	}
	defer s.close()
	ch, unsub := h.Svc.ubus.subscribe(who)
	defer unsub()
	for {
		select {
		case <-r.Context().Done():
			return
		case n, ok := <-ch:
			if !ok {
				return
			}
			if !s.event("notification", string(encode(n))) {
				return
			}
		}
	}
}

// --- repo routes ------------------------------------------------------------------

func (h *Handler) handleRepo(w http.ResponseWriter, r *http.Request, owner, repo string, rest []string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if rest[0] == "watch" && len(rest) == 1 {
		h.watch(w, r, owner, repo, p)
		return
	}
	// Webhooks: admin only (P6).
	if err := requireAuth(p); err != nil {
		writePlain(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := h.Svc.requireRole(r.Context(), owner, repo, p, string(identity.RoleAdmin)); err != nil {
		writePlain(w, statusFor(err), err.Error())
		return
	}
	switch {
	case rest[0] == "webhooks" && len(rest) == 1 && r.Method == "GET":
		hooks, err := h.Svc.ListHooks(r.Context(), owner, repo)
		if err != nil {
			writePlain(w, statusFor(err), err.Error())
			return
		}
		wire := make([]any, 0, len(hooks))
		for _, hk := range hooks {
			wire = append(wire, stripSecret(hk))
		}
		writeJSON(w, http.StatusOK, map[string]any{"webhooks": wire})
	case rest[0] == "webhooks" && len(rest) == 1 && r.Method == "POST":
		var spec HookSpec
		if err := readJSON(r, &spec); err != nil {
			writePlain(w, http.StatusBadRequest, "bad request")
			return
		}
		hk, err := h.Svc.CreateHook(r.Context(), owner, repo, p.Name, spec)
		if err != nil {
			writePlain(w, statusFor(err), err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, stripSecret(hk))
	case rest[0] == "webhooks" && len(rest) == 2 && r.Method == "GET":
		hk := h.Svc.GetHook(r.Context(), owner, repo, rest[1])
		if hk == nil {
			writePlain(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, stripSecret(hk))
	case rest[0] == "webhooks" && len(rest) == 2 && r.Method == "PATCH":
		var spec HookSpec
		if err := readJSON(r, &spec); err != nil {
			writePlain(w, http.StatusBadRequest, "bad request")
			return
		}
		hk, err := h.Svc.PatchHook(r.Context(), owner, repo, rest[1], spec)
		if err != nil {
			writePlain(w, statusFor(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, stripSecret(hk))
	case rest[0] == "webhooks" && len(rest) == 2 && r.Method == "DELETE":
		if err := h.Svc.DeleteHook(r.Context(), owner, repo, rest[1]); err != nil {
			writePlain(w, statusFor(err), err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case rest[0] == "webhooks" && len(rest) == 3 && rest[2] == "ping" && r.Method == "POST":
		delivered, err := h.Svc.PingHook(r.Context(), owner, repo, rest[1], p.Name)
		if err != nil {
			writePlain(w, statusFor(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"delivery": delivered})
	case rest[0] == "webhooks" && len(rest) == 3 && rest[2] == "deliveries" && r.Method == "GET":
		if h.Svc.GetHook(r.Context(), owner, repo, rest[1]) == nil {
			writePlain(w, http.StatusNotFound, "not found")
			return
		}
		d := h.Svc.ReadDeliveries(r.Context(), owner, repo, rest[1])
		writeNoStore(w)
		writeJSON(w, http.StatusOK, d)
	default:
		writePlain(w, http.StatusNotFound, "not found")
	}
}

// watch serves PUT/DELETE/GET /{o}/{r}/api/watch (07 §5 shape; 06 writes
// until 07 lands — see Decisions). PUT/DELETE need auth + read gate;
// GET needs auth (self only).
func (h *Handler) watch(w http.ResponseWriter, r *http.Request, owner, repo string, p auth.Principal) {
	if err := requireAuth(p); err != nil {
		writePlain(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	who := normPrincipal(p.Name)
	switch r.Method {
	case "GET":
		writeNoStore(w)
		writeJSON(w, http.StatusOK, h.Svc.GetWatch(r.Context(), who, owner, repo))
	case "PUT", "DELETE":
		if err := h.Svc.requireRead(r.Context(), owner, repo, p); err != nil {
			writePlain(w, statusFor(err), err.Error())
			return
		}
		st, err := h.Svc.SetWatch(r.Context(), who, owner, repo, r.Method == "PUT")
		if err != nil {
			writePlain(w, statusFor(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"watching": st.Watching, "watchers": st.Watchers})
	default:
		writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// stripSecret renders the wire Hook (secret write-only → secret_set).
func stripSecret(h *Hook) map[string]any {
	events := h.Events
	if events == nil {
		events = []string{}
	}
	return map[string]any{
		"id": h.ID, "url": h.URL, "events": events, "active": h.Active,
		"insecure_tls": h.InsecureTLS, "secret_set": h.Secret != "",
		"created_by": h.CreatedBy, "created_at": h.CreatedAt, "updated_at": h.UpdatedAt,
	}
}

// --- service reads/writes used by handlers ------------------------------------------

// Tray returns the newest-first page (state-filtered, after-cursor) plus
// whether older entries exist. Index-first (P4); LIST overflow sorted by
// (at desc, id) — the deterministic id is content-hash, NOT time-ordered
// (see Decisions: the 06 §1.1 sort sentence is corrected there).
func (s *Service) Tray(ctx context.Context, who, state, after string, n int) ([]Notification, bool) {
	byID := map[string]Notification{}
	order := []string{}
	remember := func(n Notification) {
		if _, ok := byID[n.ID]; !ok {
			order = append(order, n.ID)
		}
		byID[n.ID] = n
	}
	if raw, _, err := s.getJSON(ctx, NotifIndexKey(who)); err == nil && raw != nil {
		var ix IndexDoc
		if err := json.Unmarshal(raw, &ix); err == nil {
			for _, en := range ix.Entries {
				remember(Notification{
					ID: en.ID, Repo: en.Repo, Num: en.Num, Kind: en.Kind,
					Reason: en.Reason, Title: en.Title, State: en.State, CreatedAt: en.At,
				})
			}
		}
	}
	// LIST overflow (P5: paginated collaboration read; cap 1000 objects).
	count := 0
	_ = s.Store.List(ctx, NotifPrefix(who), "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, "/index.json") || count >= 1000 {
			return nil
		}
		raw, _, err := s.getJSON(ctx, m.Key)
		if err != nil || raw == nil {
			return nil
		}
		var notif Notification
		if err := json.Unmarshal(raw, &notif); err != nil {
			return nil
		}
		count++
		remember(notif)
		return nil
	})
	all := make([]Notification, 0, len(order))
	for _, id := range order {
		all = append(all, byID[id])
	}
	sortNotifications(all)
	filtered := all[:0]
	seenAfter := after == ""
	for _, notif := range all {
		if !seenAfter {
			if notif.ID == after {
				seenAfter = true
			}
			continue
		}
		if state != "" && notif.State != state {
			continue
		}
		filtered = append(filtered, notif)
	}
	if !seenAfter && after != "" {
		filtered = all[:0] // unknown cursor → first page, state-filtered
		for _, notif := range all {
			if state != "" && notif.State != state {
				continue
			}
			filtered = append(filtered, notif)
		}
	}
	if len(filtered) <= n {
		return filtered, false
	}
	return filtered[:n], true
}

// sortNotifications orders newest-first by (CreatedAt desc, ID asc).
func sortNotifications(all []Notification) {
	for i := 1; i < len(all); i++ {
		for j := i; j > 0; j-- {
			a, b := all[j-1], all[j]
			if a.CreatedAt > b.CreatedAt || (a.CreatedAt == b.CreatedAt && a.ID < b.ID) {
				break
			}
			all[j-1], all[j] = all[j], all[j-1]
		}
	}
}

// UnreadCount reads the O(1) index count (0 when absent).
func (s *Service) UnreadCount(ctx context.Context, who string) int {
	raw, _, err := s.getJSON(ctx, NotifIndexKey(who))
	if err != nil || raw == nil {
		return 0
	}
	var ix IndexDoc
	if err := json.Unmarshal(raw, &ix); err != nil {
		return 0
	}
	return ix.UnreadCount
}

// SetState CAS-flips one notification's state (object + index entry,
// unread_count delta). Foreign ids are structurally absent → 404.
func (s *Service) SetState(ctx context.Context, who, id string, read bool) (*Notification, error) {
	want := StateRead
	if !read {
		want = StateUnread
	}
	var result *Notification
	_, err := s.casUpdate(ctx, NotifKey(who, id), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, ErrNotFound
		}
		var n Notification
		if err := json.Unmarshal(cur, &n); err != nil {
			return nil, false, ErrNotFound
		}
		if n.State == want {
			result = &n
			return nil, false, nil
		}
		n.State = want
		result = &n
		return encode(n), true, nil
	})
	if err != nil {
		return nil, err
	}
	_ = s.indexFlip(ctx, who, id, want)
	return result, nil
}

// indexFlip mirrors a state flip into the index entry + count.
func (s *Service) indexFlip(ctx context.Context, who, id, want string) error {
	_, err := s.casUpdate(ctx, NotifIndexKey(who), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var ix IndexDoc
		if err := json.Unmarshal(cur, &ix); err != nil {
			return nil, false, nil
		}
		delta := 0
		found := false
		for i, en := range ix.Entries {
			if en.ID != id {
				continue
			}
			found = true
			if en.State == want {
				return nil, false, nil
			}
			if want == StateRead {
				delta = -1
			} else {
				delta = 1
			}
			ix.Entries[i].State = want
			break
		}
		if !found {
			return nil, false, nil
		}
		ix.UnreadCount += delta
		if ix.UnreadCount < 0 {
			ix.UnreadCount = 0
		}
		return encode(ix), true, nil
	})
	return err
}

// ReadAll marks the index window read (one index CAS + per-object flips
// bounded to the window). Returns the updated count.
func (s *Service) ReadAll(ctx context.Context, who string) int {
	raw, _, err := s.getJSON(ctx, NotifIndexKey(who))
	if err != nil || raw == nil {
		return 0
	}
	var ix IndexDoc
	if err := json.Unmarshal(raw, &ix); err != nil {
		return 0
	}
	updated := 0
	for _, en := range ix.Entries {
		if en.State != StateUnread {
			continue
		}
		if _, err := s.SetState(ctx, who, en.ID, true); err == nil {
			updated++
		}
	}
	return updated
}

// --- writers -------------------------------------------------------------------

// splitPath splits the escaped path and decodes each segment separately.
func splitPath(r *http.Request) []string {
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		out = append(out, decodeSegment(s))
	}
	return out
}

// decodeSegment decodes one path segment; an undecodable segment survives
// verbatim (fail closed downstream: it won't match a shape).
func decodeSegment(s string) string {
	d, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return d
}

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

func writeErr(w http.ResponseWriter, err error) {
	if aerr, ok := err.(*auth.AuthError); ok {
		switch aerr.Kind {
		case auth.ErrForbidden:
			writePlain(w, http.StatusForbidden, aerr.Why)
		case auth.ErrUnavailable:
			writePlain(w, http.StatusServiceUnavailable, aerr.Why)
		default:
			writePlain(w, http.StatusUnauthorized, aerr.Why)
		}
		return
	}
	writePlain(w, statusFor(err), err.Error())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writePlain(w, http.StatusInternalServerError, "encode error")
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

func readJSON(r *http.Request, v any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}
