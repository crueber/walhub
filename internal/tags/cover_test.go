package tags

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// Gap coverage: default constructors, nil-Auth fallback, undecodable
// segments, writer/decoder failure paths, and the roleOf ladder.

func TestNewSubprocessGitDefaults(t *testing.T) {
	g := NewSubprocessGit("")
	if g.Binary != "git" {
		t.Fatalf("Binary = %q", g.Binary)
	}
	if g.Pool == nil || g.Timeout <= 0 {
		t.Fatal("pool/timeout must default")
	}
	if p := newGitPool(0); cap(p.sem) <= 0 {
		t.Fatal("pool capacity must default positive")
	}
}

func TestPrincipalNilAuthFallsBackAnonymous(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	h := &Handler{Svc: x.svc} // no Auth → anonymous
	req := httptest.NewRequest("POST", "/o/r/api/tags", strings.NewReader(`{"name":"v1","sha":"`+testSHA+`"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("= %d (%q), want 401", w.Code, w.Body.String())
	}
	if got := w.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("401 must carry WWW-Authenticate")
	}
}

func TestDecodeSegmentUndecodable(t *testing.T) {
	if got := decodeSegment("%zz"); got != "%zz" {
		t.Fatalf("= %q, want verbatim", got)
	}
	if got := decodeSegment("v1"); got != "v1" {
		t.Fatalf("= %q", got)
	}
}

func TestWriteJSONEncodeError(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, 200, func() {}) // funcs do not marshal
	if w.Code != 500 {
		t.Fatalf("= %d, want 500", w.Code)
	}
}

type errReader struct{ err error }

func (r *errReader) Read(_ []byte) (int, error) { return 0, r.err }
func (r *errReader) Close() error               { return nil }

func TestDecodeStrictUnreadable(t *testing.T) {
	req := httptest.NewRequest("POST", "/o/r/api/tags", nil)
	req.Body = &errReader{err: errors.New("boom")}
	w := httptest.NewRecorder()
	var in CreateInput
	if decodeStrict(w, req, 4096, createTagFields, &in) {
		t.Fatal("unreadable body must fail")
	}
	if w.Code != 400 {
		t.Fatalf("= %d, want 400", w.Code)
	}
}

func TestRoleOfLadder(t *testing.T) {
	x := newHarness(t)
	cases := []struct {
		p    auth.Principal
		want string
	}{
		{auth.Principal{Name: "root", Admin: true}, "admin"},
		{auth.Principal{Name: "w", Write: true}, "write"},
		{auth.Anonymous(), ""},
		{auth.Principal{Name: "bob"}, "read"},
	}
	x.svc.Roles = nil
	for _, c := range cases {
		if got := x.svc.roleOf(ctx(), "o", "r", c.p); got != c.want {
			t.Errorf("roleOf(%+v) = %q, want %q", c.p, got, c.want)
		}
	}
	x2 := newHarness(t)
	x2.roles.grant("o", "r", "jane", "maintain")
	if got := x2.svc.roleOf(ctx(), "o", "r", writer()); got != "maintain" {
		t.Fatalf("resolved = %q", got)
	}
}
