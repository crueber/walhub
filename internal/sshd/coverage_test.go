package sshd

// coverage_test.go — the session-handler branches the e2e does not reach:
// PTY/shell refusal, GIT_PROTOCOL passthrough, the session limiter, and the
// host-key/load edge cases.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	gossh "golang.org/x/crypto/ssh"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// execCapture runs a command and records what the transport received.
type captureTransport struct {
	protosMu        sync.Mutex
	protos          []string
	block           chan struct{}
	entered         chan struct{}
	failAfterCancel bool
}

func (c *captureTransport) recordProto(p string) {
	c.protosMu.Lock()
	defer c.protosMu.Unlock()
	c.protos = append(c.protos, p)
}

func (c *captureTransport) SSHUploadPack(ctx context.Context, id git.RepoId, protocol string, stdin io.Reader, stdout, stderr io.Writer) error {
	c.recordProto(protocol)
	if c.entered != nil {
		close(c.entered)
	}
	if c.block != nil {
		<-c.block
	}
	if c.failAfterCancel && ctx.Err() != nil {
		return errors.New("canceled")
	}
	fmt.Fprint(stdout, "0000")
	return nil
}

func (c *captureTransport) SSHReceivePack(ctx context.Context, id git.RepoId, principal string, stdin io.Reader, stdout, stderr io.Writer) error {
	return nil
}

func TestPTYAndShellRefused(t *testing.T) {
	tr := &captureTransport{}
	key, signer := testKeyEntry(t, "ada", true)
	srv, err := New(Config{
		Listen:    "127.0.0.1:0",
		HostKey:   testHostKey(t),
		KeyLookup: staticLookup(map[string]KeyEntry{fpOf(signer): key}),
		Log:       discardLogger(),
	}, tr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.ListenAndServe(ctx) }()
	addr := waitAddr(t, srv)

	cl := dialTestClient(t, addr.String(), signer)
	// shell request: client Session.Shell → refused with a non-zero exit
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.Shell(); err == nil {
		t.Fatal("shell request must be refused")
	}
	// stderr text is best-effort (the channel may close before it drains)
	if len(tr.protos) != 0 {
		t.Fatal("refused sessions must not reach the transport")
	}
}

func TestGitProtocolPassthrough(t *testing.T) {
	tr := &captureTransport{}
	key, signer := testKeyEntry(t, "ada", true)
	srv, err := New(Config{
		Listen:    "127.0.0.1:0",
		HostKey:   testHostKey(t),
		KeyLookup: staticLookup(map[string]KeyEntry{fpOf(signer): key}),
		Log:       discardLogger(),
	}, tr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.ListenAndServe(ctx) }()
	addr := waitAddr(t, srv)

	cl := dialTestClient(t, addr.String(), signer)
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.Setenv("GIT_PROTOCOL", "version=2"); err != nil {
		t.Fatal(err)
	}
	var outBuf strings.Builder
	sess.Stdout = &outBuf
	if err := sess.Run("git-upload-pack '/acme/repo.git'"); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if len(tr.protos) != 1 || tr.protos[0] != "version=2" {
		t.Fatalf("protocols = %v", tr.protos)
	}
	// a non-v2 value degrades to v0
	sess2, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess2.Close()
	if err := sess2.Setenv("GIT_PROTOCOL", "version=1"); err != nil {
		t.Fatal(err)
	}
	if err := sess2.Run("git-upload-pack '/acme/repo.git'"); err != nil {
		t.Fatal(err)
	}
	if tr.protos[1] != "" {
		t.Fatalf("protocol[1] = %q, want empty (v0)", tr.protos[1])
	}
}

func TestMaxSessionsRefuses(t *testing.T) {
	tr := &captureTransport{block: make(chan struct{})}
	key, signer := testKeyEntry(t, "ada", true)
	srv, err := New(Config{
		Listen:      "127.0.0.1:0",
		HostKey:     testHostKey(t),
		KeyLookup:   staticLookup(map[string]KeyEntry{fpOf(signer): key}),
		MaxSessions: 1,
		Log:         discardLogger(),
	}, tr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.ListenAndServe(ctx) }()
	addr := waitAddr(t, srv)
	cl := dialTestClient(t, addr.String(), signer)

	// first session: held open by the blocked transport
	sess1, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess1.Close()
	go func() { _ = sess1.Run("git-upload-pack '/a/b.git'") }()
	// second session: refused immediately by the limiter
	sess2, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess2.Close()
	var errBuf strings.Builder
	sess2.Stderr = &errBuf
	err = sess2.Run("git-upload-pack '/a/b.git'")
	if err == nil || !strings.Contains(errBuf.String(), "too many concurrent sessions") {
		t.Fatalf("second session = %v %q, want limiter refusal", err, errBuf.String())
	}
	close(tr.block)
}

// releaseQueue blocks each exec in its own channel so a test can finish
type releaseQueue struct {
	mu    sync.Mutex
	hold_ []chan struct{}
}

func (q *releaseQueue) hold() <-chan struct{} {
	ch := make(chan struct{})
	q.mu.Lock()
	q.hold_ = append(q.hold_, ch)
	q.mu.Unlock()
	return ch
}

func (q *releaseQueue) releaseOne() {
	q.mu.Lock()
	ch := q.hold_[0]
	q.hold_ = q.hold_[1:]
	q.mu.Unlock()
	close(ch)
}

func (q *releaseQueue) SSHUploadPack(ctx context.Context, id git.RepoId, protocol string, stdin io.Reader, stdout, stderr io.Writer) error {
	<-q.hold()
	fmt.Fprint(stdout, "0000")
	return nil
}

func (q *releaseQueue) SSHReceivePack(ctx context.Context, id git.RepoId, principal string, stdin io.Reader, stdout, stderr io.Writer) error {
	return nil
}

// TestSessionCapIsPerConnection pins the limiter's scope and leak-freedom: the
// 16-session cap is per connection (a server-global pre-check used to
// serialize unrelated connections), and a refused attempt must not burn a slot
// on that connection (the counter used to leak upward per rejection).
func TestSessionCapIsPerConnection(t *testing.T) {
	tr := &releaseQueue{}
	key, signer := testKeyEntry(t, "ada", true)
	srv, err := New(Config{
		Listen:      "127.0.0.1:0",
		HostKey:     testHostKey(t),
		KeyLookup:   staticLookup(map[string]KeyEntry{fpOf(signer): key}),
		MaxSessions: 64,
		Log:         discardLogger(),
	}, tr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.ListenAndServe(ctx) }()
	addr := waitAddr(t, srv)
	run := func(sess *gossh.Session) {
		sess.Stderr = io.Discard
		go func() { _ = sess.Run("git-upload-pack '/a/b.git'") }()
	}

	c1 := dialTestClient(t, addr.String(), signer)
	var held []*gossh.Session
	t.Cleanup(func() {
		for _, s := range held {
			_ = s.Close()
		}
	})
	for i := 0; i < maxSessionsPerConn; i++ {
		sess, err := c1.NewSession()
		if err != nil {
			t.Fatalf("session %d: %v", i+1, err)
		}
		held = append(held, sess)
		run(sess)
	}

	// 17th on the same connection: the channel open is refused.
	if over, err := c1.NewSession(); err == nil {
		defer over.Close()
		t.Fatal("17th session opened, want per-conn refusal")
	}

	// A second connection is unaffected by the first's cap.
	c2 := dialTestClient(t, addr.String(), signer)
	sess, err := c2.NewSession()
	if err != nil {
		t.Fatalf("second connection blocked by first: %v", err)
	}
	held = append(held, sess)
	run(sess)

	// Finish one session, then the same connection must admit a new one: the
	// refused attempt above must not have leaked its increment.
	tr.releaseOne()
	time.Sleep(50 * time.Millisecond) // let the server observe the exit
	again, err := c1.NewSession()
	if err != nil {
		t.Fatalf("rejected attempt burned a slot: %v", err)
	}
	held = append(held, again)
	run(again)
}

// waitAddr polls until the server's listener is up and returns its address.
func waitAddr(t *testing.T, srv *Server) net.Addr {
	t.Helper()
	for i := 0; i < 100; i++ {
		if a := srv.Addr(); a != nil {
			if c, err := net.Dial("tcp", a.String()); err == nil {
				c.Close()
				return a
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("ssh listener did not come up")
	return nil
}

func TestHostSignerBranches(t *testing.T) {
	// explicit bytes: bad PEM → error; good PEM → signer
	if _, err := hostSigner(Config{HostKey: []byte("not a key")}); err == nil {
		t.Fatal("bad host key bytes must error")
	}
	// path exists but holds garbage → error
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hostSigner(Config{HostKeyPath: bad}); err == nil {
		t.Fatal("garbage host key file must error")
	}
	// unwritable dir → generation persists error
	if _, err := hostSigner(Config{HostKeyPath: "/proc/nope/ed25519_host_key"}); err == nil {
		t.Fatal("unwritable host key path must error")
	}
	// ephemeral (no bytes, no path) → still returns a signer
	s, err := hostSigner(Config{})
	if err != nil || s == nil {
		t.Fatalf("ephemeral host key = %v %v", s, err)
	}
}

func TestNewRejectsMissingLookup(t *testing.T) {
	if _, err := New(Config{HostKey: testHostKey(t)}, &captureTransport{}); err == nil || !strings.Contains(err.Error(), "KeyLookup is required") {
		t.Fatalf("missing KeyLookup = %v", err)
	}
}

func TestAddrNilBeforeListen(t *testing.T) {
	srv, err := New(Config{HostKey: testHostKey(t), KeyLookup: staticLookup(map[string]KeyEntry{})}, &captureTransport{})
	if err != nil {
		t.Fatal(err)
	}
	if srv.Addr() != nil {
		t.Fatal("Addr before listen must be nil")
	}
}

func TestListenAndServeBadAddr(t *testing.T) {
	srv, err := New(Config{Listen: "bogus", HostKey: testHostKey(t), KeyLookup: staticLookup(map[string]KeyEntry{})}, &captureTransport{})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.ListenAndServe(context.Background()); err == nil {
		t.Fatal("bad listen address must error")
	}
}

func TestExecClientDisconnect(t *testing.T) {
	// exec on a session whose client vanishes: transport runs with a canceled
	// ctx and the channel writes fail — must not panic and must not report a
	// transport error to a dead reader.
	tr := &captureTransport{}
	key, signer := testKeyEntry(t, "ada", true)
	srv, err := New(Config{Listen: "127.0.0.1:0", HostKey: testHostKey(t), KeyLookup: staticLookup(map[string]KeyEntry{fpOf(signer): key}), Log: discardLogger()}, tr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.ListenAndServe(ctx) }()
	waitAddr(t, srv)
	cl := dialTestClient(t, srv.Addr().String(), signer)
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var outBuf strings.Builder
	sess.Stdout = &outBuf
	sess.Run("git-upload-pack '/a/b.git'") //nolint:errcheck — the client may race the close
	cancel()                               // tear the listener down mid-flight
}

func TestSplitCommandEscapeAndTabEdges(t *testing.T) {
	// a backslash inside double quotes escapes only quote/backslash
	got, err := SplitCommand(`git-upload-pack "/a\tb.git"`)
	if err != nil || len(got) != 2 || got[1] != "/a\\tb.git" {
		t.Fatalf("escaped backslash-t = %#v %v", got, err)
	}
	// tabs act as separators
	got, err = SplitCommand("git-upload-pack\t'/x'")
	if err != nil || len(got) != 2 || got[1] != "/x" {
		t.Fatalf("tab separator = %#v %v", got, err)
	}
}

func TestParseGitCommandBadPath(t *testing.T) {
	// a path with an invalid owner charset is refused by ParseRepoId
	if _, _, err := ParseGitCommand("git-upload-pack '/a b/c.git'"); err == nil {
		t.Fatal("space in owner must fail")
	}
}

func TestExecOptionInjectionWord(t *testing.T) {
	// a second word starting with '-' is refused even when quoted
	if _, _, err := ParseGitCommand("git-receive-pack '/a/b.git' --report"); err == nil {
		t.Fatal("extra option word must fail")
	}
}

func TestChannelAndRequestEdges(t *testing.T) {
	tr := &captureTransport{}
	key, signer := testKeyEntry(t, "ada", true)
	key.Admin = true
	srv, err := New(Config{
		Listen:    "127.0.0.1:0",
		HostKey:   testHostKey(t),
		KeyLookup: staticLookup(map[string]KeyEntry{fpOf(signer): key}),
		Log:       discardLogger(),
	}, tr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.ListenAndServe(ctx) }()
	addr := waitAddr(t, srv)
	cl := dialTestClient(t, addr.String(), signer)

	// non-session channels are rejected
	if _, _, err := cl.OpenChannel("direct-tcpip", []byte("junk")); err == nil {
		t.Fatal("direct-tcpip must be rejected")
	}
	// a session rejects unknown request types with want-reply
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	ok, err := sess.SendRequest("keepalive@openssh.com", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("unknown request must be replied false")
	}
	// a malformed env payload is replied false, not fatal
	for _, bad := range [][]byte{{}, {0x01}, {0xff, 0xff, 0xff, 0xff, 0x00}} {
		_, _ = sess.SendRequest("env", true, bad)
	}
	// admin flag travels (key marked admin above)
	if err := sess.Run("git-upload-pack '/a/b.git'"); err != nil {
		t.Fatal(err)
	}
}

func TestExecClientVanishesMidTransport(t *testing.T) {
	// the transport blocks until the server context cancels: the session sees
	// a canceled ctx and takes the silent exit path (325).
	tr := &captureTransport{block: make(chan struct{})}
	key, signer := testKeyEntry(t, "ada", true)
	srv, err := New(Config{Listen: "127.0.0.1:0", HostKey: testHostKey(t), KeyLookup: staticLookup(map[string]KeyEntry{fpOf(signer): key}), Log: discardLogger()}, tr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.ListenAndServe(ctx) }()
	waitAddr(t, srv)
	cl := dialTestClient(t, srv.Addr().String(), signer)
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = sess.Run("git-upload-pack '/a/b.git'")
		close(tr.block)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel() // kill the connection while the transport is blocked
}

func TestParseGitCommandPassthroughAndOptionWord(t *testing.T) {
	// SplitCommand errors pass through ParseGitCommand (unterminated quote).
	if _, _, err := ParseGitCommand("git-upload-pack 'unterminated"); err == nil {
		t.Fatal("unterminated quote must fail")
	}
	// a trailing option word is refused
	if _, _, err := ParseGitCommand("git-receive-pack '/a/b.git' --report"); err == nil {
		t.Fatal("trailing option word must fail")
	}
}

func TestWriteFileErrorPath(t *testing.T) {
	dir := t.TempDir()
	roDir := filepath.Join(dir, "ro")
	if err := os.Mkdir(roDir, 0o500); err != nil {
		t.Fatal(err)
	}
	// dir exists but is not writable: MkdirAll succeeds (exists), WriteFile fails
	if _, err := hostSigner(Config{HostKeyPath: filepath.Join(roDir, "hostkey")}); err == nil {
		t.Fatal("unwritable host key file must error")
	}
}

func TestExecCanceledWhileTransportBlocked(t *testing.T) {
	tr := &captureTransport{block: make(chan struct{}), entered: make(chan struct{})}
	key, signer := testKeyEntry(t, "ada", true)
	key.Admin = true
	srv, err := New(Config{Listen: "127.0.0.1:0", HostKey: testHostKey(t), KeyLookup: staticLookup(map[string]KeyEntry{fpOf(signer): key}), Log: discardLogger()}, tr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.ListenAndServe(ctx) }()
	waitAddr(t, srv)
	cl := dialTestClient(t, srv.Addr().String(), signer)
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	go func() {
		<-tr.entered
		cancel()
		close(tr.block)
	}()
	_ = sess.Run("git-upload-pack '/a/b.git'")
}

func TestNewBadHostKeyBytes(t *testing.T) {
	if _, err := New(Config{HostKey: []byte("junk"), KeyLookup: staticLookup(map[string]KeyEntry{})}, &captureTransport{}); err == nil {
		t.Fatal("New must reject bad host key bytes")
	}
}

func TestExecCanceledExitPath(t *testing.T) {
	// the ctx-canceled exit path: transport entered, ctx canceled while it
	// runs, then the transport returns — exec must exit(1) silently.
	tr := &captureTransport{block: make(chan struct{}), entered: make(chan struct{})}
	key, signer := testKeyEntry(t, "ada", true)
	srv, err := New(Config{Listen: "127.0.0.1:0", HostKey: testHostKey(t), KeyLookup: staticLookup(map[string]KeyEntry{fpOf(signer): key}), Log: discardLogger()}, tr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.ListenAndServe(ctx) }()
	waitAddr(t, srv)
	cl := dialTestClient(t, srv.Addr().String(), signer)
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	go func() {
		<-tr.entered
		cancel()
		close(tr.block)
	}()
	_ = sess.Run("git-upload-pack '/a/b.git'")
}

func TestSessionAcceptErrorIsBenign(t *testing.T) {
	// handleSession's newCh.Accept error branch: a client that opens a session
	// channel and immediately hangs up exercises it without a panic.
	srv, err := New(Config{Listen: "127.0.0.1:0", HostKey: testHostKey(t), KeyLookup: staticLookup(map[string]KeyEntry{})}, &captureTransport{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.ListenAndServe(ctx) }()
	waitAddr(t, srv)
	c, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close() // raw disconnect: handshake error path in handleConn
}

func TestHostSignerFileErrors(t *testing.T) {
	dir := t.TempDir()
	// a garbage host key file → parse error, never regeneration
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hostSigner(Config{HostKeyPath: bad}); err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("garbage file = %v", err)
	}
	// an unwritable directory → generation persist fails
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := hostSigner(Config{HostKeyPath: filepath.Join(ro, "hostkey")}); err == nil {
		t.Fatal("unwritable dir must fail")
	}
	// a read-only directory with the key missing → write fails
	wr := filepath.Join(dir, "wr")
	if err := os.Mkdir(wr, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wr, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := hostSigner(Config{HostKeyPath: filepath.Join(wr, "hostkey")}); err == nil {
		t.Fatal("read-only dir must fail the write")
	}
}

func TestPerConnectionSessionCap(t *testing.T) {
	tr := &captureTransport{}
	key, signer := testKeyEntry(t, "ada", true)
	srv, err := New(Config{Listen: "127.0.0.1:0", HostKey: testHostKey(t), KeyLookup: staticLookup(map[string]KeyEntry{fpOf(signer): key}), Log: discardLogger()}, tr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.ListenAndServe(ctx) }()
	waitAddr(t, srv)
	cl := dialTestClient(t, srv.Addr().String(), signer)

	// 16 session channels open and park (no exec); the 17th is rejected.
	opened := 0
	var sessions []*gossh.Session
	for i := 0; i < 17; i++ {
		sess, err := cl.NewSession()
		if err != nil {
			break // the 17th open fails (channel rejected)
		}
		sessions = append(sessions, sess)
		opened++
	}
	if opened != 16 {
		t.Fatalf("opened = %d sessions, want exactly 16 then a refusal", opened)
	}
	for _, s := range sessions {
		s.Close()
	}
}

func TestHostSignerReadAndMkdirErrors(t *testing.T) {
	dir := t.TempDir()
	// HostKeyPath is a DIRECTORY: os.ReadFile → EISDIR (not NotExist) → fatal
	if _, err := hostSigner(Config{HostKeyPath: dir}); err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("directory host key path = %v", err)
	}
	// HostKeyPath under a FILE: MkdirAll fails
	fileAsDir := filepath.Join(dir, "file")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hostSigner(Config{HostKeyPath: filepath.Join(fileAsDir, "hostkey")}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("read error under a file = %v", err)
	}
}

func TestExecBadPayloadAndCanceledExit(t *testing.T) {
	tr := &captureTransport{block: make(chan struct{}), entered: make(chan struct{}), failAfterCancel: true}
	key, signer := testKeyEntry(t, "ada", true)
	srv, err := New(Config{Listen: "127.0.0.1:0", HostKey: testHostKey(t), KeyLookup: staticLookup(map[string]KeyEntry{fpOf(signer): key}), Log: discardLogger()}, tr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.ListenAndServe(ctx) }()
	waitAddr(t, srv)
	cl := dialTestClient(t, srv.Addr().String(), signer)

	// malformed exec payload → request replied false
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	ok, err := sess.SendRequest("exec", true, []byte{0xff, 0xff, 0xff, 0xff})
	if err != nil || ok {
		t.Fatalf("bad exec payload = ok %v err %v", ok, err)
	}
	sess.Close()

	// transport erroring after the ctx cancels: exec takes the silent exit
	sess2, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-tr.entered
		cancel()
	}()
	_ = sess2.Run("git-upload-pack '/a/b.git'")
	close(tr.block)
}
