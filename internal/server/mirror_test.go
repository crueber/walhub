// mirror_test.go — the pull-only mirror push refusal (Forgejo #240,
// R1 (c)): discovery 403, funnel per-ref ng on both transports, SSH
// pre-advertisement refusal, admin-independence, and fetch immunity.
package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

func mirrorGuardServer(t *testing.T, eng *fakeEngine, guard func(ctx context.Context, id git.RepoId) bool) *Server {
	t.Helper()
	s, _ := newTestServer(t, func(o *Options) {
		o.Engine = eng
		o.MirrorGuard = guard
	})
	eng.exists = true
	eng.placement = Placement{Serve: true}
	return s
}

func mirrorOf(ids ...string) func(ctx context.Context, id git.RepoId) bool {
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	return func(ctx context.Context, id git.RepoId) bool { return set[id.String()] }
}

func TestMirrorDiscoveryRefusal(t *testing.T) {
	eng := &fakeEngine{}
	s := mirrorGuardServer(t, eng, mirrorOf("o/r"))
	root := t.TempDir()

	// Receive-pack discovery on a mirror → 403 plain text (admin or not:
	// alice's token carries admin and is still refused).
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-receive-pack", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(context.WithValue(req.Context(), repoRootKey{}, root))
	rec := httptest.NewRecorder()
	s.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != MirrorRefusal {
		t.Fatalf("body = %q", rec.Body.String())
	}

	// Upload-pack discovery (fetch/clone) is unaffected.
	req = httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(context.WithValue(req.Context(), repoRootKey{}, root))
	rec = httptest.NewRecorder()
	s.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code == http.StatusForbidden {
		t.Fatalf("fetch refused: %d %q", rec.Code, rec.Body.String())
	}

	// A non-mirror repo is unaffected.
	req = httptest.NewRequest("GET", "http://x/o/other.git/info/refs?service=git-receive-pack", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(context.WithValue(req.Context(), repoRootKey{}, root))
	rec = httptest.NewRecorder()
	s.gitInfoRefs(rec, req, mustRepoID(t, "o/other"))
	if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), MirrorRefusal) {
		t.Fatalf("non-mirror refused: %d", rec.Code)
	}

	// Nil guard → legacy (no refusal anywhere).
	s2, _ := newTestServer(t, func(o *Options) { o.Engine = eng })
	if s2.isMirrorRepo(context.Background(), mustRepoID(t, "o/r")) {
		t.Fatal("nil guard reports mirror")
	}
}

func TestMirrorPushPipelineRefusal(t *testing.T) {
	eng := &fakeEngine{exists: true, placement: Placement{Serve: true}}
	s := sshGateServer(t, eng, nil)
	s.mirrorGuard = mirrorOf("o/r")
	root := t.TempDir()
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)

	// HTTP funnel (a client skipping discovery and POSTing straight to
	// receive-pack): per-ref ng lines on the git wire, no publish.
	zero := strings.Repeat("0", 40)
	repo, err := eng.Repo(ctx, mustRepoID(t, "o/r"), true, git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	body := pushBody(t, repo, zero, "report-status side-band-64k", nil) // pure delete
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.receivePackLocal(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	report := rec.Body.String()
	if !strings.Contains(report, "unpack ok") {
		t.Fatalf("report = %q", report)
	}
	if !strings.Contains(report, "ng refs/heads/main "+MirrorRefusal) {
		t.Fatalf("ng missing: %q", report)
	}
	if eng.published != 0 {
		t.Fatalf("mirror push published (%d)", eng.published)
	}

	// Direct funnel call with a nil repo: the guard precedes ingest, so
	// the repo is never touched.
	var out2 strings.Builder
	oid := "1111111111111111111111111111111111111111"
	preq := &git.PushRequest{
		Commands: []git.PushCommand{{Old: zero, New: oid, Ref: "refs/heads/main"}},
		Caps:     []string{"report-status", "side-band-64k"},
	}
	p := auth.Principal{Name: "root", Write: true, Admin: true} // admins are refused too
	if err := s.pushPipeline(ctx, mustRepoID(t, "o/r"), p, nil, preq, nil, 1<<20, &out2); err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if !strings.Contains(out2.String(), "ng refs/heads/main "+MirrorRefusal) {
		t.Fatalf("direct report = %q", out2.String())
	}
}

func TestMirrorSSHAdvertRefusal(t *testing.T) {
	eng := &fakeEngine{exists: true, placement: Placement{Serve: true}}
	s := sshGateServer(t, eng, nil)
	s.mirrorGuard = mirrorOf("o/r")
	root := t.TempDir()
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)

	// Refusal precedes the advertisement: stdout stays empty (a client
	// that read an advertisement would hang waiting to send).
	var out strings.Builder
	err := s.SSHReceivePack(ctx, mustRepoID(t, "o/r"), "ada", strings.NewReader(""), &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), MirrorRefusal) {
		t.Fatalf("err = %v, want mirror refusal", err)
	}
	if out.Len() != 0 {
		t.Fatalf("advertisement leaked %d bytes", out.Len())
	}
	if eng.published != 0 {
		t.Fatalf("published = %d", eng.published)
	}
}
