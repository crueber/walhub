package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/wal"
)

var principalAlice = auth.Principal{Name: "alice", Write: true, Admin: true}

// gitCommitPack builds a real commit in a scratch repo and returns (pack, oid).
func gitCommitPack(t *testing.T, dir string) ([]byte, string) {
	t.Helper()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "HOME=" + dir}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	run("init", "-q", "-b", "main", ".")
	if err := writeFileSink(filepath.Join(dir, "f.txt"), "hello walgit"); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "c1")
	oid := strings.TrimSpace(run("rev-parse", "HEAD"))
	var buf bytes.Buffer
	cmd := exec.Command("git", "pack-objects", "--revs", "--stdout")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(oid + "\n")
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("pack-objects: %v", err)
	}
	pack := buf.Bytes()
	if len(pack) == 0 {
		t.Fatal("empty pack")
	}
	return pack, oid
}

func writeFileSink(p, body string) error { return os.WriteFile(p, []byte(body), 0o644) }

// pushBody builds a receive-pack request body: commands + pack.
func pushBody(t *testing.T, repo *git.LocalRepo, newOid string, caps string, pack []byte) []byte {
	t.Helper()
	if caps == "" {
		caps = "report-status object-format=" + repo.Format().String()
	}
	zero := strings.Repeat("0", len(repo.ZeroOid()))
	var b bytes.Buffer
	b.Write(git.Pkt(zero + " " + newOid + " refs/heads/main\000" + caps + "\n"))
	b.Write(git.Flush())
	b.Write(pack)
	return b.Bytes()
}

func TestUploadPackV2LsRefs(t *testing.T) {
	s, _ := newTestServer(t, nil)
	root := t.TempDir()
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	fe.placement = Placement{Serve: true}

	// Give the server repo a real ref so ls-refs has something to advertise.
	scratch := t.TempDir()
	_, oid := gitCommitPack(t, scratch)
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)
	repo, err := fe.Repo(ctx, mustRepoID(t, "o/r"), true, git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "fetch", "-q", scratch, "main:refs/heads/main")
	cmd.Dir = repo.Path
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + repo.Path}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed fetch: %v: %s", err, out)
	}

	body := append(git.Pkt("command=ls-refs\n"), git.Flush()...)
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-upload-pack", bytes.NewReader(body))
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer tok123")
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Git-Protocol", "version=2")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.uploadPack(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-git-upload-pack-result" {
		t.Fatalf("content-type = %q", ct)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "refs/heads/main") || !strings.Contains(out, oid) {
		t.Fatalf("ls-refs output missing ref: %q", out)
	}
}

// TestReceivePackEndToEnd pushes a real pack through receivePackLocal with the
// fake engine: parse → ingest → connectivity → publish → report-status.
func TestReceivePackEndToEnd(t *testing.T) {
	s, _ := newTestServer(t, nil)
	root := t.TempDir()
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	fe.placement = Placement{Serve: true, Maintain: true}

	scratch := t.TempDir()
	pack, oid := gitCommitPack(t, scratch)

	// Materialize the server-side repo the fake engine returns.
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)
	repo, err := fe.Repo(ctx, mustRepoID(t, "o/r"), true, git.Sha1)
	if err != nil {
		t.Fatal(err)
	}

	body := pushBody(t, repo, oid, "report-status side-band-64k object-format=sha1", pack)
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", bytes.NewReader(body))
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer tok123")
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.receivePackLocal(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "unpack ok") || !strings.Contains(out, "ok refs/heads/main") {
		t.Fatalf("report missing: %q", out)
	}
	if fe.published != 1 {
		t.Fatalf("publish called %d times", fe.published)
	}
}

func TestReceivePackConnectivityFailureBand2(t *testing.T) {
	s, _ := newTestServer(t, nil)
	root := t.TempDir()
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)
	repo, _ := fe.Repo(ctx, mustRepoID(t, "o/r"), true, git.Sha1)
	// A command pointing at an object that was never ingested → connectivity fail.
	body := pushBody(t, repo, strings.Repeat("a", 40), "report-status side-band-64k", nil)
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", bytes.NewReader(body))
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.receivePackLocal(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "connectivity check failed") || !strings.Contains(out, "unpack ng") {
		t.Fatalf("want band2 + report-status failure, got %q", out)
	}
}

func TestReceivePackMalformedBody(t *testing.T) {
	s, _ := newTestServer(t, nil)
	root := t.TempDir()
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)
	fe.Repo(ctx, mustRepoID(t, "o/r"), true, git.Sha1)
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", strings.NewReader("garbage"))
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.receivePackLocal(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}
func TestReceivePackNotFound(t *testing.T) {
	s, _ := newTestServer(t, nil)
	fe := s.engine.(*fakeEngine)
	fe.exists = false
	fe.noCreate = true
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(context.WithValue(req.Context(), repoRootKey{}, t.TempDir()))
	rec := httptest.NewRecorder()
	s.receivePackLocal(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestBodyReaderGzip(t *testing.T) {
	s, _ := newTestServer(t, nil)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte("hello compressed"))
	gz.Close()
	req := httptest.NewRequest("POST", "/x", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	rd, ok := s.bodyReader(rec, req)
	if !ok {
		t.Fatal("gzip body must decode")
	}
	b := make([]byte, 64)
	n, _ := rd.Read(b)
	if string(b[:n]) != "hello compressed" {
		t.Fatalf("decoded = %q", b[:n])
	}
	// Corrupt gzip → 400.
	req = httptest.NewRequest("POST", "/x", strings.NewReader("not gzip"))
	req.Header.Set("Content-Encoding", "gzip")
	rec = httptest.NewRecorder()
	if _, ok := s.bodyReader(rec, req); ok {
		t.Fatal("corrupt gzip must fail")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

// TestForwardToBroker covers the broker forwarding path with a real stub
// broker: headers forwarded, loop guard, and the local fallback on broker down.
func TestForwardToBroker(t *testing.T) {
	var gotPath, gotPrincipal, gotForwarded string
	var gotBody []byte
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		gotPrincipal = r.Header.Get("X-Walgit-Principal")
		gotForwarded = r.Header.Get("X-Walgit-Forwarded")
		if gotForwarded != "1" || gotPrincipal == "" {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("broker-ack"))
	}))
	defer broker.Close()

	s, _ := newTestServer(t, nil)
	s.cfg.WAL.PushBrokerURL = broker.URL
	s.cfg.WAL.PushBrokerBufferBytes = 1 << 20

	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	req.Header.Set("User-Agent", "git/2.46.0")
	resp, err := s.forwardToBroker(context.Background(), req, []byte("PACKDATA"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("broker status = %d", resp.StatusCode)
	}
	if string(gotBody) != "PACKDATA" {
		t.Fatalf("broker body = %q", gotBody)
	}
	if gotPath != "/o/r.git/git-receive-pack" || gotForwarded != "1" || gotPrincipal != "alice" {
		t.Fatalf("forwarded request shape: path=%q fwd=%q principal=%q", gotPath, gotForwarded, gotPrincipal)
	}

	// Loop guard: forwarded requests are refused at receivePack.
	s2, _ := newTestServer(t, nil)
	s2.cfg.WAL.PushBrokerURL = broker.URL
	req = httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", nil)
	req.Header.Set("X-Walgit-Forwarded", "1")
	rec := httptest.NewRecorder()
	s2.receivePack(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("loop guard status = %d", rec.Code)
	}
}

func TestReceivePackForwardBrokerDownLocalFallback(t *testing.T) {
	root := t.TempDir()
	fe := &fakeEngine{placement: Placement{Serve: false, Maintain: false}, exists: true}
	s, _ := newTestServer(t, func(o *Options) {
		o.Engine = fe
		o.Config.WAL.PushBrokerURL = "http://127.0.0.1:1" // refused: broker down
		o.Config.WAL.PushBrokerBufferBytes = 1 << 20
	})
	fe.exists = true
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)

	scratch := t.TempDir()
	pack, oid := gitCommitPack(t, scratch)
	repo, err := fe.Repo(ctx, mustRepoID(t, "o/r"), true, git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	body := pushBody(t, repo, oid, "report-status object-format=sha1", pack)
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", bytes.NewReader(body))
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.receivePackForward(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "unpack ok") {
		t.Fatalf("local fallback: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if fe.published != 1 {
		t.Fatalf("publish count = %d", fe.published)
	}
}

func TestBand2FailureAndHelpers(t *testing.T) {
	// isZeroOidStr.
	if !isZeroOidStr("0000") || !isZeroOidStr("0") || isZeroOidStr("") || isZeroOidStr("00a") {
		t.Fatal("isZeroOidStr truth table broken")
	}
	// isNotFound / errorsAs.
	if !isNotFound(wal.ErrNotFound("x")) {
		t.Fatal("wal not-found must map")
	}
	if isNotFound(errors.New("x")) {
		t.Fatal("plain error is not not-found")
	}
	var we *wal.WalError
	if !errorsAs(wal.ErrNotFound("x"), &we) {
		t.Fatal("errorsAs must resolve direct WalError")
	}
	// band2Failure without report-status.
	rec := httptest.NewRecorder()
	band2Failure(rec, &git.PushRequest{}, "boom")
	if _, ok := pktErrOf(rec.Body.String()); !ok {
		// band2 is a sideband frame, not a pkt ERR; just require the band-2 body.
		if !strings.Contains(rec.Body.String(), "boom") {
			t.Fatalf("band2 body = %q", rec.Body.String())
		}
	}
}

func TestGitServiceMethodGates(t *testing.T) {
	s, _ := newTestServer(t, nil)
	// GET on git-service → 405 with Allow.
	req := httptest.NewRequest("GET", "http://x/o/r.git/git-upload-pack", nil)
	rec := httptest.NewRecorder()
	s.gitService(rec, req, mustRepoID(t, "o/r"), git.ServiceUploadPack, true)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
		t.Fatalf("status = %d allow = %q", rec.Code, rec.Header().Get("Allow"))
	}
	// receive-pack without .git → pkt ERR.
	req = httptest.NewRequest("POST", "http://x/o/r/git-receive-pack", nil)
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.gitService(rec, req, mustRepoID(t, "o/r"), git.ServiceReceivePack, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if msg, ok := pktErrOf(rec.Body.String()); !ok || !strings.Contains(msg, ".git") {
		t.Fatalf("pkt err = %q", rec.Body.String())
	}
	// Unauthenticated → gitAuthFailure (401, no Authorization header carried).
	req = httptest.NewRequest("POST", "http://x/o/r.git/git-upload-pack", nil)
	rec = httptest.NewRecorder()
	s.gitService(rec, req, mustRepoID(t, "o/r"), git.ServiceUploadPack, true)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon upload-pack status = %d", rec.Code)
	}
}

func TestBundlesDispatchRest(t *testing.T) {
	s, _ := newTestServer(t, nil)
	id := mustRepoID(t, "o/r")
	req := authedGet("http://x/o/r.git/bundles/list")
	// empty rest → 404
	rec := httptest.NewRecorder()
	s.bundlesDispatch(rec, req, id, nil, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("empty rest = %d", rec.Code)
	}
	// catchup returns the chain entries.
	s.engine.(*fakeEngine).bundles = BundleList{
		Fulls: []BundleEntry{{Strategy: "full", Name: "f.bundle"}},
		Chain: []BundleEntry{{Strategy: "day1", Name: "c.bundle"}},
	}
	req = authedGet("http://x/o/r.git/bundles/catchup")
	rec = httptest.NewRecorder()
	s.bundlesDispatch(rec, req, id, []string{"catchup"}, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "c.bundle") || strings.Contains(rec.Body.String(), "f.bundle") {
		t.Fatalf("catchup body = %s", rec.Body.String())
	}
	// 405 on POST list.
	req = httptest.NewRequest("POST", "http://x/o/r.git/bundles/list", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.bundlesDispatch(rec, req, id, []string{"list"}, true)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post list = %d", rec.Code)
	}
	// unknown repo → 404.
	fe := s.engine.(*fakeEngine)
	fe.exists = false
	req = authedGet("http://x/o/r.git/bundles/list")
	rec = httptest.NewRecorder()
	s.bundlesDispatch(rec, req, id, []string{"list"}, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing repo bundles = %d", rec.Code)
	}
	fe.exists = true
	// one-segment unknown → 404.
	req = authedGet("http://x/o/r.git/bundles/only")
	rec = httptest.NewRecorder()
	s.bundlesDispatch(rec, req, id, []string{"only"}, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("one seg = %d", rec.Code)
	}
}

func authedGet(url string) *http.Request {
	req := httptest.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer tok123")
	return req
}

func TestUploadPackSyncFailure(t *testing.T) {
	s, _ := newTestServer(t, nil)
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	// Force Sync error by making Placement fail? Sync is called first: use a
	// repo id the fake engine treats as missing.
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-upload-pack", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(context.WithValue(req.Context(), repoRootKey{}, t.TempDir()))
	rec := httptest.NewRecorder()
	fe.exists = false
	s.uploadPack(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing sync status = %d", rec.Code)
	}
}

func TestGitInfoRefsMethods(t *testing.T) {
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("POST", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("status = %d allow = %q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestPktErrForNilForNonGit(t *testing.T) {
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("User-Agent", "curl/8")
	if s.pktErrFor(httptest.NewRecorder(), req) != nil {
		t.Fatal("non-git UA must get nil pkt writer")
	}
	req.Header.Set("User-Agent", "git/2.46.0")
	if s.pktErrFor(httptest.NewRecorder(), req) == nil {
		t.Fatal("git UA must get a pkt writer")
	}
}
