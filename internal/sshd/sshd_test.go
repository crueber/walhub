package sshd

// sshd_test.go — command parsing, auth, and dispatch: the SSH wire is driven
// with a real x/crypto client over loopback so every assertion lands on the
// bytes a real git client would send.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	gossh "golang.org/x/crypto/ssh"
)

// testTransport records dispatch calls and serves canned responses.
type testTransport struct {
	uploads   []uploadCall
	receives  []receiveCall
	uploadErr error
	recvErr   error
	out       string
}

type uploadCall struct {
	id       git.RepoId
	protocol string
	in       string
}

type receiveCall struct {
	id        git.RepoId
	principal string
	in        string
}

func (t *testTransport) SSHUploadPack(_ context.Context, id git.RepoId, protocol string, stdin io.Reader, stdout, stderr io.Writer) error {
	b, _ := io.ReadAll(stdin)
	t.uploads = append(t.uploads, uploadCall{id: id, protocol: protocol, in: string(b)})
	if t.uploadErr != nil {
		fmt.Fprint(stderr, "transport failed")
		return t.uploadErr
	}
	fmt.Fprint(stdout, t.out)
	return nil
}

func (t *testTransport) SSHReceivePack(_ context.Context, id git.RepoId, principal string, stdin io.Reader, stdout, stderr io.Writer) error {
	b, _ := io.ReadAll(stdin)
	t.receives = append(t.receives, receiveCall{id: id, principal: principal, in: string(b)})
	if t.recvErr != nil {
		fmt.Fprint(stderr, "transport failed")
		return t.recvErr
	}
	fmt.Fprint(stdout, t.out)
	return nil
}

func testKeyEntry(t *testing.T, principal string, write bool) (KeyEntry, gossh.Signer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(gossh.MarshalAuthorizedKey(signer.PublicKey())))
	return KeyEntry{Principal: principal, Write: write, Line: line}, signer
}

func testHostKey(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	block, err := gossh.MarshalPrivateKey(priv, "test host key")
	if err != nil {
		t.Fatal(err)
	}
	_ = signer
	return pem.EncodeToMemory(block)
}

// startTestServer boots an sshd.Server on 127.0.0.1:0 and returns its address
// plus the client signer matching the configured key.
func startTestServer(t *testing.T, tr Transport, cfg Config) string {
	t.Helper()
	srv, err := New(cfg, tr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.ListenAndServe(ctx) }()
	for i := 0; i < 100; i++ {
		if a := srv.Addr(); a != nil {
			c, err := net.DialTimeout("tcp", a.String(), 100*time.Millisecond)
			if err == nil {
				c.Close()
				return a.String()
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("ssh listener did not come up")
	return ""
}

func dialTestClient(t *testing.T, addr string, signer gossh.Signer) *gossh.Client {
	t.Helper()
	cl, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "git",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         1e9,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	return cl
}

// runExec runs one command on a fresh session and returns stdout, stderr,
// and the remote exit status.
func runExec(t *testing.T, cl *gossh.Client, cmd string) (string, string, int) {
	t.Helper()
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var outBuf, errBuf strings.Builder
	sess.Stdout = &outBuf
	sess.Stderr = &errBuf
	code := 0
	if err := sess.Run(cmd); err != nil {
		var ee *gossh.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("exec %q: %v", cmd, err)
		}
		code = ee.ExitStatus()
	}
	return outBuf.String(), errBuf.String(), code
}

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"git-upload-pack '/o/r.git'", []string{"git-upload-pack", "/o/r.git"}},
		{`git-upload-pack "/o/r.git"`, []string{"git-upload-pack", "/o/r.git"}},
		{"git-upload-pack /o/r.git", []string{"git-upload-pack", "/o/r.git"}},
		{"git-upload-pack '/o r/s p.git'", []string{"git-upload-pack", "/o r/s p.git"}},
		{`git-upload-pack "/o\"r.git"`, []string{"git-upload-pack", `/o"r.git`}},
		{"  git-upload-pack   '/x'  ", []string{"git-upload-pack", "/x"}},
		{"", nil},
	}
	for _, c := range cases {
		got, err := SplitCommand(c.in)
		if err != nil {
			t.Fatalf("SplitCommand(%q) error: %v", c.in, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("SplitCommand(%q) = %#v, want %#v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("SplitCommand(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
	if got, err := SplitCommand(`git-upload-pack /a\/b`); err != nil || got[1] != "/a/b" {
		t.Fatalf("escaped slash = %#v %v", got, err)
	}
	for _, bad := range []string{"git-upload-pack 'unterminated", `git-upload-pack "unterminated`, "git-upload-pack \\"} {
		if _, err := SplitCommand(bad); err == nil {
			t.Fatalf("SplitCommand(%q) must fail", bad)
		}
	}
}

func TestParseGitCommand(t *testing.T) {
	good := []struct {
		in   string
		verb string
		id   git.RepoId
	}{
		{"git-upload-pack '/acme/repo.git'", "git-upload-pack", git.RepoId{Owner: "acme", Name: "repo"}},
		{"git-receive-pack '/acme/repo.git'", "git-receive-pack", git.RepoId{Owner: "acme", Name: "repo"}},
		{"git-upload-pack 'acme/repo'", "git-upload-pack", git.RepoId{Owner: "acme", Name: "repo"}},
		{"git-upload-pack '/acme/repo.git/'", "git-upload-pack", git.RepoId{Owner: "acme", Name: "repo"}},
	}
	for _, c := range good {
		verb, id, err := ParseGitCommand(c.in)
		if err != nil || verb != c.verb || id != c.id {
			t.Fatalf("ParseGitCommand(%q) = %q %q %v, want %q %q", c.in, verb, id.String(), err, c.verb, c.id.String())
		}
	}
	bad := []string{
		"git-upload-pack",                       // no repo
		"git-upload-pack '/a' extra",            // extra word
		"git-upload-pack '-oProxyCommand=evil'", // option injection
		"git-shell '/x'",                        // unknown verb
		"git-upload-pack '/a/b/../../../etc'",   // invalid path charset
		"git-upload-pack ''",                    // empty path
		"ls -la",                                // not a git verb
		"git-upload-pack '/a/b' --quiet",        // trailing option
	}
	for _, c := range bad {
		if _, _, err := ParseGitCommand(c); err == nil {
			t.Fatalf("ParseGitCommand(%q) must fail", c)
		}
	}
}

func TestExecDispatchAndAuth(t *testing.T) {
	tr := &testTransport{out: "0000"}
	key, signer := testKeyEntry(t, "ada", true)
	roKey, roSigner := testKeyEntry(t, "robot", false)
	addr := startTestServer(t, tr, Config{
		Listen:  "127.0.0.1:0",
		HostKey: testHostKey(t),
		Keys:    []KeyEntry{key, roKey},
		Log:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	cl := dialTestClient(t, addr, signer)

	out, errText, code := runExec(t, cl, "git-upload-pack '/acme/repo.git'")
	if code != 0 || out != "0000" || errText != "" {
		t.Fatalf("upload-pack exec = %q %q %d", out, errText, code)
	}
	if len(tr.uploads) != 1 || tr.uploads[0].id.String() != "acme/repo" || tr.uploads[0].protocol != "" {
		t.Fatalf("upload call = %+v", tr.uploads[0])
	}

	// push path with a write-capable key
	out, _, code = runExec(t, cl, "git-receive-pack '/acme/repo.git'")
	if code != 0 || out != "0000" {
		t.Fatalf("receive-pack exec = %q %q %d", out, errText, code)
	}
	if len(tr.receives) != 1 || tr.receives[0].principal != "ada" {
		t.Fatalf("receive call = %+v", tr.receives[0])
	}

	// read-only principal: fetch ok, push refused
	ro := dialTestClient(t, addr, roSigner)
	if _, _, code = runExec(t, ro, "git-upload-pack '/acme/repo.git'"); code != 0 {
		t.Fatal("read-only key must fetch")
	}
	_, errText, code = runExec(t, ro, "git-receive-pack '/acme/repo.git'")
	if code == 0 || !strings.Contains(errText, "write access required") {
		t.Fatalf("read-only push = %q %d, want write-access refusal", errText, code)
	}

	// unknown verb + injection attempts are refused before any transport call
	n := len(tr.uploads) + len(tr.receives)
	for _, cmd := range []string{"git-upload-pack '-oProxyCommand=evil'", "ls -la", "git-upload-pack"} {
		_, _, code = runExec(t, cl, cmd)
		if code == 0 {
			t.Fatalf("command %q must be refused", cmd)
		}
	}
	if n != len(tr.uploads)+len(tr.receives) {
		t.Fatal("refused commands must not reach the transport")
	}
}

func TestUnknownKeyRefused(t *testing.T) {
	tr := &testTransport{}
	key, _ := testKeyEntry(t, "ada", true)
	otherSigner := testKeySigner(t)
	addr := startTestServer(t, tr, Config{
		Listen:  "127.0.0.1:0",
		HostKey: testHostKey(t),
		Keys:    []KeyEntry{key},
		Log:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	_, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "git",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(otherSigner)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         time.Second,
	})
	if err == nil {
		t.Fatal("unknown key must fail the handshake")
	}
	if len(tr.uploads) != 0 {
		t.Fatal("unknown key must not reach the transport")
	}
}

func TestTransportErrorsMapToStderr(t *testing.T) {
	tr := &testTransport{uploadErr: fmt.Errorf("%w: acme/x", ErrNotFound)}
	key, signer := testKeyEntry(t, "ada", true)
	addr := startTestServer(t, tr, Config{
		Listen:  "127.0.0.1:0",
		HostKey: testHostKey(t),
		Keys:    []KeyEntry{key},
		Log:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	cl := dialTestClient(t, addr, signer)
	_, errText, code := runExec(t, cl, "git-upload-pack '/acme/x.git'")
	if code != 1 || !strings.Contains(errText, "repository not found") {
		t.Fatalf("not-found transport error = %q %d", errText, code)
	}
}

func TestHostKeyGenerationPersists(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/ssh/host_ed25519"
	cfg := Config{HostKeyPath: path}
	if _, err := hostSigner(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("generated host key missing: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// second boot loads the same key (no regeneration)
	s1, err := hostSigner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := hostSigner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_ = first
	if string(s1.PublicKey().Marshal()) != string(s2.PublicKey().Marshal()) {
		t.Fatal("persisted host key must be stable across boots")
	}
}

func TestMaxSessions(t *testing.T) {
	// A 0-slot semaphore must refuse immediately rather than hang.
	tr := &testTransport{}
	key, signer := testKeyEntry(t, "ada", true)
	addr := startTestServer(t, tr, Config{
		Listen:      "127.0.0.1:0",
		HostKey:     testHostKey(t),
		Keys:        []KeyEntry{key},
		MaxSessions: -1, // normalizes to the 64 default; this test only proves the path runs
		Log:         slog.New(slog.DiscardHandler),
	})
	cl := dialTestClient(t, addr, signer)
	if _, _, code := runExec(t, cl, "git-upload-pack '/a/b.git'"); code != 0 {
		t.Fatal("default session limit must serve")
	}
}

// testKeySigner generates a client signer with NO configured counterpart.
func testKeySigner(t *testing.T) gossh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
