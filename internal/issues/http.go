package issues

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

// Wire conventions (07 §2, same as internal/api and internal/identity):
// JSON success, plain-text errors, arrays [] never null, RFC 3339 UTC,
// per-segment decoding, no-store or SWR+ETag per route, both lanes
// everywhere. Anonymous-denied reads get a real 401 with
// WWW-Authenticate: Bearer (never a 200 with an in-band error).

// Handler is the Seam 1 surface: every §7 endpoint on both lanes.
// Composition chains it in front of the core api mux: Handle reports
// false for non-issues paths so the core mux answers. (Code-seam note:
// the doc's `api.Lanes`/`api.RouteProvider` phrasing resolves in this
// tree to the server.ExtraRoutes chain — see the Wave A amendment in
// 14_extensibility.md Decisions; this package follows the landed
// internal/identity pattern exactly.)
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

// Handle answers one request; false when the path is not an issues route.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r)
	if segs[0] == "api" || segs[0] == "api-browser" {
		return false // no top-level issues routes; all are repo-scoped
	}
	if len(segs) >= 4 && (segs[2] == "api" || segs[2] == "api-browser") {
		owner, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
		if _, err := git.ParseRepoId(owner + "/" + repo); err != nil {
			return false
		}
		return h.handleRepo(w, r, owner, repo, segs[3:])
	}
	return false
}

// ServeHTTP answers issues routes and 404s otherwise (httptest surface).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.Handle(w, r) {
		writePlain(w, http.StatusNotFound, "not found")
	}
}

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
// verbatim (fail closed downstream: it won't match a num or id shape).
func decodeSegment(s string) string {
	d, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return d
}

// --- writers ---------------------------------------------------------------

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
		writePlain(w, http.StatusInternalServerError, "issues: encode: "+err.Error())
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Cache-Control", ccNoStore)
	hdr.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeCached writes JSON with a cache class + ETag/304 path.
func writeCached(w http.ResponseWriter, r *http.Request, class, etag string, status int, v any) {
	hdr := w.Header()
	hdr.Set("Cache-Control", class)
	if etag != "" {
		hdr.Set("ETag", `"`+etag+`"`)
		if matchETag(r.Header.Get("If-None-Match"), etag) {
			hdr.Set("Content-Length", "0")
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		writePlain(w, http.StatusInternalServerError, "issues: encode: "+err.Error())
		return
	}
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func matchETag(header, etag string) bool {
	if header == "" {
		return false
	}
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for _, part := range strings.Split(header, ",") {
		p := strings.TrimSpace(part)
		p = strings.TrimPrefix(p, "W/")
		p = strings.Trim(p, `"`)
		if p == etag {
			return true
		}
	}
	return false
}

const (
	ccSWR     = "private, max-age=0, stale-while-revalidate=60"
	ccNoStore = "no-store"
)

// decodeStrict unmarshals body into v after rejecting unknown top-level
// keys (fail closed, same rule as policy effects: unknown keys on write
// are 400, on read ignored).
func decodeStrict(w http.ResponseWriter, r *http.Request, limit int64, allowed map[string]bool, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		writePlain(w, http.StatusBadRequest, "unreadable body")
		return false
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		writePlain(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	if keys == nil {
		writePlain(w, http.StatusBadRequest, "invalid JSON: expected an object")
		return false
	}
	for k := range keys {
		if !allowed[k] {
			writePlain(w, http.StatusBadRequest, "unknown field "+strconv.Quote(k))
			return false
		}
	}
	if err := json.Unmarshal(body, v); err != nil {
		writePlain(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func methodNotAllowed(w http.ResponseWriter, allow ...string) {
	w.Header().Set("Allow", strings.Join(allow, ", "))
	writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
}

// --- routing ------------------------------------------------------------------

var (
	numRe       = regexp.MustCompile(`^[1-9][0-9]{0,8}$`)
	milestoneRe = regexp.MustCompile(`^[0-9a-f]{6}$`)
)

// parseNum validates a wire issue number ([1-9][0-9]{0,8}, decimal;
// 06x storage-only). Numbers past the 06x range cannot exist → unknown
// issue (404), keeping the one error contract.
func parseNum(seg string) (int, error) {
	if !numRe.MatchString(seg) {
		return 0, ErrNotFound
	}
	n, _ := strconv.Atoi(seg)
	if n > 0xFFFFFF {
		return 0, ErrNotFound
	}
	return n, nil
}

func (h *Handler) handleRepo(w http.ResponseWriter, r *http.Request, owner, repo string, rest []string) bool {
	switch rest[0] {
	case "issues":
		return h.routeIssues(w, r, owner, repo, rest[1:])
	case "labels":
		return h.routeLabels(w, r, owner, repo, rest[1:])
	case "milestones":
		return h.routeMilestones(w, r, owner, repo, rest[1:])
	}
	return false
}

// --- issues ---------------------------------------------------------------------

func (h *Handler) routeIssues(w http.ResponseWriter, r *http.Request, owner, repo string, rest []string) bool {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			h.listIssues(w, r, owner, repo)
			return true
		case http.MethodPost:
			h.createIssue(w, r, owner, repo)
			return true
		}
		methodNotAllowed(w, "GET", "POST")
		return true
	}
	num, err := parseNum(rest[0])
	if err != nil {
		writePlain(w, http.StatusNotFound, "unknown issue")
		return true
	}
	rest = rest[1:]
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			h.getIssue(w, r, owner, repo, num)
			return true
		case http.MethodPatch:
			h.patchIssue(w, r, owner, repo, num)
			return true
		}
		methodNotAllowed(w, "GET", "PATCH")
		return true
	}
	switch rest[0] {
	case "events":
		if len(rest) != 1 {
			return false
		}
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return true
		}
		h.listEvents(w, r, owner, repo, num)
		return true
	case "comments":
		if len(rest) != 1 {
			return false
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.addComment(w, r, owner, repo, num)
		return true
	case "reactions":
		if len(rest) == 1 {
			if r.Method != http.MethodPost {
				methodNotAllowed(w, "POST")
				return true
			}
			h.addReaction(w, r, owner, repo, num)
			return true
		}
		if len(rest) == 3 {
			if r.Method != http.MethodDelete {
				methodNotAllowed(w, "DELETE")
				return true
			}
			h.removeReaction(w, r, owner, repo, num, rest[1], rest[2])
			return true
		}
		return false
	}
	return false
}

func (h *Handler) listIssues(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	q := r.URL.Query()
	f := ListFilter{}
	if v := q.Get("state"); v != "" {
		if v != StateOpen && v != StateClosed {
			writePlain(w, http.StatusBadRequest, "invalid state: must be open|closed")
			return
		}
		f.State = v
	}
	if v := q.Get("labels"); v != "" {
		for _, l := range strings.Split(v, ",") {
			l = strings.TrimSpace(l)
			if l != "" {
				f.Labels = append(f.Labels, l)
			}
		}
	}
	if v := q.Get("assignee"); v != "" {
		f.Assignee = v
	}
	if v := q.Get("milestone"); v != "" {
		f.Milestone = strings.ToLower(v)
	}
	if v := q.Get("since"); v != "" {
		f.Since = v
	}
	if v := q.Get("after"); v != "" {
		n, cerr := strconv.Atoi(v)
		if cerr != nil || n < 1 {
			writePlain(w, http.StatusBadRequest, "invalid after: must be an issue number")
			return
		}
		f.After = n
	}
	f.N = 50
	if v := q.Get("n"); v != "" {
		n, cerr := strconv.Atoi(v)
		if cerr != nil || n < 1 {
			writePlain(w, http.StatusBadRequest, "invalid n: must be a positive integer")
			return
		}
		if n > 100 {
			writePlain(w, http.StatusBadRequest, "invalid n: max 100")
			return
		}
		f.N = n
	}
	res, err := h.Svc.ListIssues(r.Context(), owner, repo, p, f)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) createIssue(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if !decodeStrict(w, r, 1<<20, map[string]bool{"title": true, "body": true}, &body) {
		return
	}
	th, ev, err := h.Svc.CreateIssue(r.Context(), owner, repo, p, body.Title, body.Body)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"thread": th, "events": []*Event{ev}})
}

func (h *Handler) seqWindow(r *http.Request) (afterSeq, n int, ok bool) {
	q := r.URL.Query()
	n = 50
	if v := q.Get("n"); v != "" {
		nv, cerr := strconv.Atoi(v)
		if cerr != nil || nv < 1 {
			return 0, 0, false
		}
		if nv > 200 {
			nv = 200
		}
		n = nv
	}
	if v := q.Get("after_seq"); v != "" {
		sv, cerr := strconv.Atoi(v)
		if cerr != nil || sv < 0 {
			return 0, 0, false
		}
		afterSeq = sv
	}
	return afterSeq, n, true
}

func (h *Handler) getIssue(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	afterSeq, n, ok := h.seqWindow(r)
	if !ok {
		writePlain(w, http.StatusBadRequest, "invalid after_seq/n")
		return
	}
	view, err := h.Svc.GetThread(r.Context(), owner, repo, num, p, afterSeq, n)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeCached(w, r, ccSWR, "v"+strconv.Itoa(view.Thread.Version), http.StatusOK, view)
}

func (h *Handler) listEvents(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	afterSeq, n, ok := h.seqWindow(r)
	if !ok {
		writePlain(w, http.StatusBadRequest, "invalid after_seq/n")
		return
	}
	view, err := h.Svc.GetThread(r.Context(), owner, repo, num, p, afterSeq, n)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": view.Events, "more": view.EventsMore})
}

func (h *Handler) patchIssue(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Title       *string         `json:"title"`
		State       *string         `json:"state"`
		StateReason *string         `json:"state_reason"`
		Labels      *[]string       `json:"labels"`
		Assignees   *[]string       `json:"assignees"`
		Milestone   json.RawMessage `json:"milestone"`
	}
	if !decodeStrict(w, r, 1<<20,
		map[string]bool{"title": true, "state": true, "state_reason": true, "labels": true, "assignees": true, "milestone": true},
		&body) {
		return
	}
	// Milestone rides as a value json.RawMessage (not **string):
	// encoding/json maps an explicit null onto a nil **string, which is
	// indistinguishable from an absent key — and absent must mean "no
	// change" while null means "clear" (02 §7, issue #119). A value
	// RawMessage stays nil when absent and holds "null" when clearing;
	// anything but null or a string is 400.
	var milestone **string
	if raw := body.Milestone; raw != nil {
		var want *string
		if string(raw) != "null" {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				writePlain(w, http.StatusBadRequest, "invalid milestone: must be an id string or null")
				return
			}
			want = &s
		}
		milestone = &want
	}
	th, err := h.Svc.PatchIssue(r.Context(), owner, repo, num, p, IssuePatch{
		Title: body.Title, State: body.State, StateReason: body.StateReason,
		Labels: body.Labels, Assignees: body.Assignees, Milestone: milestone,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"thread": th})
}

func (h *Handler) addComment(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if !decodeStrict(w, r, 1<<20, map[string]bool{"body": true}, &body) {
		return
	}
	ev, err := h.Svc.AddComment(r.Context(), owner, repo, num, p, body.Body)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"event": ev})
}

func (h *Handler) addReaction(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		TargetEventSeq *int   `json:"target_event_seq"`
		Content        string `json:"content"`
	}
	if !decodeStrict(w, r, 64<<10, map[string]bool{"target_event_seq": true, "content": true}, &body) {
		return
	}
	if body.TargetEventSeq == nil {
		writePlain(w, http.StatusBadRequest, "target_event_seq is required")
		return
	}
	th, ev, added, err := h.Svc.AddReaction(r.Context(), owner, repo, num, *body.TargetEventSeq, p, body.Content)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !added {
		// Duplicate add is a no-op returning the summary, not an event (§8).
		writeJSON(w, http.StatusOK, map[string]any{"summary": th.ReactionSummary})
		return
	}
	// ev is the committed event from the same two-step — no re-read, so a
	// concurrent interleaving event cannot misattribute the 201 body.
	writeJSON(w, http.StatusCreated, map[string]any{"event": ev})
}

func (h *Handler) removeReaction(w http.ResponseWriter, r *http.Request, owner, repo string, num int, seqSeg, content string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	seq, cerr := strconv.Atoi(seqSeg)
	if cerr != nil || seq < 0 {
		writePlain(w, http.StatusBadRequest, "invalid target_event_seq")
		return
	}
	if _, err := h.Svc.RemoveReaction(r.Context(), owner, repo, num, seq, p, content); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- labels ---------------------------------------------------------------------

func (h *Handler) routeLabels(w http.ResponseWriter, r *http.Request, owner, repo string, rest []string) bool {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			h.listLabels(w, r, owner, repo)
			return true
		case http.MethodPost:
			h.createLabel(w, r, owner, repo)
			return true
		}
		methodNotAllowed(w, "GET", "POST")
		return true
	}
	if len(rest) != 1 || rest[0] == "" {
		return false
	}
	switch r.Method {
	case http.MethodPatch:
		h.updateLabel(w, r, owner, repo, rest[0])
		return true
	case http.MethodDelete:
		h.deleteLabel(w, r, owner, repo, rest[0])
		return true
	}
	methodNotAllowed(w, "PATCH", "DELETE")
	return true
}

func (h *Handler) listLabels(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := h.Svc.requireRead(r.Context(), owner, repo, p); err != nil {
		writeErr(w, err)
		return
	}
	ls, _, err := h.Svc.loadLabels(r.Context(), owner, repo)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"labels": ls.Labels})
}

func (h *Handler) createLabel(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Name        string `json:"name"`
		Color       string `json:"color"`
		Description string `json:"description"`
	}
	if !decodeStrict(w, r, 64<<10, map[string]bool{"name": true, "color": true, "description": true}, &body) {
		return
	}
	l, err := h.Svc.CreateLabel(r.Context(), owner, repo, p, body.Name, body.Color, body.Description)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"label": l})
}

func (h *Handler) updateLabel(w http.ResponseWriter, r *http.Request, owner, repo, name string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Color       *string `json:"color"`
		Description *string `json:"description"`
	}
	if !decodeStrict(w, r, 64<<10, map[string]bool{"color": true, "description": true}, &body) {
		return
	}
	l, err := h.Svc.UpdateLabel(r.Context(), owner, repo, p, name, body.Color, body.Description)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"label": l})
}

func (h *Handler) deleteLabel(w http.ResponseWriter, r *http.Request, owner, repo, name string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	affected, err := h.Svc.DeleteLabel(r.Context(), owner, repo, p, name)
	if err != nil {
		writeErr(w, err)
		return
	}
	// §3.1 mandates the {"threads_affected": N} report, which a 204 cannot
	// carry — 200 with the report (documented in 02 Decisions).
	writeJSON(w, http.StatusOK, map[string]any{"threads_affected": affected})
}

// --- milestones -------------------------------------------------------------------

func (h *Handler) routeMilestones(w http.ResponseWriter, r *http.Request, owner, repo string, rest []string) bool {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			h.listMilestones(w, r, owner, repo)
			return true
		case http.MethodPost:
			h.createMilestone(w, r, owner, repo)
			return true
		}
		methodNotAllowed(w, "GET", "POST")
		return true
	}
	if len(rest) != 1 || !milestoneRe.MatchString(strings.ToLower(rest[0])) {
		return false
	}
	id := strings.ToLower(rest[0])
	switch r.Method {
	case http.MethodGet:
		h.getMilestone(w, r, owner, repo, id)
		return true
	case http.MethodPatch:
		h.updateMilestone(w, r, owner, repo, id)
		return true
	case http.MethodDelete:
		h.deleteMilestone(w, r, owner, repo, id)
		return true
	}
	methodNotAllowed(w, "GET", "PATCH", "DELETE")
	return true
}

func (h *Handler) listMilestones(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := h.Svc.requireRead(r.Context(), owner, repo, p); err != nil {
		writeErr(w, err)
		return
	}
	state := r.URL.Query().Get("state")
	if state != "" && state != StateOpen && state != StateClosed && state != "all" {
		writePlain(w, http.StatusBadRequest, "invalid state: must be open|closed|all")
		return
	}
	all, err := h.Svc.listMilestones(r.Context(), owner, repo)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]*Milestone, 0, len(all))
	for _, m := range all {
		if state == StateOpen && m.State != StateOpen {
			continue
		}
		if state == StateClosed && m.State != StateClosed {
			continue
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"milestones": out})
}

func (h *Handler) createMilestone(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Title       string  `json:"title"`
		Description string  `json:"description"`
		DueOn       *string `json:"due_on"`
	}
	if !decodeStrict(w, r, 64<<10, map[string]bool{"title": true, "description": true, "due_on": true}, &body) {
		return
	}
	m, err := h.Svc.CreateMilestone(r.Context(), owner, repo, p, body.Title, body.Description, body.DueOn)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"milestone": m})
}

func (h *Handler) getMilestone(w http.ResponseWriter, r *http.Request, owner, repo, id string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := h.Svc.requireRead(r.Context(), owner, repo, p); err != nil {
		writeErr(w, err)
		return
	}
	m, _, err := h.Svc.loadMilestone(r.Context(), owner, repo, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if m == nil {
		writePlain(w, http.StatusNotFound, "unknown milestone")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"milestone": m})
}

func (h *Handler) updateMilestone(w http.ResponseWriter, r *http.Request, owner, repo, id string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
		DueOn       *string `json:"due_on"`
		State       *string `json:"state"`
	}
	if !decodeStrict(w, r, 64<<10, map[string]bool{"title": true, "description": true, "due_on": true, "state": true}, &body) {
		return
	}
	m, err := h.Svc.UpdateMilestone(r.Context(), owner, repo, p, id, body.Title, body.Description, body.DueOn, body.State)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"milestone": m})
}

func (h *Handler) deleteMilestone(w http.ResponseWriter, r *http.Request, owner, repo, id string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := h.Svc.DeleteMilestone(r.Context(), owner, repo, p, id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
