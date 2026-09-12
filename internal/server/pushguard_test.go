package server

// pushguard_test.go — the Forgejo #347 push-guardrail matrix on both
// transports: the repo-scoped write rule (owner / org-attached /
// explicitly bound / admin) at receive-pack dispatch, and the #346
// admission for auto-create, against a REAL identity.Service (real P6
// resolution, no stub gates) with token-mode auth.
//
// Cell legend: alice = org owner (write flag, NO host admin — the bypass
// must not decide her cell); bob = org member + team write binding (host
// write flag set, proving the flag is not the gate); carol = read-only
// token with an explicit write binding on acme/r2 only (no host flag at
// all); mallory = host-write-only foreigner (THE fix: must be denied);
// root = host admin (bypass); solo = slug-namespace self; anon = no
// credential (401).

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/sshd"
)

var errProbeBoom = errors.New("bucket down")

type pushGuardFixture struct {
	srv   *Server
	ident *identity.Service
	eng   *fakeEngine
}

func pushGuardSetup(t *testing.T, exists bool) *pushGuardFixture {
	t.Helper()
	st := newFakeStore()
	cfg := config.Defaults()
	cfg.Server.Auth.Mode = "token"
	cfg.Server.Auth.Tokens = []config.StaticToken{
		{Principal: "alice@example.com", Token: "tok-alice", Write: true},
		{Principal: "bob@example.com", Token: "tok-bob", Write: true},
		{Principal: "carol@example.com", Token: "tok-carol"},
		{Principal: "mallory@example.com", Token: "tok-mallory", Write: true},
		{Principal: "root@example.com", Token: "tok-root", Write: true, Admin: true},
		{Principal: "solo", Token: "tok-solo", Write: true},
	}
	cfg.Server.Auth.SessionSecret = "0123456789abcdef0123456789abcdef"
	ident := identity.New(st, cfg)
	ctx := context.Background()
	if _, err := ident.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := ident.SetMember(ctx, "acme", "bob@example.com", identity.OrgMember); err != nil {
		t.Fatal(err)
	}
	if _, err := ident.CreateTeam(ctx, "acme", "platform", "Platform", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ident.SetTeamMember(ctx, "acme", "platform", "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := ident.PutAccess(ctx, "acme", "r", "", identity.VisibilityPrivate,
		[]identity.AccessBinding{{Subject: "team:acme/platform", Role: identity.RoleWrite}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ident.PutAccess(ctx, "acme", "r2", "", identity.VisibilityPrivate,
		[]identity.AccessBinding{{Subject: "user:carol@example.com", Role: identity.RoleWrite}}); err != nil {
		t.Fatal(err)
	}
	eng := &fakeEngine{exists: exists, placement: Placement{Serve: true}}
	srv := New(Options{
		Config: cfg, Store: st, Engine: eng,
		DataDir: t.TempDir(), Log: testLogger(t), PushGate: ident,
	})
	return &pushGuardFixture{srv: srv, ident: ident, eng: eng}
}

func pushGuardReq(t *testing.T, method, target, token string, body io.Reader) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("User-Agent", "git/2.46.0")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req.WithContext(context.WithValue(req.Context(), repoRootKey{}, t.TempDir()))
}

// TestPushGuardDiscoveryMatrix covers the HTTP discovery gate
// (info/refs?service=git-receive-pack): allowed principals reach the
// advertisement; the host-write-only foreigner and the unbound reader get
// the named 403 (as pkt ERR under the §4.2 four conditions); anonymous
// gets a real 401.
func TestPushGuardDiscoveryMatrix(t *testing.T) {
	fx := pushGuardSetup(t, true)
	cases := []struct {
		name  string
		repo  string
		token string
		code  int
		body  string // required substring ("# service=" for allow)
		pkt   bool   // expect the pkt-ERR shape (denied-with-auth)
	}{
		{"org owner", "acme/r", "tok-alice", http.StatusOK, "# service=git-receive-pack", false},
		{"team-bound member", "acme/r", "tok-bob", http.StatusOK, "# service=git-receive-pack", false},
		{"bound reader w/o host flag", "acme/r2", "tok-carol", http.StatusOK, "# service=git-receive-pack", false},
		{"slug self", "solo/r", "tok-solo", http.StatusOK, "# service=git-receive-pack", false},
		{"host admin bypass", "acme/r", "tok-root", http.StatusOK, "# service=git-receive-pack", false},
		{"host-write-only foreigner", "acme/r", "tok-mallory", http.StatusOK, `"acme/r"`, true},
		{"unbound reader", "acme/r", "tok-carol", http.StatusOK, `"acme/r"`, true},
		{"anonymous", "acme/r", "", http.StatusUnauthorized, "authentication required", false},
	}
	for _, tc := range cases {
		req := pushGuardReq(t, "GET", "http://x/"+tc.repo+".git/info/refs?service=git-receive-pack", tc.token, nil)
		rec := httptest.NewRecorder()
		fx.srv.gitInfoRefs(rec, req, mustRepoID(t, tc.repo))
		if rec.Code != tc.code {
			t.Errorf("%s: code=%d want %d (%s)", tc.name, rec.Code, tc.code, rec.Body.String())
			continue
		}
		if tc.pkt {
			msg, ok := pktErrOf(rec.Body.String())
			if !ok || !strings.Contains(msg, tc.body) {
				t.Errorf("%s: want pkt ERR naming the repo, got %q", tc.name, rec.Body.String())
			}
			continue
		}
		if !strings.Contains(rec.Body.String(), tc.body) {
			t.Errorf("%s: body missing %q: %q", tc.name, tc.body, rec.Body.String())
		}
	}
}

// TestPushGuardAutoCreateDiscovery covers discovery on unborn repos:
// self and member-org pushes advertise empty; a foreign-owner squat dies
// with the #346 admission message; anonymous is 401.
func TestPushGuardAutoCreateDiscovery(t *testing.T) {
	fx := pushGuardSetup(t, false)
	cases := []struct {
		name  string
		repo  string
		token string
		code  int
		body  string
		pkt   bool
	}{
		{"self creates", "solo/newrepo", "tok-solo", http.StatusOK, "# service=git-receive-pack", false},
		{"member org creates", "acme/newrepo", "tok-bob", http.StatusOK, "# service=git-receive-pack", false},
		{"admin creates anywhere", "solo/hijack", "tok-root", http.StatusOK, "# service=git-receive-pack", false},
		{"foreign squat denied", "solo/hijack", "tok-mallory", http.StatusOK, "not permitted", true},
		{"anonymous denied", "solo/newrepo", "", http.StatusUnauthorized, "authentication required", false},
	}
	for _, tc := range cases {
		req := pushGuardReq(t, "GET", "http://x/"+tc.repo+".git/info/refs?service=git-receive-pack", tc.token, nil)
		rec := httptest.NewRecorder()
		fx.srv.gitInfoRefs(rec, req, mustRepoID(t, tc.repo))
		if rec.Code != tc.code {
			t.Errorf("%s: code=%d want %d (%s)", tc.name, rec.Code, tc.code, rec.Body.String())
			continue
		}
		if tc.pkt {
			msg, ok := pktErrOf(rec.Body.String())
			if !ok || !strings.Contains(msg, tc.body) {
				t.Errorf("%s: want pkt ERR with %q, got %q", tc.name, tc.body, rec.Body.String())
			}
			continue
		}
		if !strings.Contains(rec.Body.String(), tc.body) {
			t.Errorf("%s: body missing %q: %q", tc.name, tc.body, rec.Body.String())
		}
	}
}

// TestPushGuardBodyMatrix covers the POST body gate (receivePack): allowed
// pushes proceed past the gate (the garbage body then 400s at parse —
// gate-passage, not push-success, is the assertion); the host-write-only
// foreigner is 403 with the repo named (and pkt-ERR shaped when
// ?service=+git-eligible); anonymous is 401.
func TestPushGuardBodyMatrix(t *testing.T) {
	fx := pushGuardSetup(t, true)
	bob := auth.Principal{Name: "bob@example.com", Write: true}
	mallory := auth.Principal{Name: "mallory@example.com", Write: true}

	// Allowed: gate passes, garbage body dies at parse (400).
	req := pushGuardReq(t, "POST", "http://x/acme/r.git/git-receive-pack", "", strings.NewReader("garbage"))
	rec := httptest.NewRecorder()
	fx.srv.receivePack(rec, req, mustRepoID(t, "acme/r"), bob)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("allowed push must pass the gate (400 at parse), got %d %s", rec.Code, rec.Body.String())
	}
	// Foreigner with host write: 403 naming the repo.
	req = pushGuardReq(t, "POST", "http://x/acme/r.git/git-receive-pack", "tok-mallory", strings.NewReader("garbage"))
	rec = httptest.NewRecorder()
	fx.srv.receivePack(rec, req, mustRepoID(t, "acme/r"), mallory)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"acme/r"`) {
		t.Errorf("foreigner push = %d %q, want 403 naming the repo", rec.Code, rec.Body.String())
	}
	// Same denial, git-eligible (?service= + git UA + carried auth): pkt ERR.
	req = pushGuardReq(t, "POST", "http://x/acme/r.git/git-receive-pack?service=git-receive-pack", "tok-mallory", strings.NewReader("garbage"))
	rec = httptest.NewRecorder()
	fx.srv.receivePack(rec, req, mustRepoID(t, "acme/r"), mallory)
	if rec.Code != http.StatusOK {
		t.Errorf("git-eligible denial = %d, want 200+pkt ERR", rec.Code)
	} else if msg, ok := pktErrOf(rec.Body.String()); !ok || !strings.Contains(msg, `"acme/r"`) {
		t.Errorf("git-eligible denial must name the repo, got %q", rec.Body.String())
	}
	// Anonymous: real 401.
	req = pushGuardReq(t, "POST", "http://x/acme/r.git/git-receive-pack", "", strings.NewReader("garbage"))
	rec = httptest.NewRecorder()
	fx.srv.receivePack(rec, req, mustRepoID(t, "acme/r"), auth.Anonymous())
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous push = %d, want 401", rec.Code)
	}
	if fx.eng.published != 0 {
		t.Errorf("no gated push may publish; publishes = %d", fx.eng.published)
	}
}

// TestPushGuardSSHMatrix covers the SSH dispatch gate (SSHReceivePack):
// allowed pushes proceed to the pipeline (the "zzzz" body then dies at
// parse — gate-passage is the assertion); denied pushes name the repo;
// auto-create admits self/org-member and refuses foreign squats.
func TestPushGuardSSHMatrix(t *testing.T) {
	fx := pushGuardSetup(t, true)
	sshCtx := func() context.Context {
		return context.WithValue(context.Background(), repoRootKey{}, t.TempDir())
	}
	cases := []struct {
		name string
		repo string
		p    sshd.Principal
		// wantGate is the required outcome: "" = gate passes (parse
		// then fails on the "zzzz" body); otherwise the required
		// stderr substring.
		wantGate string
	}{
		{"org owner", "acme/r", sshd.Principal{Name: "alice@example.com", Write: true}, ""},
		{"team-bound member", "acme/r", sshd.Principal{Name: "bob@example.com", Write: true}, ""},
		{"bound w/o host flag", "acme/r2", sshd.Principal{Name: "carol@example.com"}, ""},
		{"slug self", "solo/r", sshd.Principal{Name: "solo", Write: true}, ""},
		{"host admin bypass", "acme/r", sshd.Principal{Name: "root@example.com", Write: true, Admin: true}, ""},
		{"host-write-only foreigner", "acme/r", sshd.Principal{Name: "mallory@example.com", Write: true}, `"acme/r"`},
		{"unbound reader", "acme/r", sshd.Principal{Name: "carol@example.com"}, `"acme/r"`},
	}
	for _, tc := range cases {
		err := fx.srv.SSHReceivePack(sshCtx(), mustRepoID(t, tc.repo), tc.p, strings.NewReader("zzzz"), io.Discard, io.Discard)
		if tc.wantGate == "" {
			if err == nil || !strings.Contains(err.Error(), "malformed push request") {
				t.Errorf("%s: gate must pass (parse fails next), got %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantGate) {
			t.Errorf("%s: deny must name the repo, got %v", tc.name, err)
		}
	}
	if fx.eng.published != 0 {
		t.Errorf("no gated push may publish; publishes = %d", fx.eng.published)
	}
}

// TestPushGuardSSH auto-create: self and member-org pushes proceed to the
// pipeline; a foreign squat is refused with the admission message; with
// auto-create off the write deny stands.
func TestPushGuardSSHAutoCreate(t *testing.T) {
	fx := pushGuardSetup(t, false) // missing repos; fakeEngine auto-creates
	sshCtx := func() context.Context {
		return context.WithValue(context.Background(), repoRootKey{}, t.TempDir())
	}
	// Self auto-create proceeds (malformed body = gate passed).
	if err := fx.srv.SSHReceivePack(sshCtx(), mustRepoID(t, "solo/newrepo"),
		sshd.Principal{Name: "solo", Write: true}, strings.NewReader("zzzz"), io.Discard, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "malformed push request") {
		t.Errorf("self auto-create must pass the gate, got %v", err)
	}
	// Member-org auto-create proceeds.
	if err := fx.srv.SSHReceivePack(sshCtx(), mustRepoID(t, "acme/newrepo"),
		sshd.Principal{Name: "bob@example.com", Write: true}, strings.NewReader("zzzz"), io.Discard, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "malformed push request") {
		t.Errorf("member-org auto-create must pass the gate, got %v", err)
	}
	// Foreign squat: admission refuses, naming the allowed owners.
	err := fx.srv.SSHReceivePack(sshCtx(), mustRepoID(t, "solo/hijack"),
		sshd.Principal{Name: "mallory@example.com", Write: true}, strings.NewReader("zzzz"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Errorf("foreign squat must be refused, got %v", err)
	}
	// Auto-create off: a foreign write deny stands (no admission escape).
	// (A self pusher with create off instead 404s — the write rule
	// passes, the repo simply does not exist.)
	fx.eng.noCreate = true
	err = fx.srv.SSHReceivePack(sshCtx(), mustRepoID(t, "solo/newrepo"),
		sshd.Principal{Name: "mallory@example.com", Write: true}, strings.NewReader("zzzz"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `"solo/newrepo"`) {
		t.Errorf("create-off deny must name the repo, got %v", err)
	}
}

// TestPushGuardProbeError pins the fail-closed probe path: when the
// existence probe itself errors, the push is 503 (never 403-as-404, never
// allowed through).
func TestPushGuardProbeError(t *testing.T) {
	fx := pushGuardSetup(t, true)
	fx.eng.syncErr = errProbeBoom
	mallory := auth.Principal{Name: "mallory@example.com", Write: true}
	req := pushGuardReq(t, "POST", "http://x/acme/r.git/git-receive-pack", "", strings.NewReader("garbage"))
	rec := httptest.NewRecorder()
	fx.srv.receivePack(rec, req, mustRepoID(t, "acme/r"), mallory)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("probe error = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "15" {
		t.Errorf("503 must carry Retry-After: 15")
	}
}

// TestPushGateNilLegacy pins the unwired fallback: host-flag requireWrite
// for existing repos, open auto-create — the pre-#347 behavior.
func TestPushGateNilLegacy(t *testing.T) {
	s, _ := newTestServer(t, nil)
	if s.pushGate != nil {
		t.Fatal("test server must leave the push gate unwired")
	}
	id := mustRepoID(t, "o/r")
	if aerr := s.checkPushWrite(context.Background(), id, auth.Principal{Name: "w", Write: true}); aerr != nil {
		t.Errorf("legacy write flag must pass: %v", aerr)
	}
	if aerr := s.checkPushWrite(context.Background(), id, auth.Principal{Name: "ro"}); aerr == nil {
		t.Error("legacy flagless push must be denied")
	}
	if aerr := s.checkPushCreate(context.Background(), "o", auth.Principal{Name: "w", Write: true}); aerr != nil {
		t.Errorf("legacy auto-create stays open: %v", aerr)
	}
}
