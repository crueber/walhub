// fakes_test.go — shared test doubles for internal/notify (memory store,
// fake roles/profiles/teams, op counting). Package notify (white-box:
// exercises unexported buses + tables directly where the wire cannot).
package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// --- fakes -------------------------------------------------------------------

type fakeRoles struct {
	mu    sync.Mutex
	roles map[string]string // "owner/repo\x00principal" → role
}

func newFakeRoles() *fakeRoles { return &fakeRoles{roles: map[string]string{}} }

func (f *fakeRoles) grant(owner, repo, principal, role string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roles[owner+"/"+repo+"\x00"+principal] = role
}

func (f *fakeRoles) Resolve(_ context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.roles[owner+"/"+repo+"\x00"+p.Name]; ok {
		return identity.Role(r), nil
	}
	return "", nil
}

func (f *fakeRoles) CheckRead(_ context.Context, _, _ string, p auth.Principal) *auth.AuthError {
	if p.Anonymous {
		return &auth.AuthError{Kind: auth.ErrInvalid, Why: "unauthorized"}
	}
	return nil
}

type fakeProfiles struct {
	mu   sync.Mutex
	have map[string]bool
	err  error
}

func (f *fakeProfiles) GetProfile(_ context.Context, principal string) (*identity.Profile, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.have[principal] {
		return &identity.Profile{Principal: principal}, nil
	}
	return nil, nil
}

type fakeTeams struct {
	mu      sync.Mutex
	members map[string][]string // "org/slug" → members
}

func (f *fakeTeams) GetTeam(_ context.Context, org, slug string) (*identity.Team, store.Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.members[org+"/"+slug]
	if !ok {
		return nil, "", nil
	}
	return &identity.Team{Org: org, Slug: slug, Members: append([]string(nil), m...)}, "v1", nil
}

// --- harness -------------------------------------------------------------------

type harness struct {
	svc      *Service
	handler  *Handler
	roles    *fakeRoles
	profiles *fakeProfiles
	teams    *fakeTeams
	now      time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st := store.NewMemory()
	roles := newFakeRoles()
	profiles := &fakeProfiles{have: map[string]bool{}}
	teams := &fakeTeams{members: map[string][]string{}}
	svc := New(st, roles)
	svc.Profiles = profiles
	svc.Teams = teams
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return now }
	h := &Handler{Svc: svc}
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return principalFor(r), nil
	}
	return &harness{svc: svc, handler: h, roles: roles, profiles: profiles, teams: teams, now: now}
}

// principalFor reads the test principal from the X-Test-Principal header
// ("anonymous" or absent → anonymous; "admin" suffix handling is explicit
// per test via roles/principal flags below).
func principalFor(r *http.Request) auth.Principal {
	name := r.Header.Get("X-Test-Principal")
	if name == "" || name == "anonymous" {
		return auth.Anonymous()
	}
	p := auth.Principal{Name: name}
	if r.Header.Get("X-Test-Admin") == "1" {
		p.Admin = true
	}
	return p
}

func (x *harness) addProfile(names ...string) {
	for _, n := range names {
		x.profiles.have[n] = true
	}
}

// writeThread seeds the shared P3 header projection (title + author);
// idempotent (re-seeds are no-ops).
func (x *harness) writeThread(t *testing.T, owner, repo string, num int, title, author string) {
	t.Helper()
	key := threadKey(owner, repo, num)
	raw, _ := json.Marshal(map[string]any{"title": title, "author": author})
	if _, err := store.PutBytes(context.Background(), x.svc.Store, key, raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		if !store.IsPreconditionFailed(err) {
			t.Fatal(err)
		}
	}
}

func ctx() context.Context { return context.Background() }

// mustEncode is the test-only encode for fixed shapes (production encode
// returns an error since issue #98; fixtures fail fast instead of
// threading errors through every seed site).
func mustEncode(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := encode(v)
	if err != nil {
		t.Fatalf("mustEncode: %v", err)
	}
	return raw
}

// seedRepo marks owner/repo as existing: the manifest probe is the
// repo-existence signal (watch/tray/retention paths gate on it), and the
// body is never read.
func seedRepo(t *testing.T, x *harness, owner, repo string) {
	t.Helper()
	if _, err := store.PutBytes(ctx(), x.svc.Store, manifestKey(owner, repo), []byte("manifest"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"}); err != nil {
		if !store.IsPreconditionFailed(err) {
			t.Fatal(err)
		}
	}
}
