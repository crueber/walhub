package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// --- placeholder fakes -----------------------------------------------------

type fakeCreateOwnerGate struct {
	exists  map[string]bool
	members map[string]map[string]bool
	err     error
}

// CheckCreateOwner mirrors the production #346 rule over the scripted
// roster: anonymous → 401, admin → admit, self → admit, listed member →
// admit, else 403 naming the owner; err → unavailable (caller maps 503).
func (f *fakeCreateOwnerGate) CheckCreateOwner(_ context.Context, owner string, p auth.Principal) *auth.AuthError {
	if f.err != nil {
		return &auth.AuthError{Kind: auth.ErrUnavailable, Why: "org membership unavailable"}
	}
	if p.Anonymous {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	if p.Admin {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(owner), strings.TrimSpace(p.Name)) {
		return nil
	}
	if f.exists[owner] && f.members[owner][p.Name] {
		return nil
	}
	return &auth.AuthError{Kind: auth.ErrForbidden, Why: "owner " + owner + " not permitted"}
}

type fakeAccessBoot struct {
	calls [][4]string
	err   error
}

func (f *fakeAccessBoot) EnsureRepoAccess(_ context.Context, owner, repo, creator, visibility string) error {
	f.calls = append(f.calls, [4]string{owner, repo, creator, visibility})
	return f.err
}

func placeholderFixture(t *testing.T) (*fixture, *fakeCreateOwnerGate, *fakeAccessBoot) {
	t.Helper()
	f := newFixture(t)
	g := &fakeCreateOwnerGate{exists: map[string]bool{}, members: map[string]map[string]bool{}}
	b := &fakeAccessBoot{}
	f.env.CreateOwnerGate = g
	f.env.AccessBoot = b
	return f, g, b
}

func writerPrincipal(name string) *auth.Principal {
	return &auth.Principal{Name: name, Write: true, Admin: true}
}

// writeOnlyPrincipal is a non-admin writer (the gate matrix needs
// principals the admin bypass does not rescue).
func writeOnlyPrincipal(name string) *auth.Principal {
	return &auth.Principal{Name: name, Write: true}
}

// --- PUT ?placeholder=true -------------------------------------------------

func TestPutPlaceholderCreatesSidecar(t *testing.T) {
	f, _, b := placeholderFixture(t)
	w := f.do("PUT", "/acme/newthing?placeholder=true", nil, nil, writerPrincipal("alice@example.com"))
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT placeholder = %d (%s)", w.Code, w.Body.String())
	}
	var body map[string]any
	decodeJSON(t, w, &body)
	if body["placeholder"] != true {
		t.Fatalf("placeholder flag missing: %v", body)
	}
	if body["clone_url"] == nil || !strings.HasSuffix(body["clone_url"].(string), "/acme/newthing.git") {
		t.Fatalf("clone_url: %v", body)
	}
	// Sidecar present by exact key.
	raw, _, err := store.GetBytes(context.Background(), f.env.Store, "repos/acme/newthing/"+PlaceholderKeySuffix, store.GetOptions{})
	if err != nil || raw == nil {
		t.Fatalf("sidecar missing: %v", err)
	}
	var doc PlaceholderDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != 1 || doc.CreatedBy != "alice@example.com" || doc.ObjectFormat != "sha1" || doc.CreatedAt == "" {
		t.Fatalf("sidecar doc: %+v", doc)
	}
	if doc.ExpiresAt != nil {
		t.Fatalf("expires_at must be null (TTL off): %+v", doc)
	}
	// Eager access default ran once (PUT-flag path: no visibility concept).
	if len(b.calls) != 1 || b.calls[0] != [4]string{"acme", "newthing", "alice@example.com", ""} {
		t.Fatalf("access boot calls: %v", b.calls)
	}
}

func TestPutPlaceholderWithoutFlagUnchanged(t *testing.T) {
	f, _, b := placeholderFixture(t)
	w := f.do("PUT", "/acme/plain", nil, nil, writerPrincipal("alice@example.com"))
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT = %d (%s)", w.Code, w.Body.String())
	}
	var body map[string]any
	decodeJSON(t, w, &body)
	if _, ok := body["placeholder"]; ok {
		t.Fatalf("flag-less PUT must not carry placeholder: %v", body)
	}
	raw, _, _ := store.GetBytes(context.Background(), f.env.Store, "repos/acme/plain/"+PlaceholderKeySuffix, store.GetOptions{})
	if raw != nil {
		t.Fatal("flag-less PUT must not write a sidecar")
	}
	if len(b.calls) != 0 {
		t.Fatalf("flag-less PUT must not boot access: %v", b.calls)
	}
}

func TestPutPlaceholderCreateTwice(t *testing.T) {
	f, _, _ := placeholderFixture(t)
	// The view sees the created repo as unborn (no refs yet).
	f.view.summaries["acme/twice"] = SummaryData{}
	if w := f.do("PUT", "/acme/twice?placeholder=true", nil, nil, writerPrincipal("alice@example.com")); w.Code != 201 {
		t.Fatalf("first = %d", w.Code)
	}
	// Same principal → idempotent 200 already:true, no state change.
	w := f.do("PUT", "/acme/twice?placeholder=true", nil, nil, writerPrincipal("alice@example.com"))
	if w.Code != http.StatusOK {
		t.Fatalf("same-principal re-create = %d (%s)", w.Code, w.Body.String())
	}
	var body map[string]any
	decodeJSON(t, w, &body)
	if body["already"] != true || body["placeholder"] != true {
		t.Fatalf("idempotent body: %v", body)
	}
	// Different principal → 409 plain-text carrying the winner URL.
	w = f.do("PUT", "/acme/twice?placeholder=true", nil, nil, writerPrincipal("bob@example.com"))
	if w.Code != http.StatusConflict {
		t.Fatalf("diff-principal re-create = %d (%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("409 must be plain-text, got %q", ct)
	}
	if !strings.Contains(w.Body.String(), "/acme/twice") {
		t.Fatalf("409 must carry the winner URL: %q", w.Body.String())
	}
	// PUT without the flag on an existing placeholder → legacy 409.
	w = f.do("PUT", "/acme/twice", nil, nil, writerPrincipal("alice@example.com"))
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "http") {
		t.Fatalf("flag-less re-PUT = %d (%q), want legacy 409", w.Code, w.Body.String())
	}
}

func TestPutPlaceholderSamePrincipalCaseInsensitive(t *testing.T) {
	f, _, _ := placeholderFixture(t)
	f.view.summaries["acme/ci"] = SummaryData{}
	if w := f.do("PUT", "/acme/ci?placeholder=true", nil, nil, writerPrincipal("Alice@Example.com")); w.Code != 201 {
		t.Fatalf("first = %d", w.Code)
	}
	w := f.do("PUT", "/acme/ci?placeholder=true", nil, nil, writerPrincipal("alice@example.com"))
	if w.Code != 200 {
		t.Fatalf("case-folded same principal = %d (%s)", w.Code, w.Body.String())
	}
}

func TestPutPlaceholderBadFormat(t *testing.T) {
	f, _, _ := placeholderFixture(t)
	w := f.do("PUT", "/acme/bad?placeholder=true&object_format=bogus", nil, nil, writerPrincipal("a@b.c"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad format = %d", w.Code)
	}
}

func TestPutPlaceholderWarningOnUICollision(t *testing.T) {
	f, _, _ := placeholderFixture(t)
	w := f.do("PUT", "/import/thing?placeholder=true", nil, nil, writerPrincipal("a@b.c"))
	if w.Code != 201 {
		t.Fatalf("PUT = %d (%s)", w.Code, w.Body.String())
	}
	var body map[string]any
	decodeJSON(t, w, &body)
	if body["warning"] == nil || !strings.Contains(body["warning"].(string), "UI route") {
		t.Fatalf("collision warning missing: %v", body)
	}
}

// --- org gate ---------------------------------------------------------------

func TestPutPlaceholderOrgGate(t *testing.T) {
	f, g, _ := placeholderFixture(t)
	g.exists["acme"] = true
	g.members["acme"] = map[string]bool{"alice@example.com": true}
	// Non-member (non-admin) → 403 naming the owner.
	w := f.do("PUT", "/acme/gated?placeholder=true", nil, nil, writeOnlyPrincipal("mallory@example.com"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-member = %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "acme") {
		t.Fatalf("403 must name the owner: %q", w.Body.String())
	}
	// Member → 201.
	w = f.do("PUT", "/acme/gated?placeholder=true", nil, nil, writeOnlyPrincipal("alice@example.com"))
	if w.Code != 201 {
		t.Fatalf("member = %d (%s)", w.Code, w.Body.String())
	}
	// Self-namespace (owner == principal, no org involved) → 201.
	w = f.do("PUT", "/mallory/self?placeholder=true", nil, nil, writeOnlyPrincipal("mallory"))
	if w.Code != 201 {
		t.Fatalf("self = %d (%s)", w.Code, w.Body.String())
	}
	// Foreign prefix that is neither self nor a member org → 403
	// (the #346 close of the legacy-open unclaimed-prefix shape).
	w = f.do("PUT", "/free/gated?placeholder=true", nil, nil, writeOnlyPrincipal("mallory@example.com"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("foreign prefix = %d (%s), want 403", w.Code, w.Body.String())
	}
	// Host admin bypasses the admission rule.
	w = f.do("PUT", "/acme/admin?placeholder=true", nil, nil, writerPrincipal("root@example.com"))
	if w.Code != 201 {
		t.Fatalf("admin = %d (%s), want 201", w.Code, w.Body.String())
	}
	// Probe error → 503, never 403.
	g.err = errors.New("store down")
	w = f.do("PUT", "/acme/gated2?placeholder=true", nil, nil, writeOnlyPrincipal("mallory@example.com"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("probe error = %d (%s)", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("503 must carry Retry-After")
	}
}

// --- POST /api/v1/repos twin --------------------------------------------------

func postCreate(f *fixture, lane, payload, principal string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if payload != "" {
		body = strings.NewReader(payload)
	} else {
		body = strings.NewReader("")
	}
	p := writerPrincipal(principal)
	return f.do("POST", lane, body, map[string]string{"Content-Type": "application/json"}, p)
}

func TestPostReposCreatesPlaceholderBothLanes(t *testing.T) {
	for _, lane := range []string{"/api/v1/repos", "/api-browser/v1/repos"} {
		f, _, _ := placeholderFixture(t)
		ch := &CreateHandler{Env: f.env}
		r := httptest.NewRequest("POST", lane, strings.NewReader(`{"owner":"acme","name":"via-post"}`))
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(WithPrincipal(r.Context(), *writerPrincipal("alice@example.com")))
		w := httptest.NewRecorder()
		if !ch.Handle(w, r) {
			t.Fatalf("%s: not handled", lane)
		}
		if w.Code != 201 {
			t.Fatalf("%s = %d (%s)", lane, w.Code, w.Body.String())
		}
		if w.Header().Get("Location") == "" {
			t.Fatalf("%s: missing Location", lane)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["placeholder"] != true || body["clone_url"] == nil {
			t.Fatalf("%s body: %v", lane, body)
		}
		raw, _, err := store.GetBytes(context.Background(), f.env.Store, "repos/acme/via-post/"+PlaceholderKeySuffix, store.GetOptions{})
		if err != nil || raw == nil {
			t.Fatalf("%s: sidecar missing", lane)
		}
	}
}

func TestPostReposValidation(t *testing.T) {
	f, _, _ := placeholderFixture(t)
	ch := &CreateHandler{Env: f.env}
	call := func(payload string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/repos", strings.NewReader(payload))
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(WithPrincipal(r.Context(), *writerPrincipal("a@b.c")))
		w := httptest.NewRecorder()
		ch.Handle(w, r)
		return w
	}
	// Missing name → 400 plain-text.
	if w := call(`{"owner":"acme"}`); w.Code != 400 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("missing name = %d (%s)", w.Code, w.Body.String())
	}
	// Bad owner charset → 400 plain-text.
	if w := call(`{"owner":".evil","name":"x"}`); w.Code != 400 {
		t.Fatalf("bad owner = %d (%s)", w.Code, w.Body.String())
	}
	// Bad object_format → 400.
	if w := call(`{"owner":"acme","name":"x","object_format":"bogus"}`); w.Code != 400 {
		t.Fatalf("bad format = %d", w.Code)
	}
	// Bad visibility → 400.
	if w := call(`{"owner":"acme","name":"x","visibility":"galaxy"}`); w.Code != 400 {
		t.Fatalf("bad visibility = %d", w.Code)
	} // .git suffix stripped.
	if w := call(`{"owner":"acme","name":"sfx.git"}`); w.Code != 201 {
		t.Fatalf("git suffix = %d (%s)", w.Code, w.Body.String())
	}
	// Non-POST on the route → 405 (still claimed).
	r := httptest.NewRequest("GET", "/api/v1/repos", nil)
	r = r.WithContext(WithPrincipal(r.Context(), *writerPrincipal("a@b.c")))
	w := httptest.NewRecorder()
	if !ch.Handle(w, r) || w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = claimed=%v code=%d", true, w.Code)
	}
	// Unrelated path → false (core mux answers).
	r = httptest.NewRequest("POST", "/api/v1/owners", nil)
	w = httptest.NewRecorder()
	if ch.Handle(w, r) {
		t.Fatal("unrelated path must not be claimed")
	}
}

func TestPostReposVisibilityThreaded(t *testing.T) {
	// The POST visibility toggle must reach the eager access default —
	// "private" must not silently materialize public (review on #218).
	f, _, b := placeholderFixture(t)
	ch := &CreateHandler{Env: f.env}
	call := func(payload string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/repos", strings.NewReader(payload))
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(WithPrincipal(r.Context(), *writerPrincipal("alice@example.com")))
		w := httptest.NewRecorder()
		ch.Handle(w, r)
		return w
	}
	if w := call(`{"owner":"acme","name":"priv","visibility":"private"}`); w.Code != 201 {
		t.Fatalf("private = %d (%s)", w.Code, w.Body.String())
	}
	if len(b.calls) != 1 || b.calls[0][3] != "private" {
		t.Fatalf("visibility not threaded to access boot: %v", b.calls)
	}
	f2, _, b2 := placeholderFixture(t)
	ch2 := &CreateHandler{Env: f2.env}
	r := httptest.NewRequest("POST", "/api/v1/repos", strings.NewReader(`{"owner":"acme","name":"pub"}`))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(WithPrincipal(r.Context(), *writerPrincipal("alice@example.com")))
	w := httptest.NewRecorder()
	ch2.Handle(w, r)
	if w.Code != 201 {
		t.Fatalf("default = %d (%s)", w.Code, w.Body.String())
	}
	if len(b2.calls) != 1 || b2.calls[0][3] != "" {
		t.Fatalf("default visibility must be empty (public default): %v", b2.calls)
	}
}

func TestPostReposIdempotencyAndConflict(t *testing.T) {
	f, _, _ := placeholderFixture(t)
	f.view.summaries["acme/dup"] = SummaryData{}
	ch := &CreateHandler{Env: f.env}
	call := func(payload, principal string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/repos", strings.NewReader(payload))
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(WithPrincipal(r.Context(), *writerPrincipal(principal)))
		w := httptest.NewRecorder()
		ch.Handle(w, r)
		return w
	}
	if w := call(`{"owner":"acme","name":"dup"}`, "alice@example.com"); w.Code != 201 {
		t.Fatalf("first = %d", w.Code)
	}
	w := call(`{"owner":"acme","name":"dup"}`, "alice@example.com")
	if w.Code != 200 {
		t.Fatalf("same principal = %d (%s)", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["already"] != true {
		t.Fatalf("already flag: %v", body)
	}
	w = call(`{"owner":"acme","name":"dup"}`, "bob@example.com")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "/acme/dup") {
		t.Fatalf("diff principal = %d (%q)", w.Code, w.Body.String())
	}
	// Same principal re-create AFTER the first push (now real) → 409:
	// it is a repo (§6).
	f.view.summaries["acme/dup"] = SummaryData{Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1}
	w = call(`{"owner":"acme","name":"dup"}`, "alice@example.com")
	if w.Code != http.StatusConflict {
		t.Fatalf("re-create of real repo = %d (%s), want 409", w.Code, w.Body.String())
	}
}

func TestPostReposOrgGate(t *testing.T) {
	f, g, _ := placeholderFixture(t)
	g.exists["acme"] = true
	g.members["acme"] = map[string]bool{"alice@example.com": true}
	ch := &CreateHandler{Env: f.env}
	call := func(p *auth.Principal, name string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/repos", strings.NewReader(`{"owner":"acme","name":"`+name+`"}`))
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(WithPrincipal(r.Context(), *p))
		w := httptest.NewRecorder()
		ch.Handle(w, r)
		return w
	}
	if w := call(writeOnlyPrincipal("mallory@example.com"), "g"); w.Code != http.StatusForbidden {
		t.Fatalf("non-member = %d", w.Code)
	}
	if w := call(writeOnlyPrincipal("alice@example.com"), "g"); w.Code != 201 {
		t.Fatalf("member = %d (%s)", w.Code, w.Body.String())
	}
	if w := call(writerPrincipal("root@example.com"), "g-admin"); w.Code != 201 {
		t.Fatalf("admin bypass = %d (%s)", w.Code, w.Body.String())
	}
}

// --- summary projection (R1 B1) ------------------------------------------------

func TestSummaryPlaceholderProjection(t *testing.T) {
	f, _, _ := placeholderFixture(t)
	id := git.RepoId{Owner: "acme", Name: "ph"}
	f.view.summaries[id.String()] = SummaryData{Health: RepoHealthEmpty}
	// No sidecar → placeholder null.
	w := f.do("GET", "/acme/ph/api", nil, nil, writerPrincipal("a@b.c"))
	if w.Code != 200 {
		t.Fatalf("summary = %d", w.Code)
	}
	var body map[string]any
	decodeJSON(t, w, &body)
	if _, ok := body["placeholder"]; ok {
		t.Fatalf("no sidecar → no projection: %v", body)
	}
	// With sidecar → projection present.
	raw, _ := json.Marshal(PlaceholderDoc{Version: 1, CreatedBy: "alice@example.com", CreatedAt: "2026-09-08T00:00:00Z", ObjectFormat: "sha1"})
	if _, err := f.env.Store.Put(context.Background(), "repos/acme/ph/"+PlaceholderKeySuffix, store.PutBody{Bytes: raw}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	w = f.do("GET", "/acme/ph/api", nil, nil, writerPrincipal("a@b.c"))
	decodeJSON(t, w, &body)
	ph, ok := body["placeholder"].(map[string]any)
	if !ok || ph["created_by"] != "alice@example.com" || ph["created_at"] != "2026-09-08T00:00:00Z" {
		t.Fatalf("projection: %v", body)
	}
	// Real repo + stale marker → projection ABSENT (refs==0 && marker rule).
	f.view.summaries["acme/real"] = SummaryData{Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1, Health: RepoHealthHealthy}
	raw2, _ := json.Marshal(PlaceholderDoc{Version: 1, CreatedBy: "alice@example.com", CreatedAt: "2026-09-08T00:00:00Z", ObjectFormat: "sha1"})
	if _, err := f.env.Store.Put(context.Background(), "repos/acme/real/"+PlaceholderKeySuffix, store.PutBody{Bytes: raw2}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	w = f.do("GET", "/acme/real/api", nil, nil, writerPrincipal("a@b.c"))
	var real map[string]any
	decodeJSON(t, w, &real)
	if _, ok := real["placeholder"]; ok {
		t.Fatalf("stale marker on real repo must not project: %v", real)
	}
}

// --- discovery (RegisterExposed) -------------------------------------------------

func TestCreateExposedTemplateRegistered(t *testing.T) {
	RegisterExposed(ExposedTemplatesCreate...)
	eps := discoveryEndpoints()
	found := false
	for _, e := range eps {
		if e == "/api/v1/repos" {
			found = true
		}
	}
	if !found {
		t.Fatalf("discovery missing /api/v1/repos: %v", eps)
	}
}
