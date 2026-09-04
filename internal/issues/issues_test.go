package issues

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// --- test principals ----------------------------------------------------------

var (
	adminP  = auth.Principal{Name: "admin@example.com", Write: true, Admin: true}
	writerP = auth.Principal{Name: "writer@example.com", Write: true}
	aliceP  = auth.Principal{Name: "alice@example.com"}
	bobP    = auth.Principal{Name: "bob@example.com"}
	janeP   = auth.Principal{Name: "jane@example.com"}
	anonP   = auth.Anonymous()
)

// --- fake RoleService ------------------------------------------------------------

// fakeRoles grants per-repo per-principal roles; visibility defaults
// public unless set private. Host flags (admin/write) short-circuit like
// the real resolution (P6 step 3). unavail forces identity-down (503).
type fakeRoles struct {
	roles   map[string]map[string]string // "o/r" → principal → role
	private map[string]bool
	unavail bool
}

func newFakeRoles() *fakeRoles {
	return &fakeRoles{roles: map[string]map[string]string{}, private: map[string]bool{}}
}

func (f *fakeRoles) grant(owner, repo, principal, role string) {
	k := owner + "/" + repo
	if f.roles[k] == nil {
		f.roles[k] = map[string]string{}
	}
	f.roles[k][normPrincipal(principal)] = role
}

func (f *fakeRoles) roleFor(owner, repo string, p auth.Principal) string {
	if p.Admin {
		return string(identity.RoleAdmin)
	}
	if p.Write {
		return string(identity.RoleWrite)
	}
	if p.Anonymous {
		return ""
	}
	return f.roles[owner+"/"+repo][normPrincipal(p.Name)]
}

func (f *fakeRoles) Resolve(ctx context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc) {
	return identity.Role(f.roleFor(owner, repo, p)), nil
}

func (f *fakeRoles) CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError {
	if f.unavail {
		return &auth.AuthError{Kind: auth.ErrUnavailable, Why: "identity down"}
	}
	if p.Admin || p.Write {
		return nil
	}
	if roleRank(f.roleFor(owner, repo, p)) >= roleRank("read") {
		return nil
	}
	if p.Anonymous {
		if !f.private[owner+"/"+repo] {
			return nil
		}
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	if !f.private[owner+"/"+repo] {
		return nil
	}
	return &auth.AuthError{Kind: auth.ErrForbidden, Why: "read access required"}
}

// --- harness ----------------------------------------------------------------------

func testService(roles RoleService) *Service {
	s := New(store.NewMemory(), roles)
	s.Now = func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }
	return s
}

func testHandler(s *Service, p auth.Principal) *Handler {
	return &Handler{Svc: s, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return p, nil
	}}
}

func doReq(h *Handler, method, target, body string) *httptest.ResponseRecorder {
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func reqCtx() context.Context { return context.Background() }

// mustCreate creates one issue or fails the test.
func mustCreate(t *testing.T, s *Service, owner, repo string, p auth.Principal, title, body string) *Thread {
	t.Helper()
	th, _, err := s.CreateIssue(reqCtx(), owner, repo, p, title, body)
	if err != nil {
		t.Fatalf("CreateIssue(%q) = %v", title, err)
	}
	return th
}

// grantTriage gives alice triage (the common moderator in tests).
func grantTriage(f *fakeRoles, owner, repo string) {
	f.grant(owner, repo, "alice@example.com", "triage")
}
