package pulls

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

// Wire conventions (07 §2, same as internal/api and internal/issues): JSON
// success, plain-text errors, arrays [] never null, RFC 3339 UTC,
// per-segment decoding, no-store or SWR+ETag per route, both lanes
// everywhere. Anonymous-denied reads get a real 401 with
// WWW-Authenticate: Bearer (never a 200 with an in-band error).

// Handler is the Seam 1 surface: every §8 endpoint on both lanes.
// Composition chains it in front of the core mux: Handle reports false for
// non-pulls paths so the core mux answers (the Wave A amendment in
// 14_extensibility.md Decisions — the `server.ExtraRoutes` chain, exactly
// like internal/identity and internal/issues).
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

// correlationID passes the request id through to WAL publish meta.
func correlationID(r *http.Request) string {
	if v := r.Header.Get("X-Request-ID"); v != "" {
		return v
	}
	return r.Header.Get("X-Walgit-Request-ID")
}

// Handle answers one request; false when the path is not a pulls route.
//
// splitPath always yields at least one segment (even "/" splits to [""]),
// so segs[0] is safe without a length check.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r)
	if segs[0] == "api" || segs[0] == "api-browser" {
		return h.handleTop(w, r, segs)
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

// ServeHTTP answers pulls routes and 404s otherwise (httptest surface).
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
		writePlain(w, http.StatusInternalServerError, "pulls: encode: "+err.Error())
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
		writePlain(w, http.StatusInternalServerError, "pulls: encode: "+err.Error())
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

// --- routing ------------------------------------------------------------------

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

// handleTop answers the top-level fork route (§8):
// POST /api/v1/repos/{owner}/{repo}/forks (+ /api-browser/v1 twin). It is
// only called with a valid lane (Handle routes on segs[0]).
func (h *Handler) handleTop(w http.ResponseWriter, r *http.Request, segs []string) bool {
	rest := segs[1:]
	// Strip the version segment (v1) when present.
	if len(rest) > 0 && rest[0] == "v1" {
		rest = rest[1:]
	}
	if len(rest) == 4 && rest[0] == "repos" && rest[3] == "forks" {
		owner, repo := rest[1], strings.TrimSuffix(rest[2], ".git")
		if _, err := git.ParseRepoId(owner + "/" + repo); err != nil {
			return false
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.fork(w, r, owner, repo)
		return true
	}
	return false
}

func (h *Handler) handleRepo(w http.ResponseWriter, r *http.Request, owner, repo string, rest []string) bool {
	if len(rest) == 0 || rest[0] != "pulls" {
		return false
	}
	return h.routePulls(w, r, owner, repo, rest[1:])
}

func (h *Handler) routePulls(w http.ResponseWriter, r *http.Request, owner, repo string, rest []string) bool {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			h.listPulls(w, r, owner, repo)
			return true
		case http.MethodPost:
			h.openPull(w, r, owner, repo)
			return true
		}
		methodNotAllowed(w, "GET", "POST")
		return true
	}
	num, err := parseNum(rest[0])
	if err != nil {
		writePlain(w, http.StatusNotFound, "unknown pull request")
		return true
	}
	rest = rest[1:]
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			h.getPull(w, r, owner, repo, num)
			return true
		case http.MethodPut:
			h.updatePull(w, r, owner, repo, num)
			return true
		}
		methodNotAllowed(w, "GET", "PUT")
		return true
	}
	switch rest[0] {
	case "diff":
		if len(rest) != 1 {
			return false
		}
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return true
		}
		h.getDiff(w, r, owner, repo, num)
		return true
	case "commits":
		if len(rest) != 1 {
			return false
		}
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return true
		}
		h.getCommits(w, r, owner, repo, num)
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
	case "merge":
		if len(rest) == 1 {
			if r.Method != http.MethodPost {
				methodNotAllowed(w, "POST")
				return true
			}
			h.merge(w, r, owner, repo, num)
			return true
		}
		if len(rest) == 2 && rest[1] == "task" {
			if r.Method != http.MethodGet {
				methodNotAllowed(w, "GET")
				return true
			}
			h.mergeTask(w, r, owner, repo, num)
			return true
		}
		return false
	case "update-branch":
		if len(rest) != 1 {
			return false
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.updateBranch(w, r, owner, repo, num)
		return true
	case "head":
		if len(rest) != 1 {
			return false
		}
		if r.Method != http.MethodDelete {
			methodNotAllowed(w, "DELETE")
			return true
		}
		h.deleteHead(w, r, owner, repo, num)
		return true
	}
	return false
}

// --- handlers -------------------------------------------------------------------

func (h *Handler) listPulls(w http.ResponseWriter, r *http.Request, owner, repo string) {
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
	f.Base = q.Get("base")
	f.Head = q.Get("head")
	if v := q.Get("sort"); v != "" {
		if v != "updated" && v != "created" {
			writePlain(w, http.StatusBadRequest, "invalid sort: must be updated|created")
			return
		}
		f.Sort = v
	}
	if v := q.Get("after"); v != "" {
		n, cerr := strconv.Atoi(v)
		if cerr != nil || n < 1 {
			writePlain(w, http.StatusBadRequest, "invalid after: must be a pull request number")
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
	res, err := h.Svc.ListPRs(r.Context(), owner, repo, p, f)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) openPull(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Title   string `json:"title"`
		BaseRef string `json:"base_ref"`
		HeadRef string `json:"head_ref"`
		Body    string `json:"body"`
		Fork    *struct {
			Repo string `json:"repo"`
		} `json:"fork"`
	}
	if !decodeStrict(w, r, 1<<20, map[string]bool{"title": true, "base_ref": true, "head_ref": true, "body": true, "fork": true}, &body) {
		return
	}
	in := OpenInput{Title: body.Title, BaseRef: body.BaseRef, HeadRef: body.HeadRef, Body: body.Body}
	if body.Fork != nil {
		in.Fork = &ForkInfo{Repo: body.Fork.Repo}
	}
	th, pr, err := h.Svc.OpenPR(r.Context(), owner, repo, p, in, correlationID(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"thread": th, "pr": pr})
}

func (h *Handler) getPull(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	q := r.URL.Query()
	afterSeq, n := 0, 50
	if v := q.Get("after_seq"); v != "" {
		sv, cerr := strconv.Atoi(v)
		if cerr != nil || sv < 0 {
			writePlain(w, http.StatusBadRequest, "invalid after_seq: must be a non-negative integer")
			return
		}
		afterSeq = sv
	}
	if v := q.Get("n"); v != "" {
		nv, cerr := strconv.Atoi(v)
		if cerr != nil || nv < 1 {
			writePlain(w, http.StatusBadRequest, "invalid n: must be a positive integer")
			return
		}
		if nv > 200 {
			nv = 200
		}
		n = nv
	}
	view, err := h.Svc.GetPR(r.Context(), owner, repo, num, p, afterSeq, n)
	if err != nil {
		writeErr(w, err)
		return
	}
	// SWR + ETag <head sha> (§8): the head sha is the ref-class stamp.
	writeCached(w, r, ccSWR, view.HeadLive, http.StatusOK, view)
}

func (h *Handler) getDiff(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	patch, err := h.Svc.Diff(r.Context(), owner, repo, num, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	hdr.Set("Cache-Control", ccSWR)
	hdr.Set("Content-Length", strconv.Itoa(len(patch)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(patch))
}

func (h *Handler) getCommits(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	q := r.URL.Query()
	skip, n := 0, 50
	if v := q.Get("skip"); v != "" {
		sv, cerr := strconv.Atoi(v)
		if cerr != nil || sv < 0 {
			writePlain(w, http.StatusBadRequest, "invalid skip: must be a non-negative integer")
			return
		}
		skip = sv
	}
	if v := q.Get("n"); v != "" {
		nv, cerr := strconv.Atoi(v)
		if cerr != nil || nv < 1 {
			writePlain(w, http.StatusBadRequest, "invalid n: must be a positive integer")
			return
		}
		if nv > 200 {
			nv = 200
		}
		n = nv
	}
	commits, more, err := h.Svc.Commits(r.Context(), owner, repo, num, p, skip, n)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"commits": commits, "more": more})
}

func (h *Handler) updatePull(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
		State *string `json:"state"`
	}
	if !decodeStrict(w, r, 1<<20, map[string]bool{"title": true, "body": true, "state": true}, &body) {
		return
	}
	th, pr, err := h.Svc.UpdatePR(r.Context(), owner, repo, num, p, PRPatch{Title: body.Title, Body: body.Body, State: body.State})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"thread": th, "pr": pr})
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

func (h *Handler) merge(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Strategy      string `json:"strategy"`
		CommitTitle   string `json:"commit_title"`
		CommitMessage string `json:"commit_message"`
		DeleteHead    bool   `json:"delete_head"`
	}
	if !decodeStrict(w, r, 1<<20, map[string]bool{"strategy": true, "commit_title": true, "commit_message": true, "delete_head": true}, &body) {
		return
	}
	rec, err := h.Svc.StartMerge(r.Context(), owner, repo, num, p, MergeInput{
		Strategy: body.Strategy, CommitTitle: body.CommitTitle,
		CommitMessage: body.CommitMessage, DeleteHead: body.DeleteHead,
	}, correlationID(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	// 202 + task attach shape (§8: SSE task attach; no-store on task starts).
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", ccNoStore)
	b, _ := json.Marshal(map[string]any{"task": rec})
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(b)
}

// mergeTask answers the SSE-attach poll for the running pull-merge task
// (the record carries progress packets + the terminal outcome; 404 when no
// merge is running for the repo).
func (h *Handler) mergeTask(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := h.Svc.requireRead(r.Context(), owner, repo, p); err != nil {
		writeErr(w, err)
		return
	}
	rec := h.Svc.MergeTask(owner, repo)
	if rec == nil || rec.Num != num {
		writePlain(w, http.StatusNotFound, "no merge task running")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": rec})
}

func (h *Handler) updateBranch(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		ExpectedHeadSHA string `json:"expected_head_sha"`
	}
	if !decodeStrict(w, r, 1<<20, map[string]bool{"expected_head_sha": true}, &body) {
		return
	}
	rec, err := h.Svc.UpdateBranch(r.Context(), owner, repo, num, p, body.ExpectedHeadSHA)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", ccNoStore)
	b, _ := json.Marshal(map[string]any{"task": rec})
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(b)
}

func (h *Handler) deleteHead(w http.ResponseWriter, r *http.Request, owner, repo string, num int) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := h.Svc.DeleteHead(r.Context(), owner, repo, num, p); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) fork(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		TargetOwner string `json:"target_owner"`
		Name        string `json:"name"`
	}
	if !decodeStrict(w, r, 1<<20, map[string]bool{"target_owner": true, "name": true}, &body) {
		return
	}
	rec, child, err := h.Svc.StartFork(r.Context(), owner, repo, p, ForkInput{TargetOwner: body.TargetOwner, Name: body.Name})
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", ccNoStore)
	b, _ := json.Marshal(map[string]any{"task": rec, "repo": child})
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(b)
}
