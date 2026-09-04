package social

import (
	"context"
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

// --- harness -------------------------------------------------------------------

type harness struct {
	svc     *Service
	handler *Handler
	roles   *fakeRoles
	now     time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st := store.NewMemory()
	roles := newFakeRoles()
	svc := New(st, roles)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return now }
	h := &Handler{Svc: svc}
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return principalFor(r), nil
	}
	return &harness{svc: svc, handler: h, roles: roles, now: now}
}

func principalFor(r *http.Request) auth.Principal {
	name := r.Header.Get("X-Test-Principal")
	if name == "" || name == "anonymous" {
		return auth.Anonymous()
	}
	return auth.Principal{Name: name}
}

func ctx() context.Context { return context.Background() }

func jane() auth.Principal { return auth.Principal{Name: "jane"} }

func seedSocialKey(t *testing.T, x *harness, key, raw string) {
	t.Helper()
	// Overwrite whatever is there (delete-then-create: the store is
	// Create-only for absent keys).
	_ = x.svc.Store.Delete(ctx(), key, "")
	if _, err := store.PutBytes(ctx(), x.svc.Store, key, []byte(raw),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
}

// seedRepo marks owner/repo as existing: the manifest probe is the
// repo-existence signal (star/count/list paths gate on it), and the body
// is never read.
func seedRepo(t *testing.T, x *harness, owner, repo string) {
	t.Helper()
	if _, err := store.PutBytes(ctx(), x.svc.Store, manifestKey(owner, repo), []byte("manifest"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"}); err != nil {
		if !store.IsPreconditionFailed(err) {
			t.Fatal(err)
		}
	}
}

// seedWatchRecord writes a 06-shaped watch record (07 reads it; 06 writes it).
func seedWatchRecord(t *testing.T, x *harness, principal, owner, repo string) {
	t.Helper()
	raw := `{"repo":"` + owner + `/` + repo + `","watched_at":"2026-09-04T12:00:00Z"}`
	if err := x.svc.putCreate(ctx(), WatchingKey(principal, owner, repo), []byte(raw)); err != nil {
		t.Fatal(err)
	}
}
