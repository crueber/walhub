// http.go — the Seam 1 surface (server.ExtraRoutes, both lanes): the
// repo-lane push-mirror config + Sync-now + keygen routes.
//
// Wire conventions (07 §2): JSON success, plain-text errors, arrays []
// never null, RFC 3339 UTC, per-segment decoding, no-store on task
// starts, both lanes everywhere. Anonymous-denied writes get a real 401
// with WWW-Authenticate: Bearer (never a 200 with an in-band error).
//
// Authz: GET is open (the config sidecar holds no secrets — the summary
// renders a redacted view to readers alike); PUT/DELETE/POST are
// admin-only. Secrets are write-only: the API never echoes stored
// material in full (presence + last-4 hint only), and every error is
// scrubbed.
//
// No top-level twins by design: push mirrors are configured post-hoc on
// existing repos only (the issue: never at create or import time), so
// every route is repo-scoped (both lanes). Discovery lists the repo
// lanes via api.RegisterExposed from composition (Forgejo #272 rule).
package pushmirror

import (
	"context"
	"encoding/json"
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

// ExposedTemplates lists the discovery endpoints[] entries this surface
// serves (14 §14.12 lane rule) — registered from composition via
// api.RegisterExposed in the same change (law 12). Repo lanes use the
// /{owner}/{repo}/api spelling. Pinned by TestExposedTemplatesExact +
// TestExposedCoversRoutes (no phantoms, nothing missing).
var ExposedTemplates = []string{
	"/{owner}/{repo}/api/pushmirror",
	"/{owner}/{repo}/api/pushmirror/sync",
	"/{owner}/{repo}/api/pushmirror/keygen",
}

// Handler is the Seam 1 surface. Composition chains it in front of the
// core mux: Handle reports false for non-pushmirror paths so the core
// mux answers (the server.ExtraRoutes chain, exactly like
// internal/mirror).
type Handler struct {
	Svc  *Service
	Auth Authenticator

	// Now is overridable for tests.
	Now func() time.Time
}

// Authenticator resolves the request principal through Seam 2 (the
// server's AuthService, injected by composition). Nil falls back to
// anonymous.
type Authenticator func(r *http.Request) (auth.Principal, *auth.AuthError)

func (h *Handler) principal(r *http.Request) (auth.Principal, *auth.AuthError) {
	if h.Auth != nil {
		return h.Auth(r)
	}
	return auth.Anonymous(), nil
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// Handle answers one request; false when the path is not a pushmirror route.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r)
	// Repo lanes: /{owner}/{repo}/(api|api-browser)/pushmirror[/sync|/keygen].
	if len(segs) >= 4 && (segs[2] == "api" || segs[2] == "api-browser") {
		owner, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
		if _, err := git.ParseRepoId(owner + "/" + repo); err != nil {
			return false
		}
		rest := segs[3:]
		if len(rest) == 0 || rest[0] != "pushmirror" {
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
		case len(rest) == 2 && rest[1] == "keygen" && r.Method == http.MethodPost:
			h.keygen(w, r, owner, repo)
			return true
		case len(rest) == 1:
			// Known shape (config), wrong method.
			writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
			return true
		case len(rest) == 2 && (rest[1] == "sync" || rest[1] == "keygen"):
			// Known shapes, wrong method. Anything else (e.g.
			// /pushmirror/extra) falls through to the core mux.
			writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
			return true
		}
	}
	return false
}

// ServeHTTP answers pushmirror routes and 404s otherwise (httptest surface).
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

func decodeSegment(s string) string {
	if d, err := url.PathUnescape(s); err == nil {
		return d
	}
	return s
}

// --- GET /{o}/{r}/api/pushmirror --------------------------------------------
//
// Open read (the config sidecar holds no secrets — material lives in the
// secret sidecar and renders as presence + last-4 only). 404 when no
// push mirror is configured.
func (h *Handler) get(w http.ResponseWriter, r *http.Request, owner, repo string) {
	if h.Svc == nil {
		writePlain(w, http.StatusServiceUnavailable, "push-mirror service not configured")
		return
	}
	ctx := r.Context()
	doc, _, err := Load(ctx, h.Svc.store, owner, repo)
	if err != nil {
		writePlain(w, http.StatusInternalServerError, "pushmirror: "+scrubText(err.Error()))
		return
	}
	if doc == nil {
		writePlain(w, http.StatusNotFound, "no push mirror configured")
		return
	}
	sec, _, serr := LoadSecret(ctx, h.Svc.store, owner, repo)
	if serr != nil {
		writePlain(w, http.StatusInternalServerError, "pushmirror: "+scrubText(serr.Error()))
		return
	}
	writeJSON(w, http.StatusOK, ViewOf(doc, sec, h.now()))
}

// putBody is the strict PUT shape: credentials write-only, schedule "" =
// on-push only (the default), upstream immutable after create.
type putBody struct {
	UpstreamURL   string `json:"upstream_url"`
	AuthKind      string `json:"auth_kind"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	Token         string `json:"token"`
	SSHPrivateKey string `json:"ssh_private_key"`
	SSHKnownHosts string `json:"ssh_known_hosts"`
	SSHPublicKey  string `json:"ssh_public_key"`
	Schedule      string `json:"schedule"`
	Dangerous     bool   `json:"dangerous"`
}

// --- PUT /{o}/{r}/api/pushmirror --------------------------------------------
//
// Admin-only. No config yet → create (upstream_url required, auth_kind
// defaults to none, schedule defaults to "" off) — the repo must already
// exist (404 otherwise — PUT never creates repos). Config present →
// update: upstream_url change is 409 (delete-and-recreate, so the outcome
// anchor never silently switches sources); auth material replaces when
// the corresponding field is non-empty (omitted = keep); schedule ""
// turns scheduled sync off.
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
		writePlain(w, http.StatusServiceUnavailable, "push-mirror service not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writePlain(w, http.StatusBadRequest, "unreadable body")
		return
	}
	var in putBody
	if err := decodeStrict(body, &in); err != nil {
		writePlain(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	doc, ver, lerr := Load(ctx, h.Svc.store, owner, repo)
	if lerr != nil {
		writePlain(w, http.StatusInternalServerError, "pushmirror: "+scrubText(lerr.Error()))
		return
	}
	if doc != nil {
		h.update(ctx, w, body, owner, repo, doc, ver, &in)
		return
	}
	h.create(ctx, w, owner, repo, &in)
}

// create writes a fresh config + secret pair on an existing repo.
func (h *Handler) create(ctx context.Context, w http.ResponseWriter, owner, repo string, in *putBody) {
	if strings.TrimSpace(in.UpstreamURL) == "" {
		writePlain(w, http.StatusBadRequest, "upstream_url is required to configure a push mirror")
		return
	}
	kind := in.AuthKind
	if kind == "" {
		kind = AuthNone
	}
	if !ValidAuthKind(kind) {
		writePlain(w, http.StatusBadRequest, "unknown auth kind "+strconv.Quote(kind)+": want one of none|password|token|ssh")
		return
	}
	schedule := in.Schedule
	if !ValidSchedule(schedule) {
		writePlain(w, http.StatusBadRequest, "unknown schedule preset "+strconv.Quote(schedule)+": want one of \"\"|"+strings.Join(Presets, "|"))
		return
	}
	n, nerr := repoimport.NormalizeSource(in.UpstreamURL)
	if nerr != nil {
		writeStatusErr(w, nerr)
		return
	}
	if verr := ValidateTarget(n, kind, hasInputMaterial(in)); verr != nil {
		writePlain(w, http.StatusBadRequest, scrubText(verr.Error()))
		return
	}
	if serr := h.checkSSRF(n, in.Dangerous); serr != nil {
		writeStatusErr(w, serr)
		return
	}
	// PUT is config on a repo, never repo creation. A sidecar on an
	// unborn repo would strand the name while every sync fails at Open.
	if meta, herr := h.Svc.store.Head(ctx, store.RepoPrefix(owner, repo)+store.Manifest); herr != nil && !store.IsNotFound(herr) {
		writePlain(w, http.StatusInternalServerError, "pushmirror: "+scrubText(herr.Error()))
		return
	} else if herr != nil || meta == nil {
		writePlain(w, http.StatusNotFound, "repository not found")
		return
	}
	sec, serr := secretFromInput(kind, strings.TrimSpace(in.Username), in)
	if serr != nil {
		writePlain(w, http.StatusBadRequest, scrubText(serr.Error()))
		return
	}
	doc, cerr := Create(ctx, h.Svc.store, owner, repo, n.URL, kind, strings.TrimSpace(in.Username), schedule)
	if cerr != nil {
		if isExists(cerr) {
			writePlain(w, http.StatusConflict, "push mirror already configured")
			return
		}
		writePlain(w, http.StatusInternalServerError, "pushmirror: "+scrubText(cerr.Error()))
		return
	}
	if sec != nil {
		if serr := SaveSecret(ctx, h.Svc.store, owner, repo, sec); serr != nil {
			writePlain(w, http.StatusInternalServerError, "pushmirror: "+scrubText(serr.Error()))
			return
		}
	}
	// No first-sync fire on create (unlike pull mirrors): the next
	// client push fans out on its own, and an immediate push-back of
	// current state is one Sync-now away. Creation stays a pure config
	// write — cheap and unsurprising.
	writeJSON(w, http.StatusCreated, ViewOf(doc, sec, h.now()))
}

// update rewrites schedule/auth on an existing config. Upstream changes
// are 409 (delete-and-recreate). Auth fields replace only when
// non-empty in the request (omitted = keep stored material); auth_kind
// changes drop the old material shape for the new one. hasSchedule
// distinguishes "schedule omitted" (keep) from "schedule: \"\"" (OFF).
func (h *Handler) update(ctx context.Context, w http.ResponseWriter, body []byte, owner, repo string, doc *Doc, ver store.Version, in *putBody) {
	hasSchedule := bodyHasKey(body, "schedule")
	hasKnownHosts := bodyHasKey(body, "ssh_known_hosts")
	if strings.TrimSpace(in.UpstreamURL) != "" && strings.TrimSpace(in.UpstreamURL) != doc.UpstreamURL {
		// Canonical-compare: same source in different spelling is not a change.
		if n, nerr := repoimport.NormalizeSource(in.UpstreamURL); nerr != nil || n.URL != doc.UpstreamURL {
			writePlain(w, http.StatusConflict, "push-mirror upstream_url is immutable; delete the push mirror and recreate it to change destinations")
			return
		}
	}
	kind := doc.AuthKind
	if in.AuthKind != "" {
		if !ValidAuthKind(in.AuthKind) {
			writePlain(w, http.StatusBadRequest, "unknown auth kind "+strconv.Quote(in.AuthKind)+": want one of none|password|token|ssh")
			return
		}
		kind = in.AuthKind
	}
	if !ValidSchedule(in.Schedule) && hasSchedule {
		// Explicit unknown preset fails closed ("" is OFF — valid;
		// anything else unknown is 400).
		writePlain(w, http.StatusBadRequest, "unknown schedule preset "+strconv.Quote(in.Schedule)+": want one of \"\"|"+strings.Join(Presets, "|"))
		return
	}
	if hasSchedule {
		doc.Schedule = in.Schedule
	}
	// Re-validate the destination against the final kind BEFORE writing
	// any secret: the kind-change branch below SaveSecrets, and a late
	// 400 there would leave secret material for a kind the config no
	// longer names (bricked until repaired by hand).
	if n, nerr := repoimport.NormalizeSource(doc.UpstreamURL); nerr != nil {
		writePlain(w, http.StatusInternalServerError, "pushmirror: "+scrubText(nerr.Error()))
		return
	} else if verr := ValidateTarget(n, kind, true); verr != nil {
		writePlain(w, http.StatusBadRequest, scrubText(verr.Error()))
		return
	}
	if in.AuthKind != "" && in.AuthKind != doc.AuthKind {
		doc.AuthKind = kind
		doc.Username = strings.TrimSpace(in.Username)
		doc.PublicKey = ""
		doc.KeyFingerprint = ""
		// Old material shape is dropped: loading the stale secret for
		// a new kind would silently downgrade identity.
		sec, serr := secretFromInput(kind, strings.TrimSpace(in.Username), in)
		if serr != nil {
			writePlain(w, http.StatusBadRequest, scrubText(serr.Error()))
			return
		}
		if err := SaveSecret(ctx, h.Svc.store, owner, repo, effectiveSecret(sec, kind)); err != nil {
			writePlain(w, http.StatusInternalServerError, "pushmirror: "+scrubText(err.Error()))
			return
		}
	} else {
		if strings.TrimSpace(in.Username) != "" {
			doc.Username = strings.TrimSpace(in.Username)
		}
		if hasInputMaterial(in) {
			sec, _, _ := LoadSecret(ctx, h.Svc.store, owner, repo)
			merged, serr := mergeSecretInput(sec, doc.AuthKind, doc.Username, in, hasKnownHosts)
			if serr != nil {
				writePlain(w, http.StatusBadRequest, scrubText(serr.Error()))
				return
			}
			if err := SaveSecret(ctx, h.Svc.store, owner, repo, merged); err != nil {
				writePlain(w, http.StatusInternalServerError, "pushmirror: "+scrubText(err.Error()))
				return
			}
		}
	}
	// kind is already validated above (validate-before-write).
	if err := UpdateCAS(ctx, h.Svc.store, owner, repo, doc, ver); err != nil {
		writePlain(w, http.StatusInternalServerError, "pushmirror: "+scrubText(err.Error()))
		return
	}
	sec, _, _ := LoadSecret(ctx, h.Svc.store, owner, repo)
	writeJSON(w, http.StatusOK, ViewOf(doc, sec, h.now()))
}

// --- DELETE /{o}/{r}/api/pushmirror -----------------------------------------
//
// Admin-only. Deleting stops on-push fan-out and scheduled syncs for the
// repo (probe-absent → skip — no orphaned syncs, no extra machinery).
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
		writePlain(w, http.StatusServiceUnavailable, "push-mirror service not configured")
		return
	}
	doc, _, lerr := Load(r.Context(), h.Svc.store, owner, repo)
	if lerr != nil {
		writePlain(w, http.StatusInternalServerError, "pushmirror: "+scrubText(lerr.Error()))
		return
	}
	if doc == nil {
		writePlain(w, http.StatusNotFound, "no push mirror configured")
		return
	}
	if derr := Delete(r.Context(), h.Svc.store, owner, repo); derr != nil {
		writePlain(w, http.StatusInternalServerError, "pushmirror: "+scrubText(derr.Error()))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- POST /{o}/{r}/api/pushmirror/sync --------------------------------------
//
// Admin-only manual "Sync now" (202, join-or-run by
// (repo,mirror-push-sync)). The body carries only the force flag —
// credentials come from the stored secret sidecar (never the request:
// echoing them per-fire would reintroduce the stored-token surface the
// secret sidecar exists to avoid).
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
		writePlain(w, http.StatusServiceUnavailable, "push-mirror service not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writePlain(w, http.StatusBadRequest, "unreadable body")
		return
	}
	var in struct {
		Force bool `json:"force"`
	}
	if len(body) > 0 {
		if derr := decodeStrict(body, &in); derr != nil {
			writePlain(w, http.StatusBadRequest, derr.Error())
			return
		}
	}
	doc, _, lerr := Load(r.Context(), h.Svc.store, owner, repo)
	if lerr != nil {
		writePlain(w, http.StatusInternalServerError, "pushmirror: "+scrubText(lerr.Error()))
		return
	}
	if doc == nil {
		writePlain(w, http.StatusNotFound, "no push mirror configured")
		return
	}
	id := h.Svc.SyncAsync(r.Context(), owner, repo, in.Force)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusAccepted, map[string]any{"task": map[string]any{"id": id}, "target": owner + "/" + repo})
}

// --- GET /{o}/{r}/api/pushmirror/sync ---------------------------------------
//
// Resolves a manual-sync 202: ?id=<async-id> → {task, done, outcome};
// without id → {active:[...], recent:[...]} (recent = finished
// mirror-push-sync table records, newest last). Open read, like GET.
func (h *Handler) syncStatus(w http.ResponseWriter, r *http.Request, owner, repo string) {
	if h.Svc == nil {
		writePlain(w, http.StatusServiceUnavailable, "push-mirror service not configured")
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

// --- POST /{o}/{r}/api/pushmirror/keygen ------------------------------------
//
// Admin-only. Generates a fresh ed25519 deploy keypair, stores the
// private key in the secret sidecar (auth_kind becomes ssh), and returns
// the PUBLIC key + fingerprint for upstream install. The private key is
// NEVER returned (write-only). Requires an existing push-mirror config
// (404 otherwise — keygen is config on a repo, never repo creation) on
// an SSH upstream (400 otherwise — keygen would brick an https config).
func (h *Handler) keygen(w http.ResponseWriter, r *http.Request, owner, repo string) {
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
		writePlain(w, http.StatusServiceUnavailable, "push-mirror service not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writePlain(w, http.StatusBadRequest, "unreadable body")
		return
	}
	var in struct {
		KnownHosts string `json:"known_hosts"`
	}
	if len(body) > 0 {
		if derr := decodeStrict(body, &in); derr != nil {
			writePlain(w, http.StatusBadRequest, derr.Error())
			return
		}
	}
	ctx := r.Context()
	doc, ver, lerr := Load(ctx, h.Svc.store, owner, repo)
	if lerr != nil {
		writePlain(w, http.StatusInternalServerError, "pushmirror: "+scrubText(lerr.Error()))
		return
	}
	if doc == nil {
		writePlain(w, http.StatusNotFound, "no push mirror configured")
		return
	}
	// Keygen mints an SSH deploy key: refuse unless the destination is
	// an SSH upstream (otherwise the config would flip to a kind the
	// stored URL rejects — https+ssh fails ValidateTarget at sync
	// time, leaving a bricked config behind a 200).
	if n, nerr := repoimport.NormalizeSource(doc.UpstreamURL); nerr != nil {
		writePlain(w, http.StatusInternalServerError, "pushmirror: "+scrubText(nerr.Error()))
		return
	} else if n.Scheme != "ssh" && n.Scheme != "scp" {
		writePlain(w, http.StatusBadRequest, "pushmirror keygen needs an ssh upstream (this mirror points at "+n.Scheme+")")
		return
	}
	key, gerr := GenerateKeypair("")
	if gerr != nil {
		writePlain(w, http.StatusInternalServerError, "pushmirror: "+scrubText(gerr.Error()))
		return
	}
	doc.AuthKind = AuthSSH
	doc.PublicKey = key.PublicKey
	doc.KeyFingerprint = key.Fingerprint
	if err := UpdateCAS(ctx, h.Svc.store, owner, repo, doc, ver); err != nil {
		writePlain(w, http.StatusInternalServerError, "pushmirror: "+scrubText(err.Error()))
		return
	}
	sec := &Secret{AuthKind: AuthSSH, Username: doc.Username, SSHPrivateKey: key.PrivatePEM, SSHKnownHosts: in.KnownHosts}
	if err := SaveSecret(ctx, h.Svc.store, owner, repo, sec); err != nil {
		writePlain(w, http.StatusInternalServerError, "pushmirror: "+scrubText(err.Error()))
		return
	}
	// The response carries the PUBLIC key only (install this upstream).
	// The private key stays in the secret sidecar (presence + hint).
	writeJSON(w, http.StatusOK, map[string]any{
		"public_key":      key.PublicKey,
		"key_fingerprint": key.Fingerprint,
		"has_secret":      true,
		"secret_hint":     sec.SecretHint(),
		"mirror":          ViewOf(doc, sec, h.now()),
	})
}

// --- secret input helpers ----------------------------------------------------

// hasInputMaterial reports whether the PUT body carries any credential
// field (non-empty password/token/private-key — known_hosts alone does
// not count: it is trust config, not material).
func hasInputMaterial(in *putBody) bool {
	return in.Password != "" || in.Token != "" || in.SSHPrivateKey != ""
}

// secretFromInput builds the secret sidecar from a create/update body.
// A user-provided ssh public line is validated (ed25519 only) and its
// fingerprint recorded by the CALLER via the returned public key — the
// secret itself carries only the private half + trust config.
func secretFromInput(kind, username string, in *putBody) (*Secret, error) {
	switch kind {
	case AuthNone:
		if hasInputMaterial(in) {
			return nil, fmt.Errorf("pushmirror: auth_kind none takes no credentials")
		}
		return nil, nil
	case AuthPassword:
		if in.Password == "" {
			return nil, fmt.Errorf("pushmirror: password auth needs the password field")
		}
		return &Secret{AuthKind: kind, Username: username, Password: in.Password}, nil
	case AuthToken:
		if in.Token == "" {
			return nil, fmt.Errorf("pushmirror: token auth needs the token field")
		}
		return &Secret{AuthKind: kind, Username: username, Token: in.Token}, nil
	case AuthSSH:
		if in.SSHPrivateKey == "" {
			return nil, fmt.Errorf("pushmirror: ssh auth needs ssh_private_key (paste a key) or POST pushmirror/keygen (we generate one)")
		}
		sec := &Secret{AuthKind: kind, Username: username, SSHPrivateKey: in.SSHPrivateKey, SSHKnownHosts: in.SSHKnownHosts}
		if in.SSHPublicKey != "" {
			if _, err := ParsePublicLine(in.SSHPublicKey); err != nil {
				return nil, err
			}
		}
		return sec, nil
	}
	return nil, fmt.Errorf("pushmirror: unknown auth kind %q", kind)
}

// effectiveSecret maps a nil secret (auth none) to a tombstone-free nil
// (no secret sidecar for none) and passes material kinds through.
func effectiveSecret(sec *Secret, kind string) *Secret {
	if kind == AuthNone {
		return &Secret{AuthKind: kind}
	}
	if sec == nil {
		return &Secret{AuthKind: kind}
	}
	sec.AuthKind = kind
	return sec
}

// mergeSecretInput folds non-empty credential fields over the stored
// secret (omitted = keep). A provided public line is validated.
// hasKnownHosts distinguishes "known_hosts omitted" (keep) from
// "known_hosts: \"\"" (clear to accept-new).
func mergeSecretInput(stored *Secret, kind, username string, in *putBody, hasKnownHosts bool) (*Secret, error) {
	sec := &Secret{AuthKind: kind, Username: username}
	if stored != nil {
		*sec = *stored
		sec.AuthKind = kind
		sec.Username = username
	}
	switch kind {
	case AuthPassword:
		if in.Password != "" {
			sec.Password = in.Password
		}
		if sec.Password == "" {
			return nil, fmt.Errorf("pushmirror: password auth needs the password field")
		}
	case AuthToken:
		if in.Token != "" {
			sec.Token = in.Token
		}
		if sec.Token == "" {
			return nil, fmt.Errorf("pushmirror: token auth needs the token field")
		}
	case AuthSSH:
		if in.SSHPrivateKey != "" {
			sec.SSHPrivateKey = in.SSHPrivateKey
		}
		if hasKnownHosts {
			sec.SSHKnownHosts = in.SSHKnownHosts
		}
		if sec.SSHPrivateKey == "" {
			return nil, fmt.Errorf("pushmirror: ssh auth needs ssh_private_key (paste a key) or POST pushmirror/keygen (we generate one)")
		}
		if in.SSHPublicKey != "" {
			if _, err := ParsePublicLine(in.SSHPublicKey); err != nil {
				return nil, err
			}
		}
	}
	return sec, nil
}

// bodyHasKey reports whether the raw JSON body names key (so "" can
// mean OFF for schedule without conflating "omitted" — the PUT
// schedule/known_hosts discipline). Malformed bodies report false (the
// strict decoder already rejected them upstream).
func bodyHasKey(body []byte, key string) bool {
	if len(body) == 0 {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

// --- SSRF gate ----------------------------------------------------------------

// checkSSRF enforces the egress gate on a normalized destination (the
// stored canonical URL re-gates host/private only — the admin-only PUT
// is the authority, so the explicit dangerous flag covers off-allowlist
// hosts under a locked-down instance, the import shape).
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

// decodeStrict decodes JSON with unknown fields rejected (fail closed).
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

func isExists(err error) bool { return store.IsPreconditionFailed(err) }

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

var _ = fmt.Sprintf // keep fmt referenced while helpers evolve
