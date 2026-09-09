// http.go — the Seam 1 surface (server.ExtraRoutes, both lanes): the
// repo-lane mirror config + Sync-now routes and the create-from-URL
// top-level twin.
//
// Wire conventions (07 §2, same as internal/api and internal/pulls):
// JSON success, plain-text errors, arrays [] never null, RFC 3339 UTC,
// per-segment decoding, no-store on task starts, both lanes
// everywhere. Anonymous-denied writes get a real 401 with
// WWW-Authenticate: Bearer (never a 200 with an in-band error).
//
// Authz (settings-PUT parity, R1 (c) audit): GET is open (the sidecar
// holds no secrets — public-only v1 — and the summary API renders the
// same view to readers); PUT/DELETE/POST-sync are admin-only.
// Create-from-URL requires write, plus the dangerous-confirm authority
// rule (admin under token/oidc) for off-allowlist hosts — the import
// dangerousAllowed shape.
package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/repoimport"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// ExposedTemplates lists the discovery endpoints[] entries this
// surface serves (14 §14.12 lane rule) — registered from composition
// via api.RegisterExposed in the same change (law 12). Only the
// top-level twin is exposed (repo-lane feature routes stay out of
// discovery, the 01/02/03/C2 rule).
var ExposedTemplates = []string{
	"/api/v1/repos/mirrors",
}

// Handler is the Seam 1 surface. Composition chains it in front of
// the core mux: Handle reports false for non-mirror paths so the core
// mux answers (the server.ExtraRoutes chain, exactly like
// internal/pulls and internal/repoimport).
type Handler struct {
	Svc  *Service
	Auth Authenticator

	// CreateRepo creates an empty repo for the create-from-URL flow
	// (composition binds it onto wal.Registry.Create). Nil → the
	// create twin answers 503.
	CreateRepo func(ctx context.Context, owner, name string) error
	// AuthMode reports server.auth.mode for the dangerous-confirm
	// authority rule (import dangerousAllowed shape).
	AuthMode func() string
	// Now is overridable for tests.
	Now func() time.Time
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

// now returns the handler clock (tests pin it; production is time.Now).
func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// Handle answers one request; false when the path is not a mirror route.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r)
	// Top-level twin: POST /api/v1/repos/mirrors (+ api-browser twin).
	if (segs[0] == "api" || segs[0] == "api-browser") &&
		len(segs) == 4 && segs[1] == "v1" && segs[2] == "repos" && segs[3] == "mirrors" {
		if r.Method == http.MethodPost {
			h.createFromURL(w, r)
			return true
		}
		writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
		return true
	}
	// Repo lanes: /{owner}/{repo}/(api|api-browser)/mirror[/sync].
	if len(segs) >= 4 && (segs[2] == "api" || segs[2] == "api-browser") {
		owner, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
		if _, err := git.ParseRepoId(owner + "/" + repo); err != nil {
			return false
		}
		rest := segs[3:]
		if len(rest) == 0 || rest[0] != "mirror" {
			return false
		}
		switch {
		case len(rest) == 1 && r.Method == http.MethodGet:
			h.get(w, r, owner, repo)
			return true
		case len(rest) == 1 && r.Method == http.MethodPut:
			h.put(w, r, owner, repo)
			return true
		case len(rest) == 1 && r.Method == http.MethodDelete:
			h.delete(w, r, owner, repo)
			return true
		case len(rest) == 2 && rest[1] == "sync" && r.Method == http.MethodPost:
			h.syncNow(w, r, owner, repo)
			return true
		case len(rest) == 2 && rest[1] == "sync" && r.Method == http.MethodGet:
			h.syncStatus(w, r, owner, repo)
			return true
		case len(rest) == 1 || len(rest) == 2:
			writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
			return true
		}
	}
	return false
}

// ServeHTTP answers mirror routes and 404s otherwise (httptest surface).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.Handle(w, r) {
		writePlain(w, http.StatusNotFound, "not found")
	}
}

func splitPath(r *http.Request) []string {
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		out = append(out, decodeSegment(s))
	}
	return out
}

// decodeSegment decodes one path segment; an undecodable segment
// survives verbatim (fail closed downstream: it won't match a repo id
// or route shape).
func decodeSegment(s string) string {
	if d, err := url.PathUnescape(s); err == nil {
		return d
	}
	return s
}

// --- GET /{o}/{r}/api/mirror -------------------------------------------------
//
// Open read (no secrets in the sidecar — public-only v1; the summary
// renders the same view). 404 when not a mirror.
func (h *Handler) get(w http.ResponseWriter, r *http.Request, owner, repo string) {
	if h.Svc == nil {
		writePlain(w, http.StatusServiceUnavailable, "mirror service not configured")
		return
	}
	doc, _, err := Load(r.Context(), h.Svc.store, owner, repo)
	if err != nil {
		writePlain(w, http.StatusInternalServerError, "mirror: "+scrubText(err.Error()))
		return
	}
	if doc == nil {
		writePlain(w, http.StatusNotFound, "not a mirror")
		return
	}
	writeJSON(w, http.StatusOK, ViewOf(doc, h.now()))
}

// --- PUT /{o}/{r}/api/mirror -------------------------------------------------
//
// Admin-only. No sidecar yet → create (upstream_url required,
// schedule defaults to daily) + anonymous first sync spawned (public
// upstream; a token-requiring upstream's first sync fails narrated —
// use Sync-now with a memory-only token). The repo must already exist
// (404 otherwise — PUT never creates repos, only the create twin
// does). Sidecar present → schedule
// update only (an upstream_url change is 409 delete-and-recreate —
// the anchor must not silently switch sources).
func (h *Handler) put(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeAuthErr(w, aerr)
		return
	}
	if !p.Admin {
		if p.Anonymous {
			writeAuthErr(w, &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"})
		} else {
			writePlain(w, http.StatusForbidden, "admin access required")
		}
		return
	}
	if h.Svc == nil {
		writePlain(w, http.StatusServiceUnavailable, "mirror service not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writePlain(w, http.StatusBadRequest, "unreadable body")
		return
	}
	var in struct {
		UpstreamURL string `json:"upstream_url"`
		Schedule    string `json:"schedule"`
	}
	if err := decodeStrict(body, &in); err != nil {
		writePlain(w, http.StatusBadRequest, err.Error())
		return
	}
	schedule := in.Schedule
	if schedule == "" {
		schedule = DefaultPreset
	}
	if !ValidPreset(schedule) {
		writePlain(w, http.StatusBadRequest, "unknown schedule preset "+strconv.Quote(schedule)+": want one of "+strings.Join(Presets, "|"))
		return
	}
	ctx := r.Context()
	doc, _, lerr := Load(ctx, h.Svc.store, owner, repo)
	if lerr != nil {
		writePlain(w, http.StatusInternalServerError, "mirror: "+scrubText(lerr.Error()))
		return
	}
	if doc != nil {
		if in.UpstreamURL != "" && in.UpstreamURL != doc.UpstreamURL {
			writePlain(w, http.StatusConflict, "mirror upstream_url is immutable; delete the mirror and recreate it to change sources")
			return
		}
		updated, uerr := SetSchedule(ctx, h.Svc.store, owner, repo, schedule)
		if uerr != nil {
			writePlain(w, http.StatusInternalServerError, "mirror: "+scrubText(uerr.Error()))
			return
		}
		writeJSON(w, http.StatusOK, ViewOf(updated, h.now()))
		return
	}
	if strings.TrimSpace(in.UpstreamURL) == "" {
		writePlain(w, http.StatusBadRequest, "upstream_url is required to create a mirror")
		return
	}
	n, nerr := repoimport.NormalizeSource(in.UpstreamURL)
	if nerr != nil {
		writeStatusErr(w, nerr)
		return
	}
	if verr := repoimport.ValidateTransport(n, false); verr != nil {
		writeStatusErr(w, verr)
		return
	}
	if serr := h.checkSSRF(n, false); serr != nil {
		writeStatusErr(w, serr)
		return
	}
	// PUT is config on a repo, never repo creation (the create twin
	// owns that). A sidecar on an unborn repo would 403 every future
	// push to the name — the guard probes the sidecar, not the
	// manifest — while every sync fails at Open. Fail fast with 404
	// instead of stranding the name.
	if meta, herr := h.Svc.store.Head(ctx, store.RepoPrefix(owner, repo)+store.Manifest); herr != nil && !store.IsNotFound(herr) {
		writePlain(w, http.StatusInternalServerError, "mirror: "+scrubText(herr.Error()))
		return
	} else if herr != nil || meta == nil {
		writePlain(w, http.StatusNotFound, "repository not found")
		return
	}
	if _, cerr := Create(ctx, h.Svc.store, owner, repo, n.URL, schedule); cerr != nil {
		if isExists(cerr) {
			writePlain(w, http.StatusConflict, "already a mirror")
			return
		}
		writePlain(w, http.StatusInternalServerError, "mirror: "+scrubText(cerr.Error()))
		return
	}
	// The first sync fires async (law 7 — a clone never blocks the
	// request); the 202 carries the id, GET .../mirror/sync?id=
	// resolves it, and the sidecar records the outcome.
	id := h.Svc.SyncAsync(ctx, owner, repo, "", false)
	w.Header().Set("Cache-Control", "no-store")
	doc, _, _ = Load(ctx, h.Svc.store, owner, repo)
	out := map[string]any{"target": owner + "/" + repo, "task": map[string]any{"id": id}}
	if doc != nil {
		out["mirror"] = ViewOf(doc, h.now())
	}
	writeJSON(w, http.StatusAccepted, out)
}

// --- DELETE /{o}/{r}/api/mirror ----------------------------------------------
//
// Admin-only. Deleting the sidecar stops the loop for the repo
// (probe-absent → skip — no orphaned syncs, no extra machinery).
func (h *Handler) delete(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeAuthErr(w, aerr)
		return
	}
	if !p.Admin {
		if p.Anonymous {
			writeAuthErr(w, &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"})
		} else {
			writePlain(w, http.StatusForbidden, "admin access required")
		}
		return
	}
	if h.Svc == nil {
		writePlain(w, http.StatusServiceUnavailable, "mirror service not configured")
		return
	}
	doc, _, lerr := Load(r.Context(), h.Svc.store, owner, repo)
	if lerr != nil {
		writePlain(w, http.StatusInternalServerError, "mirror: "+scrubText(lerr.Error()))
		return
	}
	if doc == nil {
		writePlain(w, http.StatusNotFound, "not a mirror")
		return
	}
	if derr := Delete(r.Context(), h.Svc.store, owner, repo); derr != nil {
		writePlain(w, http.StatusInternalServerError, "mirror: "+scrubText(derr.Error()))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- POST /{o}/{r}/api/mirror/sync -------------------------------------------
//
// Admin-only manual "Sync now" (202, join-or-run by
// (repo,mirror-sync)). The POST body MAY carry a memory-only token
// (never persisted, never logged — import S2 scrub rules) for
// first-contact with a token-requiring upstream, plus the force
// rewind escape hatch.
func (h *Handler) syncNow(w http.ResponseWriter, r *http.Request, owner, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeAuthErr(w, aerr)
		return
	}
	if !p.Admin {
		if p.Anonymous {
			writeAuthErr(w, &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"})
		} else {
			writePlain(w, http.StatusForbidden, "admin access required")
		}
		return
	}
	if h.Svc == nil {
		writePlain(w, http.StatusServiceUnavailable, "mirror service not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writePlain(w, http.StatusBadRequest, "unreadable body")
		return
	}
	var in struct {
		Token string `json:"token"`
		Force bool   `json:"force"`
	}
	if len(body) > 0 {
		if derr := decodeStrict(body, &in); derr != nil {
			writePlain(w, http.StatusBadRequest, derr.Error())
			return
		}
	}
	doc, _, lerr := Load(r.Context(), h.Svc.store, owner, repo)
	if lerr != nil {
		writePlain(w, http.StatusInternalServerError, "mirror: "+scrubText(lerr.Error()))
		return
	}
	if doc == nil {
		writePlain(w, http.StatusNotFound, "not a mirror")
		return
	}
	id := h.Svc.SyncAsync(r.Context(), owner, repo, in.Token, in.Force)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusAccepted, map[string]any{"task": map[string]any{"id": id}, "target": owner + "/" + repo})
}

// --- GET /{o}/{r}/api/mirror/sync --------------------------------------------
//
// Resolves a manual-sync 202: ?id=<async-id> → {task, done, outcome};
// without id → {active:[...], recent:[...]} (recent = finished
// mirror-sync table records, newest last). Open read, like GET mirror.
func (h *Handler) syncStatus(w http.ResponseWriter, r *http.Request, owner, repo string) {
	if h.Svc == nil {
		writePlain(w, http.StatusServiceUnavailable, "mirror service not configured")
		return
	}
	target := owner + "/" + repo
	if id := r.URL.Query().Get("id"); id != "" {
		a, ok := h.Svc.SyncStatus(id)
		if !ok {
			writePlain(w, http.StatusNotFound, "unknown sync: "+id)
			return
		}
		select {
		case <-a.done:
			out := map[string]any{"id": a.id, "target": a.target, "done": true}
			if a.rec != nil {
				out["task"] = taskJSON(a.rec)
			}
			if a.err != nil {
				out["error"] = scrubText(a.err.Error())
			}
			writeJSON(w, http.StatusOK, out)
		default:
			writeJSON(w, http.StatusOK, map[string]any{"id": a.id, "target": a.target, "done": false, "started": a.started})
		}
		return
	}
	active := []any{}
	h.Svc.asyncMu.Lock()
	for _, a := range h.Svc.async {
		if a.target != target {
			continue
		}
		select {
		case <-a.done:
		default:
			active = append(active, map[string]any{"id": a.id, "started": a.started})
		}
	}
	h.Svc.asyncMu.Unlock()
	recent := []any{}
	for _, rec := range h.Svc.RecentSyncs(target) {
		recent = append(recent, taskJSON(rec))
	}
	writeJSON(w, http.StatusOK, map[string]any{"active": active, "recent": recent})
}

// --- POST /api/v1/repos/mirrors (create-from-URL) ----------------------------
//
// Write-gated (repo creation parity) + the dangerous-confirm
// authority rule for off-allowlist hosts (admin under token/oidc —
// the import dangerousAllowed shape). Creates the empty repo, writes
// the sidecar, and runs the first sync immediately — the token (when
// given) is memory-only for that first sync and dropped after.
func (h *Handler) createFromURL(w http.ResponseWriter, r *http.Request) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeAuthErr(w, aerr)
		return
	}
	if !p.Write && !p.Admin {
		if p.Anonymous {
			writeAuthErr(w, &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"})
		} else {
			writePlain(w, http.StatusForbidden, "write access required")
		}
		return
	}
	if h.Svc == nil || h.CreateRepo == nil {
		writePlain(w, http.StatusServiceUnavailable, "mirror service not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writePlain(w, http.StatusBadRequest, "unreadable body")
		return
	}
	var in struct {
		SourceURL string `json:"source_url"`
		Owner     string `json:"owner"`
		Name      string `json:"name"`
		Schedule  string `json:"schedule"`
		Token     string `json:"token"`
		Dangerous bool   `json:"dangerous"`
	}
	if derr := decodeStrict(body, &in); derr != nil {
		writePlain(w, http.StatusBadRequest, derr.Error())
		return
	}
	if strings.TrimSpace(in.Owner) == "" || strings.TrimSpace(in.Name) == "" {
		writePlain(w, http.StatusBadRequest, "owner and name are required")
		return
	}
	if _, err := git.ParseRepoId(in.Owner + "/" + in.Name); err != nil {
		writePlain(w, http.StatusBadRequest, "bad target: "+err.Error())
		return
	}
	schedule := in.Schedule
	if schedule == "" {
		schedule = DefaultPreset
	}
	if !ValidPreset(schedule) {
		writePlain(w, http.StatusBadRequest, "unknown schedule preset "+strconv.Quote(schedule)+": want one of "+strings.Join(Presets, "|"))
		return
	}
	n, nerr := repoimport.NormalizeSource(in.SourceURL)
	if nerr != nil {
		writeStatusErr(w, nerr)
		return
	}
	if verr := repoimport.ValidateTransport(n, in.Token != ""); verr != nil {
		writeStatusErr(w, verr)
		return
	}
	// The dangerous confirm's AUTHORITY (import fix #237): an
	// authenticated admin under a real auth mode — url.go only
	// evaluates the flag.
	if in.Dangerous && !h.dangerousAllowed(p) {
		writePlain(w, http.StatusForbidden, "dangerous:true requires an authenticated admin (server.auth.mode token/oidc)")
		return
	}
	if serr := h.checkSSRF(n, in.Dangerous); serr != nil {
		writeStatusErr(w, serr)
		return
	}
	ctx := r.Context()
	if cerr := h.CreateRepo(ctx, in.Owner, in.Name); cerr != nil {
		if isExists(cerr) || isRepoExists(cerr) {
			writePlain(w, http.StatusConflict, "repository already exists")
			return
		}
		writePlain(w, http.StatusInternalServerError, "mirror: "+scrubText(cerr.Error()))
		return
	}
	if _, cerr := Create(ctx, h.Svc.store, in.Owner, in.Name, n.URL, schedule); cerr != nil {
		if isExists(cerr) {
			writePlain(w, http.StatusConflict, "already a mirror")
			return
		}
		writePlain(w, http.StatusInternalServerError, "mirror: "+scrubText(cerr.Error()))
		return
	}
	id := h.Svc.SyncAsync(ctx, in.Owner, in.Name, in.Token, false)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusAccepted, map[string]any{"task": map[string]any{"id": id}, "target": in.Owner + "/" + in.Name})
}

// dangerousAllowed mirrors the import rule: an authenticated admin
// under token/oidc (auth-none Admin is unauthenticated — anyone on
// the network — so mode none never qualifies).
func (h *Handler) dangerousAllowed(p auth.Principal) bool {
	if p.Anonymous || !p.Admin {
		return false
	}
	mode := "none"
	if h.AuthMode != nil {
		if m := h.AuthMode(); m != "" {
			mode = m
		}
	}
	return mode == "token" || mode == "oidc"
}

// checkSSRF enforces the v1 egress gate on a normalized source (the
// stored canonical URL re-gates host/private only — the dangerous
// authority was checked at creation).
func (h *Handler) checkSSRF(n repoimport.Normalized, dangerous bool) error {
	if h.Svc == nil {
		return nil
	}
	return repoimport.CheckSSRF(n, repoimport.SSRFConfig{
		AllowPrivate: h.Svc.allowPrivate,
		Allowlist:    h.Svc.allowlist,
		AllowFile:    h.Svc.allowFile,
		Dangerous:    dangerous,
	}, nil)
}

// --- wire shapes --------------------------------------------------------------

func taskJSON(rec *wal.TaskRecord) map[string]any {
	if rec == nil {
		return map[string]any{}
	}
	logTail := rec.LogTail
	if logTail == nil {
		logTail = []string{}
	}
	out := map[string]any{
		"id": rec.ID, "kind": rec.Kind, "repo": rec.Repo, "hostname": rec.Hostname,
		"started": rec.Started, "elapsed_ms": rec.ElapsedMS,
		"summary": rec.Summary, "log_tail": logTail,
	}
	if rec.Finished != "" {
		out["finished"] = rec.Finished
	}
	if rec.OK != nil {
		out["ok"] = *rec.OK
	}
	if rec.Progress != nil {
		out["progress"] = map[string]any{"label": rec.Progress.Label, "done": rec.Progress.Done, "unit": rec.Progress.Unit}
	}
	if len(rec.Params) > 0 {
		out["params"] = rec.Params
	}
	return out
}

// decodeStrict decodes JSON with unknown fields rejected (fail closed
// — the policy strictness rule applied to the mirror surface).
func decodeStrict(body []byte, v any) error {
	if len(body) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %v", err)
	}
	return nil
}

// --- writers -------------------------------------------------------------------

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
	if se, ok := err.(*repoimport.StatusError); ok {
		writePlain(w, se.Status, se.Message)
		return
	}
	writePlain(w, http.StatusInternalServerError, scrubText(err.Error()))
}

// isExists reports the Create-once 412 (already a mirror — the caller
// maps it to 409, the import-claim 412 shape).
func isExists(err error) bool { return store.IsPreconditionFailed(err) }

// isRepoExists reports the registry's already-exists (a raced repo
// create — also a 409, never a 500).
func isRepoExists(err error) bool {
	var we *wal.WalError
	return errors.As(err, &we) && we.Kind == wal.WalErrAlreadyExists
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
