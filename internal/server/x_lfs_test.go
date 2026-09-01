package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store"
)

// lfsOID is a valid sha256 hex oid for body.
func lfsOID(t *testing.T, body string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func TestLFSDispatchRoutes(t *testing.T) {
	s, _ := newTestServer(t, nil)
	id := mustRepoID(t, "o/r")
	authReq := func(method, url string) *http.Request {
		req := httptest.NewRequest(method, url, nil)
		req.Header.Set("Authorization", "Bearer tok123")
		return req
	}
	// Unknown subtree → 404.
	rec := httptest.NewRecorder()
	s.lfsDispatch(rec, authReq("GET", "http://x/o/r.git/info/lfs/other"), id, []string{"other"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown = %d", rec.Code)
	}
	// GET on batch → 405.
	rec = httptest.NewRecorder()
	s.lfsDispatch(rec, authReq("GET", "http://x/o/r.git/info/lfs/objects/batch"), id, []string{"objects/batch"})
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
		t.Fatalf("batch GET = %d", rec.Code)
	}
	// DELETE object → 405 with Allow.
	rec = httptest.NewRecorder()
	s.lfsDispatch(rec, authReq("DELETE", "http://x/o/r.git/info/lfs/objects/ab"), id, []string{"objects", "ab"})
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD, PUT" {
		t.Fatalf("object DELETE = %d", rec.Code)
	}
	// batch with bad body → 400.
	rec = httptest.NewRecorder()
	req := authReq("POST", "http://x/o/r.git/info/lfs/objects/batch")
	req.Body = http.NoBody
	req = httptest.NewRequest("POST", "http://x/o/r.git/info/lfs/objects/batch", strings.NewReader("{bad json"))
	req.Header.Set("Authorization", "Bearer tok123")
	s.lfsDispatch(rec, req, id, []string{"objects/batch"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad batch body = %d", rec.Code)
	}
	// batch with bad operation → 400.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "http://x/o/r.git/info/lfs/objects/batch",
		strings.NewReader(`{"operation":"clone","objects":[]}`))
	req.Header.Set("Authorization", "Bearer tok123")
	s.lfsDispatch(rec, req, id, []string{"objects/batch"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad operation = %d", rec.Code)
	}
}

func TestLFSPutGetVerifyRoundTrip(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cacheRoot = t.TempDir() // spool dir under the temp cache root
	id := mustRepoID(t, "o/r")
	body := strings.Repeat("walgit-lfs-bytes-", 40)
	oid := lfsOID(t, body)

	// PUT the object.
	req := httptest.NewRequest("PUT", "http://x/o/r.git/info/lfs/objects/"+oid, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok123")
	req.Header.Set("X-Lfs-Expected-Size", fmt.Sprintf("%d", len(body)))
	req.Header.Set("Content-Type", "application/octet-stream")
	rec := httptest.NewRecorder()
	s.lfsPut(rec, req, id, oid)
	if rec.Code != http.StatusOK {
		t.Fatalf("put = %d body=%s", rec.Code, rec.Body.String())
	}

	// GET streams it back through the static contract.
	req = httptest.NewRequest("GET", "http://x/o/r.git/info/lfs/objects/"+oid, nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.lfsGet(rec, req, id, oid)
	if rec.Code != http.StatusOK || rec.Body.String() != body {
		t.Fatalf("get = %d body=%q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content-type = %q", ct)
	}

	// HEAD: headers only, Content-Length set.
	req = httptest.NewRequest("HEAD", "http://x/o/r.git/info/lfs/objects/"+oid, nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.lfsGet(rec, req, id, oid)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("head = %d len=%d", rec.Code, rec.Body.Len())
	}
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprintf("%d", len(body)) {
		t.Fatalf("head content-length = %q", cl)
	}

	// VERIFY: ok, then size mismatch, then missing.
	verify := func(oid string, size int64) int {
		b := fmt.Sprintf(`{"oid":%q,"size":%d}`, oid, size)
		req := httptest.NewRequest("POST", "http://x/o/r.git/info/lfs/verify", strings.NewReader(b))
		req.Header.Set("Authorization", "Bearer tok123")
		req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
		rec := httptest.NewRecorder()
		s.lfsVerify(rec, req, id)
		return rec.Code
	}
	if code := verify(oid, int64(len(body))); code != http.StatusOK {
		t.Fatalf("verify ok = %d", code)
	}
	if code := verify(oid, int64(len(body))+1); code != http.StatusBadRequest {
		t.Fatalf("verify mismatch = %d", code)
	}
	if code := verify(lfsOID(t, "absent"), 1); code != http.StatusNotFound {
		t.Fatalf("verify missing = %d", code)
	}

	// LFS GET of an absent oid → 404.
	req = httptest.NewRequest("GET", "http://x/o/r.git/info/lfs/objects/"+lfsOID(t, "nope"), nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.lfsGet(rec, req, id, lfsOID(t, "nope"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing get = %d", rec.Code)
	}
}

func TestLFSPutValidation(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cacheRoot = t.TempDir()
	id := mustRepoID(t, "o/r")

	// Malformed oid.
	req := httptest.NewRequest("PUT", "http://x/o/r.git/info/lfs/objects/zz", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.lfsPut(rec, req, id, "zz")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed oid = %d", rec.Code)
	}
	// sha256 mismatch (valid hex, wrong content).
	oid := lfsOID(t, "expected")
	req = httptest.NewRequest("PUT", "http://x/o/r.git/info/lfs/objects/"+oid, strings.NewReader("other"))
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.lfsPut(rec, req, id, oid)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "sha256 mismatch") {
		t.Fatalf("hash mismatch = %d %s", rec.Code, rec.Body.String())
	}
	// size mismatch via header.
	oid2 := lfsOID(t, "sized")
	req = httptest.NewRequest("PUT", "http://x/o/r.git/info/lfs/objects/"+oid2, strings.NewReader("sized"))
	req.Header.Set("Authorization", "Bearer tok123")
	req.Header.Set("X-Lfs-Expected-Size", "999")
	rec = httptest.NewRecorder()
	s.lfsPut(rec, req, id, oid2)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "size mismatch") {
		t.Fatalf("size mismatch = %d %s", rec.Code, rec.Body.String())
	}
	// Unauthenticated → 401 (real 401: no Authorization carried).
	req = httptest.NewRequest("PUT", "http://x/o/r.git/info/lfs/objects/"+oid, nil)
	rec = httptest.NewRecorder()
	s.lfsPut(rec, req, id, oid)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon put = %d", rec.Code)
	}
	// Write-denied principal → pkt-ERR shape (Authorization carried, §4.2).
	s.cfg.Server.Auth.Tokens = append(s.cfg.Server.Auth.Tokens,
		config.StaticToken{Principal: "ro", Token: "ro-token", Write: false, Admin: false})
	s.authSvc = NewAuthService(&s.cfg.Server.Auth, s.Now)
	req = httptest.NewRequest("PUT", "http://x/o/r.git/info/lfs/objects/"+oid+"?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer ro-token")
	req.Header.Set("User-Agent", "git/2.46.0")
	rec = httptest.NewRecorder()
	s.lfsPut(rec, req, id, oid)
	if rec.Code != http.StatusOK {
		t.Fatalf("read-only put must hit the pkt-ERR shape, got %d", rec.Code)
	}
	if msg, ok := pktErrOf(rec.Body.String()); !ok || !strings.Contains(msg, "write") {
		t.Fatalf("want pkt ERR for write-denied LFS upload, got %q", rec.Body.String())
	}
}

func TestLFSPutSizeCapStillEnforced(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.LFS.MaxObjectBytes = config.ByteSize(4)
	s.cacheRoot = t.TempDir()
	id := mustRepoID(t, "o/r")
	body := "123456789"
	oid := lfsOID(t, body)
	req := httptest.NewRequest("PUT", "http://x/o/r.git/info/lfs/objects/"+oid, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.lfsPut(rec, req, id, oid)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("413 = %d", rec.Code)
	}
}

func TestIsHexOidLFS(t *testing.T) {
	if !isHexOidLFS(lfsOID(t, "x")) || !isHexOidLFS(strings.Repeat("a", 40)) {
		t.Fatal("valid hex oids must pass")
	}
	for _, bad := range []string{"", "zz", strings.Repeat("a", 39), strings.Repeat("a", 65)} {
		if isHexOidLFS(bad) {
			t.Fatalf("%q must fail", bad)
		}
	}
}

// TestLFSPutSpoolUnavailable covers the 503 path when the cache root cannot
// be created (a file blocks the directory).
func TestLFSPutSpoolUnavailable(t *testing.T) {
	s, _ := newTestServer(t, nil)
	blocker := t.TempDir()
	// Make cacheRoot/lfs-spool a FILE so CreateTemp fails.
	if err := os.WriteFile(blocker+"/lfs-spool", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cacheRoot = blocker
	id := mustRepoID(t, "o/r")
	oid := lfsOID(t, "blocked")
	req := httptest.NewRequest("PUT", "http://x/o/r.git/info/lfs/objects/"+oid, strings.NewReader("blocked"))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.lfsPut(rec, req, id, oid)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("spool blocked = %d body=%s", rec.Code, rec.Body.String())
	}
}

// --- upstream read-through -----------------------------------------------------

// upstreamLFS is a stub upstream LFS server: batch + object download.
func upstreamLFS(t *testing.T, oid string, body []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/batch"):
			w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
			href := fmt.Sprintf("http://%s/objects/%s?size=%d", r.Host, oid, len(body))
			fmt.Fprintf(w, `{"transfer":["basic"],"objects":[{"oid":%q,"size":%d,"actions":{"download":{"href":%q}}}]}`,
				oid, len(body), href)
		case strings.Contains(r.URL.Path, "/objects/"):
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestLFSUpstreamResolveAndReadThrough(t *testing.T) {
	id := mustRepoID(t, "o/r")
	body := "upstream-lfs-content-0123456789"
	oid := lfsOID(t, body)
	up := upstreamLFS(t, oid, []byte(body))
	defer up.Close()

	s, _ := newTestServer(t, nil)
	s.cfg.Upstream.Lfs = up.URL
	s.cacheRoot = t.TempDir()

	size, ok := s.lfsUpstreamResolve(context.Background(), id, oid)
	if !ok || size != int64(len(body)) {
		t.Fatalf("resolve = %d %v", size, ok)
	}
	// Unknown oid → not resolvable.
	if _, ok := s.lfsUpstreamResolve(context.Background(), id, lfsOID(t, "nope")); ok {
		t.Fatal("absent upstream oid must not resolve")
	}

	// GET of a missing object → read-through: streams upstream bytes.
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/lfs/objects/"+oid, nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.lfsGet(rec, req, id, oid)
	if rec.Code != http.StatusOK || rec.Body.String() != body {
		t.Fatalf("read-through = %d body=%q", rec.Code, rec.Body.String())
	}

	// The persist goroutine owns the spool after the handler returns; wait
	// briefly for the store write.
	var meta store.ObjectMeta
	var found bool
	for range 100 {
		if m, err := s.store.Head(context.Background(), lfsKey(id, oid)); err == nil && m != nil {
			meta, found = *m, true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !found {
		t.Fatal("read-through must persist the spooled object")
	}
	if meta.Size != int64(len(body)) {
		t.Fatalf("persisted size = %d", meta.Size)
	}

	// HEAD miss with upstream → 200 + Content-Length from the batch, no bytes.
	s2, _ := newTestServer(t, nil)
	s2.cfg.Upstream.Lfs = up.URL
	req = httptest.NewRequest("HEAD", "http://x/o/r.git/info/lfs/objects/"+oid, nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s2.lfsGet(rec, req, id, oid)
	if rec.Code != http.StatusOK {
		t.Fatalf("head read-through = %d", rec.Code)
	}
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprintf("%d", len(body)) {
		t.Fatalf("head content-length = %q", cl)
	}
	if rec.Body.Len() != 0 {
		t.Fatal("HEAD must not carry a body")
	}
}

func TestLFSReadThroughUpstreamDown(t *testing.T) {
	id := mustRepoID(t, "o/r")
	s, _ := newTestServer(t, nil)
	s.cfg.Upstream.Lfs = "http://127.0.0.1:1" // refused
	s.cacheRoot = t.TempDir()
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/lfs/objects/aa", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.serveLFSObject(rec, req, id, "aa")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("upstream down = %d", rec.Code)
	}
}

func TestLFSBatchUpstreamKnown(t *testing.T) {
	id := mustRepoID(t, "o/r")
	body := "batch-through"
	oid := lfsOID(t, body)
	up := upstreamLFS(t, oid, []byte(body))
	defer up.Close()
	s, _ := newTestServer(t, nil)
	s.cfg.Upstream.Lfs = up.URL
	s.cfg.Upstream.TokenEnv = "WALHUB_TEST_UPSTREAM_TOKEN"
	t.Setenv("WALHUB_TEST_UPSTREAM_TOKEN", "tok-up")
	req := httptest.NewRequest("POST", "http://x/o/r.git/info/lfs/objects/batch",
		strings.NewReader(fmt.Sprintf(`{"operation":"download","objects":[{"oid":%q,"size":0}]}`, oid)))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.lfsBatch(rec, req, id)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "?size=") {
		t.Fatalf("upstream batch = %d %s", rec.Code, rec.Body.String())
	}
}

func TestBaseURLForms(t *testing.T) {
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "http://host.example/x", nil)
	if got := s.baseURL(req); got != "http://host.example" {
		t.Fatalf("host base = %q", got)
	}
	s.cfg.Server.PublicURL = "https://pub.test/"
	if got := s.baseURL(req); got != "https://pub.test" {
		t.Fatalf("public base = %q", got)
	}
	s2, _ := newTestServer(t, nil)
	s2.tlsOn = true
	req2 := httptest.NewRequest("GET", "http://host.example/x", nil)
	if got := s2.baseURL(req2); got != "https://host.example" {
		t.Fatalf("tls base = %q", got)
	}
}

// TestLFSPutTeeWriter covers the multiWriter/firstErr helpers directly.
func TestLFSWriterHelpers(t *testing.T) {
	var a, b bytes.Buffer
	mw := multiWriter(func(p []byte) (int, error) { return a.Write(p) })
	n, err := mw.Write([]byte("half"))
	if err != nil || n != 4 {
		t.Fatalf("multiWriter = %d %v", n, err)
	}
	if firstErr(nil, err) != err || firstErr(nil, nil) != nil {
		t.Fatal("firstErr truth table broken")
	}
	_ = b
	// writerFunc passthrough.
	wf := writerFunc(func(p []byte) (int, error) { return b.Write(p) })
	wf.Write([]byte("x"))
	if b.String() != "x" {
		t.Fatalf("writerFunc = %q", b.String())
	}
}

// atomic flag helper used by the read-through disconnect assertions.
var _ = atomic.Bool{}
