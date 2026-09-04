package identity

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

func testClock() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) }

// testService builds a Service over the memory store with a fixed clock.
func testService() *Service {
	s := New(store.NewMemory(), config.Defaults())
	s.Now = testClock
	return s
}

// authed returns an Authenticator yielding p.
func authed(p auth.Principal) Authenticator {
	return func(r *http.Request) (auth.Principal, *auth.AuthError) { return p, nil }
}

// testHandler builds a Handler with canned auth.
func testHandler(s *Service, p auth.Principal) *Handler {
	return &Handler{Svc: s, Auth: authed(p)}
}

var (
	alice    = auth.Principal{Name: "alice@example.com", Write: true}
	bob      = auth.Principal{Name: "bob@example.com", Write: true}
	carol    = auth.Principal{Name: "carol@example.com"}
	dave     = auth.Principal{Name: "dave@example.com"}
	writer   = auth.Principal{Name: "w@example.com", Write: true}
	stranger = auth.Principal{Name: "stranger@example.com"}
	admin    = auth.Principal{Name: "root@example.com", Write: true, Admin: true}
	anon     = auth.Anonymous()
	noneP    = auth.None()
)

func daveP() auth.Principal { return dave }

func patP() auth.Principal { return auth.Principal{Name: "pat@example.com"} }

func finP() auth.Principal { return auth.Principal{Name: "fin@example.com"} }

func authPrincipal(name string) auth.Principal { return auth.Principal{Name: name} }

// doReq issues one request against h and returns the recorder.
func doReq(h *Handler, method, target string, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// seedOrg creates org acme owned by alice with team platform {bob}.
func seedOrg(t *testing.T, s *Service) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := s.SetMember(ctx, "acme", "bob@example.com", OrgMember); err != nil {
		t.Fatalf("SetMember: %v", err)
	}
	if _, err := s.CreateTeam(ctx, "acme", "platform", "Platform", ""); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if _, err := s.SetTeamMember(ctx, "acme", "platform", "bob@example.com"); err != nil {
		t.Fatalf("SetTeamMember: %v", err)
	}
}

// errStore injects store errors for branch coverage.
type errStore struct {
	store.ObjectStore
	getErr  error
	putErr  error
	delErr  error
	listErr error
}

func (e *errStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if e.getErr != nil {
		return nil, e.getErr
	}
	return e.ObjectStore.Get(ctx, key, opts)
}

func (e *errStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if e.putErr != nil {
		return store.ObjectMeta{}, e.putErr
	}
	return e.ObjectStore.Put(ctx, key, body, opts)
}

func (e *errStore) Delete(ctx context.Context, key string, v store.Version) error {
	if e.delErr != nil {
		return e.delErr
	}
	return e.ObjectStore.Delete(ctx, key, v)
}

func (e *errStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	if e.listErr != nil {
		return e.listErr
	}
	return e.ObjectStore.List(ctx, prefix, startAfter, fn)
}

func (e *errStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	if e.listErr != nil {
		return e.listErr
	}
	return e.ObjectStore.ListPrefixes(ctx, prefix, fn)
}

var errBoom = errors.New("boom")

// reqCtx is the background context for service calls.
func reqCtx() context.Context { return context.Background() }

// reqCtxT names the context type for stub listers.
type reqCtxT = context.Context
