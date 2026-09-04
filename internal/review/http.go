package review

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

// Wire conventions (07 §2, same as internal/api, internal/issues, and
// internal/pulls): JSON success, plain-text errors, arrays [] never null,
// RFC 3339 UTC, per-segment decoding, no-store on every route
// (ref-independent), both lanes everywhere. Anonymous-denied reads get a
// real 401 with WWW-Authenticate: Bearer (never a 200 with an in-band
// error).

// Handler is the Seam 1 surface: every §7 endpoint on both lanes.
// Composition chains it in front of the core mux: Handle reports false for
// non-review paths so the core mux answers (the Wave A amendment in
// 14_extensibility.md Decisions — the `server.ExtraRoutes` chain, exactly
// like internal/identity, internal/issues, and internal/pulls).
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

// Handle answers one request; false when the path is not a review route.
//
// splitPath always yields at least one segment (even "/" splits to [""]),
// so segs[0] is safe without a length check.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r)
	if len(segs) >= 4 && (segs[2] == "api" || segs[2] == "api-browser") {
		owner, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
		if _, err := git.ParseRepoId(owner + "/" + repo); err != nil {
			return false
		}
		return h.handleRepo(w, r, owner, repo, segs[3:])
	}
	return false
}

// ServeHTTP answers review routes and 404s otherwise (httptest surface).
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
// verbatim (fail closed downstream: it won't match a num or tid shape).
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
		writePlain(w, http.StatusInternalServerError, "review: encode: "+err.Error())
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// decodeStrict unmarshals body into v after rejecting unknown top-level
// keys (fail closed: unknown keys on write are 400, on read ignored).
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

// --- routing ---------------------------------------------------------------

var numRe = regexp.MustCompile(`^[1-9][0-9]{0,8}$`)

// parseNum validates a wire PR number ([1-9][0-9]{0,8}, decimal; 06x is
// storage-only). Numbers past the 06x range cannot exist → unknown PR (404).
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

// parseSeq validates a review seq segment (non-negative decimal).
func parseSeq(seg string) (int, error) {
	if seg == "" {
		return 0, ErrNotFound
	}
	for i := 0; i < len(seg); i++ {
		if seg[i] < '0' || seg[i] > '9' {
			return 0, ErrNotFound
		}
	}
	if len(seg) > 10 {
		return 0, ErrNotFound
	}
	n, _ := strconv.Atoi(seg)
	return n, nil
}

func (h *Handler) handleRepo(w http.ResponseWriter, r *http.Request, owner, repo string, rest []string) bool {
	if len(rest) == 0 || rest[0] != "pulls" {
		return false
	}
	return h.routePulls(w, r, owner, repo, rest[1:])
}

func (h *Handler) routePulls(w http.ResponseWriter, r *http.Request, owner, repo string, rest []string) bool {
	if len(rest) == 0 {
		return false // /pulls itself is 03's surface, not ours
	}
	num, err := parseNum(rest[0])
	if err != nil {
		writePlain(w, http.StatusNotFound, "unknown pull request")
		return true
	}
	rest = rest[1:]
	if len(rest) == 0 {
		return false
	}
	switch rest[0] {
	case "reviews":
		return h.routeReviews(w, r, owner, repo, num, rest[1:])
	case "threads":
		return h.routeThreads(w, r, owner, repo, num, rest[1:])
	case "review-requests":
		if len(rest[1:]) > 0 {
			writePlain(w, http.StatusNotFound, "not found")
			return true
		}
		switch r.Method {
		case http.MethodGet:
			h.getRequests(w, r, owner, repo, num)
		case http.MethodPost:
			h.addRequests(w, r, owner, repo, num)
		case http.MethodDelete:
			h.removeRequests(w, r, owner, repo, num)
		default:
			methodNotAllowed(w, "GET", "POST", "DELETE")
		}
		return true
	case "review-suggest":
		if len(rest[1:]) > 0 {
			writePlain(w, http.StatusNotFound, "not found")
			return true
		}
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return true
		}
		h.suggest(w, r, owner, repo, num)
		return true
	}
	return false
}

func (h *Handler) routeReviews(w http.ResponseWriter, r *http.Request, owner, repo string, num int, rest []string) bool {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			h.listReviews(w, r, owner, repo, num)
		case http.MethodPost:
			h.submitReview(w, r, owner, repo, num)
		default:
			methodNotAllowed(w, "GET", "POST")
		}
		return true
	}
	seq, err := parseSeq(rest[0])
	if err != nil {
		writePlain(w, http.StatusNotFound, "unknown review")
		return true
	}
	rest = rest[1:]
	if len(rest) == 0 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return true
		}
		h.getReview(w, r, owner, repo, num, seq)
		return true
	}
	if len(rest) == 1 && rest[0] == "dismiss" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.dismissReview(w, r, owner, repo, num, seq)
		return true
	}
	writePlain(w, http.StatusNotFound, "not found")
	return true
}

func (h *Handler) routeThreads(w http.ResponseWriter, r *http.Request, owner, repo string, num int, rest []string) bool {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			h.listThreads(w, r, owner, repo, num)
		case http.MethodPost:
			h.openThread(w, r, owner, repo, num)
		default:
			methodNotAllowed(w, "GET", "POST")
		}
		return true
	}
	tid := rest[0]
	rest = rest[1:]
	if len(rest) == 0 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return true
		}
		h.getThread(w, r, owner, repo, num, tid)
		return true
	}
	if len(rest) == 1 {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		switch rest[0] {
		case "comments":
			h.addThreadComment(w, r, owner, repo, num, tid)
		case "resolve":
			h.resolveThread(w, r, owner, repo, num, tid, true)
		case "unresolve":
			h.resolveThread(w, r, owner, repo, num, tid, false)
		default:
			writePlain(w, http.StatusNotFound, "not found")
		}
		return true
	}
	writePlain(w, http.StatusNotFound, "not found")
	return true
}

// --- reviews ---------------------------------------------------------------

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func (h *Handler) listReviews(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	res, err := h.Svc.ListReviews(r.Context(), owner, repo, num, p,
		queryInt(r, "after", 0), queryInt(r, "n", 50))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": nonNilReviews(res.Reviews), "more": res.More})
}

func nonNilReviews(in []*ReviewEvent) []*ReviewEvent {
	if in == nil {
		return []*ReviewEvent{}
	}
	return in
}

type submitThreadBody struct {
	Anchor Anchor `json:"anchor"`
	Body   string `json:"body"`
}

type submitReviewBody struct {
	State     string             `json:"state"`
	Body      string             `json:"body"`
	CommitSHA string             `json:"commit_sha"`
	Threads   []submitThreadBody `json:"threads"`
}

func (h *Handler) submitReview(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	var body submitReviewBody
	if !decodeStrict(w, r, 1<<20, map[string]bool{"state": true, "body": true, "commit_sha": true, "threads": true}, &body) {
		return
	}
	if body.Threads == nil {
		body.Threads = []submitThreadBody{}
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	in := SubmitInput{State: body.State, Body: body.Body, CommitSHA: body.CommitSHA}
	for _, t := range body.Threads {
		in.Threads = append(in.Threads, NewThread{Anchor: t.Anchor, Body: t.Body})
	}
	ev, threads, sum, err := h.Svc.SubmitReview(r.Context(), owner, repo, num, p, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"review": ev, "threads": nonNilThreads(threads), "summary": sum})
}

func (h *Handler) getReview(w http.ResponseWriter, r *http.Request, owner, repo string, num, seq int) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	ev, err := h.Svc.GetReview(r.Context(), owner, repo, num, seq, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"review": ev})
}

type dismissBody struct {
	Reason string `json:"reason"`
}

func (h *Handler) dismissReview(w http.ResponseWriter, r *http.Request, owner, repo string, num, seq int) {
	var body dismissBody
	if !decodeStrict(w, r, 64<<10, map[string]bool{"reason": true}, &body) {
		return
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	ev, sum, err := h.Svc.DismissReview(r.Context(), owner, repo, num, seq, p, body.Reason)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"review": ev, "summary": sum})
}

// --- threads ---------------------------------------------------------------

func nonNilThreads(in []*ThreadHeader) []*ThreadHeader {
	if in == nil {
		return []*ThreadHeader{}
	}
	return in
}

func nonNilComments(in []*ThreadComment) []*ThreadComment {
	if in == nil {
		return []*ThreadComment{}
	}
	return in
}

func (h *Handler) listThreads(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	var resolved *bool
	if v := r.URL.Query().Get("resolved"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writePlain(w, http.StatusBadRequest, "resolved must be true|false")
			return
		}
		resolved = &b
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	res, err := h.Svc.ListThreads(r.Context(), owner, repo, num, p, resolved,
		r.URL.Query().Get("after"), queryInt(r, "n", 50))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": nonNilThreads(res.Threads), "more": res.More})
}

type openThreadBody struct {
	Anchor Anchor `json:"anchor"`
	Body   string `json:"body"`
}

func (h *Handler) openThread(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	var body openThreadBody
	if !decodeStrict(w, r, 1<<20, map[string]bool{"anchor": true, "body": true}, &body) {
		return
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	th, err := h.Svc.OpenThread(r.Context(), owner, repo, num, p, body.Anchor, body.Body)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"thread": th})
}

func (h *Handler) getThread(w http.ResponseWriter, r *http.Request, owner, repo string, num int, tid string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	view, err := h.Svc.GetThread(r.Context(), owner, repo, num, tid, p,
		queryInt(r, "after", 0), queryInt(r, "n", 50))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"thread": view.Thread, "comments": nonNilComments(view.Comments), "more": view.More,
	})
}

type threadCommentBody struct {
	Body string `json:"body"`
}

func (h *Handler) addThreadComment(w http.ResponseWriter, r *http.Request, owner, repo string, num int, tid string) {
	var body threadCommentBody
	if !decodeStrict(w, r, 1<<20, map[string]bool{"body": true}, &body) {
		return
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	c, err := h.Svc.AddThreadComment(r.Context(), owner, repo, num, tid, p, body.Body)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"comment": c})
}

func (h *Handler) resolveThread(w http.ResponseWriter, r *http.Request, owner, repo string, num int, tid string, resolve bool) {
	// Resolve/unresolve take no body: drain defensively (bounded) so a
	// client-sent body cannot wedge the connection.
	_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, 64<<10))
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var th *ThreadHeader
	var err error
	if resolve {
		th, err = h.Svc.ResolveThread(r.Context(), owner, repo, num, tid, p)
	} else {
		th, err = h.Svc.UnresolveThread(r.Context(), owner, repo, num, tid, p)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"thread": th})
}

// --- review requests + suggest -----------------------------------------------

type reviewersBody struct {
	Reviewers []string `json:"reviewers"`
}

func (h *Handler) getRequests(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	reqs, err := h.Svc.GetRequests(r.Context(), owner, repo, num, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviewers": nonNilReviewers(reqs.Reviewers)})
}

func nonNilReviewers(in []RequestedReviewer) []RequestedReviewer {
	if in == nil {
		return []RequestedReviewer{}
	}
	return in
}

func (h *Handler) addRequests(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	var body reviewersBody
	if !decodeStrict(w, r, 64<<10, map[string]bool{"reviewers": true}, &body) {
		return
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	reqs, err := h.Svc.AddRequests(r.Context(), owner, repo, num, p, body.Reviewers)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviewers": nonNilReviewers(reqs.Reviewers)})
}

func (h *Handler) removeRequests(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	var body reviewersBody
	if !decodeStrict(w, r, 64<<10, map[string]bool{"reviewers": true}, &body) {
		return
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	reqs, err := h.Svc.RemoveRequests(r.Context(), owner, repo, num, p, body.Reviewers)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviewers": nonNilReviewers(reqs.Reviewers)})
}

func (h *Handler) suggest(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	out, err := h.Svc.Suggest(r.Context(), owner, repo, num, p, r.URL.Query().Get("q"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"suggestions": out})
}
