package checks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// Edge coverage for roles, reads, and the report/auth matrix —
// table-driven over the branches statusFor and the handlers map.

// errStore injects failures per op (nil = delegate to memory).
type errStore struct {
	inner   store.ObjectStore
	getErr  error
	putErr  error
	put412  bool
	listErr error
}

func (f *errStore) Backend() string { return f.inner.Backend() }

func (f *errStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.inner.Get(ctx, key, opts)
}

func (f *errStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	return f.inner.Head(ctx, key)
}

func (f *errStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if f.put412 {
		return store.ObjectMeta{}, store.NewPrecondition(key, "v2")
	}
	if f.putErr != nil {
		return store.ObjectMeta{}, f.putErr
	}
	return f.inner.Put(ctx, key, body, opts)
}

func (f *errStore) Delete(ctx context.Context, key string, v store.Version) error {
	return f.inner.Delete(ctx, key, v)
}

func (f *errStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	if f.listErr != nil {
		return f.listErr
	}
	return f.inner.List(ctx, prefix, startAfter, fn)
}

func (f *errStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	return f.inner.ListPrefixes(ctx, prefix, fn)
}

func (f *errStore) SignedGetURL(ctx context.Context, key string, ttl time.Duration) (*string, error) {
	return f.inner.SignedGetURL(ctx, key, ttl)
}

func (f *errStore) AccelTarget(ctx context.Context, key string) (*store.AccelTarget, error) {
	return f.inner.AccelTarget(ctx, key)
}

func (f *errStore) SupportsCompose() bool { return f.inner.SupportsCompose() }
func (f *errStore) ComposeIsNative() bool { return f.inner.ComposeIsNative() }

func (f *errStore) Compose(ctx context.Context, dst string, sources []string, opts store.PutOptions) (store.ObjectMeta, error) {
	return f.inner.Compose(ctx, dst, sources, opts)
}

func TestCoverRoleLadder(t *testing.T) {
	for role, want := range map[string]int{"read": 1, "triage": 2, "write": 3, "maintain": 4, "admin": 5} {
		if got := roleRank(role); got != want {
			t.Fatalf("rank %q = %d", role, got)
		}
		if got := roleRank(strings.ToUpper(role)); got != want {
			t.Fatalf("upper rank %q = %d", role, got)
		}
	}
	// Nil Roles: flags decide.
	svc := New(store.NewMemory(), nil)
	if got := svc.roleOf(ctx(), "o", "r", auth.Principal{Name: "a", Admin: true}); got != "admin" {
		t.Fatalf("nil admin = %q", got)
	}
	if got := svc.roleOf(ctx(), "o", "r", auth.Principal{Name: "w", Write: true}); got != "write" {
		t.Fatalf("nil write = %q", got)
	}
	if got := svc.roleOf(ctx(), "o", "r", anon()); got != "" {
		t.Fatalf("nil anon = %q", got)
	}
	if got := svc.roleOf(ctx(), "o", "r", reader()); got != "read" {
		t.Fatalf("nil authed = %q", got)
	}
	// Wired roles delegate (FakeRoles returns read for the unbound).
	e := newTestEnv()
	if got := e.svc.roleOf(ctx(), "o", "r", reader()); got != "read" {
		t.Fatalf("default = %q", got)
	}
	if _, ok := interface{}(e.roles).(RoleService); !ok {
		t.Fatal("fake must satisfy the seam")
	}
	var _ identity.Role = identity.RoleRead
}

func TestCoverRequireRead(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(70)
	if err := e.svc.requireRead(ctx(), "o", "r", admin()); err != nil {
		t.Fatalf("admin: %v", err)
	}
	if err := e.svc.requireRead(ctx(), "o", "r", writer()); err != nil {
		t.Fatalf("write flag: %v", err)
	}
	// Nil Roles: anon 401, authed pass.
	bare := New(store.NewMemory(), nil)
	if err := bare.requireRead(ctx(), "o", "r", anon()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("nil anon: %v", err)
	}
	if err := bare.requireRead(ctx(), "o", "r", reader()); err != nil {
		t.Fatalf("nil authed: %v", err)
	}
	// Private repo: anon 401, stranger 403.
	e.roles.Public = false
	if _, err := e.svc.GetStatuses(ctx(), "o", "r", sha, anon()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("private anon: %v", err)
	}
	if _, err := e.svc.Combined(ctx(), "o", "r", sha, auth.Principal{Name: "stranger@example.com"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("private stranger: %v", err)
	}
	if _, err := e.svc.ListChecks(ctx(), "o", "r", auth.Principal{Name: "stranger@example.com"}, "", 10); !errors.Is(err, ErrForbidden) {
		t.Fatalf("private list: %v", err)
	}
	// Bad sha at the service layer (the handler maps it to 400).
	if _, err := e.svc.GetStatuses(ctx(), "o", "r", "zzz", reader()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad sha: %v", err)
	}
	if _, err := e.svc.Combined(ctx(), "o", "r", "zzz", reader()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad sha: %v", err)
	}
	// Identity outage maps through.
	outage := &FakeOutage{kind: auth.ErrUnavailable}
	e2 := newTestEnv()
	e2.svc.Roles = outage
	if err := e2.svc.requireRead(ctx(), "o", "r", reader()); err == nil || !strings.Contains(err.Error(), "identity unavailable") {
		t.Fatalf("outage: %v", err)
	}
	outage.kind = auth.ErrForbidden
	if err := e2.svc.requireRead(ctx(), "o", "r", reader()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("forbidden: %v", err)
	}
	outage.kind = auth.ErrInvalid
	if err := e2.svc.requireRead(ctx(), "o", "r", reader()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("invalid: %v", err)
	}
}

// FakeOutage scripts CheckRead failures.
type FakeOutage struct{ kind auth.AuthErrorKind }

func (f *FakeOutage) Resolve(_ context.Context, _, _ string, p auth.Principal) (identity.Role, *identity.AccessDoc) {
	return identity.RoleRead, nil
}

func (f *FakeOutage) CheckRead(_ context.Context, _, _ string, _ auth.Principal) *auth.AuthError {
	return &auth.AuthError{Kind: f.kind, Why: "outage"}
}

func TestCoverReportEdge(t *testing.T) {
	// Scopeless token ⇒ 403 (valid credential, no capability).
	e := newTestEnv()
	sha := hexSHA(71)
	e.knowSHA(sha)
	raw := `{"id":"deadbeef","name":"x","token_hash":"` + hashSecret("s") + `","scopes":[],"created_by":"a","created_at":"2026-09-04T12:00:00Z","version":1}`
	if _, err := store.PutBytes(ctx(), e.store, TokenKey("o", "r", "deadbeef"), []byte(raw),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.ReportStatus(ctx(), "o", "r", sha, ci("deadbeef"), "s", ReportInput{Context: "ci", State: StateSuccess}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("scopeless: %v", err)
	}
	// Permanent CAS contention ⇒ 503 after the bounded retries.
	e2 := newTestEnv()
	e2.svc.Store = &errStore{inner: e2.store, put412: true}
	e2.knowSHA(sha)
	if _, err := e2.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{Context: "ci", State: StateSuccess}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("contention: %v", err)
	}
	// Non-CAS store failure passes through.
	e3 := newTestEnv()
	e3.svc.Store = &errStore{inner: e3.store, putErr: errors.New("disk on fire")}
	e3.knowSHA(sha)
	if _, err := e3.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{Context: "ci", State: StateSuccess}); err == nil {
		t.Fatal("store failure accepted")
	}
	// Anonymous token CRUD ⇒ 401.
	if _, err := e.svc.CreateToken(ctx(), "o", "r", anon(), "x", nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anon mint: %v", err)
	}
	if _, err := e.svc.ListTokens(ctx(), "o", "r", anon()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anon list: %v", err)
	}
	if err := e.svc.RevokeToken(ctx(), "o", "r", "deadbeef", anon()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anon revoke: %v", err)
	}
}
