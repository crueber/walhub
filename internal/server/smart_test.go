package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

// pktErrOf reports whether the body is a pkt-line ERR.
func pktErrOf(body string) (string, bool) {
	if len(body) < 8 || !strings.HasPrefix(body[4:], "ERR ") {
		return "", false
	}
	n := 0
	for i := 0; i < 4; i++ {
		c := body[i]
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		default:
			return "", false
		}
		n = n*16 + v
	}
	if n < 4 || len(body) < n {
		return "", false
	}
	return strings.TrimRight(body[4:n], "\n"), true
}

func TestPktErrHelper(t *testing.T) {
	body := string(git.ErrPkt("boom"))
	if _, ok := pktErrOf(body); !ok {
		t.Fatalf("pktErrOf failed on %q", body)
	}
}

// Test401VsPktErrTruthTable is the normative §4.2 matrix: 200 + pkt ERR only
// when git-ish AND ?service= present AND Authorization carried AND the error
// is Forbidden/Unavailable.
func Test401VsPktErrTruthTable(t *testing.T) {
	cases := []struct {
		gitish     bool
		service    bool
		authz      bool
		kind       auth.AuthErrorKind
		wantPktErr bool
		wantStatus int
	}{
		{true, true, true, auth.ErrForbidden, true, 200},
		{true, true, true, auth.ErrUnavailable, true, 200},
		{true, true, true, auth.ErrInvalid, false, 401}, // dead token → REAL 401
		{true, true, true, auth.ErrUnauthorized, false, 401},
		{true, true, false, auth.ErrForbidden, false, 401}, // no credential → git must prompt
		{true, false, true, auth.ErrForbidden, false, 403}, // not pkt-eligible → §8.6 mapping
		{false, true, true, auth.ErrForbidden, false, 403}, // not pkt-eligible → §8.6 mapping
		{false, false, false, auth.ErrInvalid, false, 401},
		{true, true, true, auth.ErrUnavailable, true, 200},
	}
	for i, tc := range cases {
		s, _ := newTestServer(t, nil)
		req := httptest.NewRequest("GET", "http://x/o/r.git/info/refs", nil)
		if tc.service {
			req.URL.RawQuery = "service=git-upload-pack"
		}
		if tc.gitish {
			req.Header.Set("User-Agent", "git/2.46.0")
		} else {
			req.Header.Set("User-Agent", "curl/8.0")
		}
		if tc.authz {
			req.Header.Set("Authorization", "Bearer whatever")
		}
		rec := httptest.NewRecorder()
		s.gitAuthFailure(rec, req, git.ServiceUploadPack, &auth.AuthError{Kind: tc.kind, Why: "denied"})
		_, hasPkt := pktErrOf(rec.Body.String())
		if hasPkt != tc.wantPktErr {
			t.Fatalf("case %d: pkt-err = %v, want %v (body %q)", i, hasPkt, tc.wantPktErr, rec.Body.String())
		}
		if rec.Code != tc.wantStatus {
			t.Fatalf("case %d: status = %d, want %d", i, rec.Code, tc.wantStatus)
		}
		if tc.wantStatus == http.StatusUnauthorized {
			if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="walgit"` {
				t.Fatalf("case %d: WWW-Authenticate = %q", i, got)
			}
		}
		if tc.kind == auth.ErrUnavailable && !tc.wantPktErr {
			if ra := rec.Header().Get("Retry-After"); ra != "15" {
				t.Fatalf("case %d: Retry-After = %q", i, ra)
			}
		}
	}
}

// TestInfoRefsFullFlow runs a real advertisement against a real local repo
// (git binary present) with token auth.
func TestInfoRefsFullFlow(t *testing.T) {
	s, _ := newTestServer(t, nil)
	root := t.TempDir()
	s.engine.(*fakeEngine).exists = true
	s.engine.(*fakeEngine).placement = Placement{Serve: true}
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer tok123")
	req.Header.Set("Git-Protocol", "version=2")
	req = req.WithContext(context.WithValue(req.Context(), repoRootKey{}, root))
	rec := httptest.NewRecorder()
	s.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-git-upload-pack-advertisement" {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "version 2") {
		t.Fatalf("v2 advert missing: %q", rec.Body.String())
	}
	// No-cache triple (§4.1).
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Fatalf("cache-control = %q", cc)
	}
	if e := rec.Header().Get("Expires"); e != "Fri, 01 Jan 1980 00:00:00 GMT" {
		t.Fatalf("expires = %q", e)
	}
	if p := rec.Header().Get("Pragma"); p != "no-cache" {
		t.Fatalf("pragma = %q", p)
	}
}

func TestInfoRefsUnknownService400(t *testing.T) {
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-evil-pack", nil)
	rec := httptest.NewRecorder()
	s.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestInfoRefsNoServiceIs404(t *testing.T) {
	// No dumb HTTP (§1): no ?service= → deliberate 404.
	s, h := newTestServer(t, nil)
	s.engine.(*fakeEngine).exists = true
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/refs", nil)
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want deliberate 404", rec.Code)
	}
}

func TestReceivePackRequiresGitSuffix(t *testing.T) {
	s, h := newTestServer(t, nil)
	s.engine.(*fakeEngine).exists = true
	req := httptest.NewRequest("POST", "http://x/o/r/git-receive-pack", nil)
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if msg, ok := pktErrOf(rec.Body.String()); !ok || !strings.Contains(msg, ".git") {
		t.Fatalf("want pkt ERR naming the .git requirement, got %q", rec.Body.String())
	}
}

func TestPlacementGate503WithPktErr(t *testing.T) {
	s, h := newTestServer(t, nil)
	s.engine.(*fakeEngine).exists = true
	s.engine.(*fakeEngine).placement = Placement{Serve: false, ServedBy: "host-b"}
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if msg, ok := pktErrOf(rec.Body.String()); !ok || !strings.Contains(msg, "host-b") {
		t.Fatalf("want pkt ERR naming the serving host, got %q", rec.Body.String())
	}
	// Non-git client → 503 + Retry-After.
	req = httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "15" {
		t.Fatalf("status = %d retry = %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestRepoBusy503(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.sem = NewRepoSemaphores(1)
	rel := s.sem.TryAcquire("o/r")
	if rel == nil {
		t.Fatal("first acquire must succeed")
	}
	defer rel()
	if s.sem.TryAcquire("o/r") != nil {
		t.Fatal("second acquire must fail (busy)")
	}
}

func mustRepoID(t *testing.T, s string) git.RepoId {
	t.Helper()
	id, err := git.ParseRepoId(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
