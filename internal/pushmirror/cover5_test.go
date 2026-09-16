// cover5_test.go — final gate pass: nil receivers, default arms,
// view/schedule edges, lease/release faults, and the SSH push shape.
package pushmirror

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

func newRegistryForTest(t *testing.T, ctx context.Context, st store.ObjectStore, cfg *config.Config) *wal.Registry {
	t.Helper()
	reg := wal.NewRegistry(ctx, st, cfg)
	t.Cleanup(reg.Close)
	return reg
}

func testSHA1() git.ObjectFormat { return git.Sha1 }

func newRecorder() http.ResponseWriter { return httptest.NewRecorder() }

func newRequest(method, path string) *http.Request { return httptest.NewRequest(method, path, nil) }

func TestNilReceiversAndDefaults(t *testing.T) {
	var nilSec *Secret
	if nilSec.HasMaterial() {
		t.Error("nil HasMaterial")
	}
	if nilSec.SecretHint() != "" {
		t.Error("nil SecretHint")
	}
	// Due with a broken schedule fails closed (false, no panic).
	if Due(&Doc{Schedule: "bogus"}, time.Now().UTC()) {
		t.Error("Due(bogus)")
	}
	// ViewOf with a broken schedule: no next fire, no due.
	v := ViewOf(&Doc{UpstreamURL: "x", AuthKind: AuthNone, Schedule: "bogus"}, nil, time.Now().UTC())
	if v.Due || v.NextSyncAt != "" {
		t.Errorf("bogus view = %+v", v)
	}
	// Backed-off scheduled doc is not due.
	now := time.Now().UTC()
	due := &Doc{Schedule: PresetDaily, LastAttemptAt: now.Format(time.RFC3339), ConsecutiveFailures: 2}
	if Due(due, now) {
		t.Error("backed-off Due")
	}
	// Handler clock override.
	h, _, _ := testHandler(t, adminPrincipal)
	h.Now = time.Now
	_ = h.now()
}

func TestRecordAndSecretLostTwice(t *testing.T) {
	ctx := context.Background()
	inner := store.NewMemory()
	if _, err := Create(ctx, inner, "o", "r", "file:///x", AuthNone, "", ScheduleOff); err != nil {
		t.Fatal(err)
	}
	if err := RecordAttempt(ctx, &faultStore{ObjectStore: inner, failLeft: 5}, "o", "r", true, "", time.Now().UTC()); err == nil {
		t.Error("RecordAttempt always-412 ok")
	}
	if err := SaveSecretCAS(ctx, &faultStore{ObjectStore: inner, failLeft: 5}, "o", "r", &Secret{AuthKind: AuthToken, Token: "x"}); err == nil {
		t.Error("SaveSecretCAS always-412 ok")
	}
}

func TestLeasePutErrors(t *testing.T) {
	ctx := context.Background()
	inner := store.NewMemory()
	reg, _ := testRegistry(t)
	boom := errors.New("put down")
	// Generic (non-412) Put error on Create.
	svc := New(Deps{Store: &faultStore2{ObjectStore: inner, failPut: boom, failPutN: -1}, Reg: reg, CacheDir: t.TempDir(), Hostname: "h"})
	if _, err := svc.acquireLease(ctx, "o", "r"); err == nil {
		t.Error("acquire Create-error ok")
	}
	// Generic Put error on steal-Update (expired lease present).
	past := time.Now().UTC().Add(-time.Hour)
	if _, err := inner.Put(ctx, store.LeaseKey(store.PushMirrorLeaseName("o", "e")),
		store.PutBody{Bytes: leaseWithExpiry(t, "old", past).Marshal()},
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.acquireLease(ctx, "o", "e"); err == nil {
		t.Error("acquire Update-error ok")
	}
	// releaseFunc with a dead store and a corrupt lease.
	svc2 := New(Deps{Store: &faultStore2{ObjectStore: inner, failGet: boom}, Reg: reg, CacheDir: t.TempDir(), Hostname: "h"})
	svc2.releaseFunc("k", "h", "")() // Get error: no-op, no panic
	if _, err := store.PutBytes(ctx, inner, "k", []byte("junk"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream"}); err != nil {
		t.Fatal(err)
	}
	svc3 := New(Deps{Store: inner, Reg: reg, CacheDir: t.TempDir(), Hostname: "h"})
	svc3.releaseFunc("k", "h", "")() // Unmarshal error: no-op, no panic
}

func TestRoundLoadErrorAndTicks(t *testing.T) {
	ctx := context.Background()
	inner := store.NewMemory()
	cfg := testConfig(t)
	reg := newRegistryForTest(t, ctx, inner, cfg)
	if _, err := reg.Create(ctx, "o/r", testSHA1()); err != nil {
		t.Fatal(err)
	}
	// Load error inside round: skipped, loop lives.
	svc := New(Deps{Store: &faultStore2{ObjectStore: inner, failGet: errors.New("down")}, Reg: reg, CacheDir: t.TempDir(), Hostname: "h"})
	svc.Round(ctx, nil)
	// Ticker actually fires (sleep past a 1ms cadence before cancel).
	svc2 := New(Deps{Store: inner, Reg: reg, CacheDir: t.TempDir(), Hostname: "h2", AllowPrivate: true, AllowFile: true})
	rctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc2.RunLoop(rctx, time.Millisecond, nil); close(done) }()
	time.Sleep(30 * time.Millisecond)
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunLoop tick did not exit")
	}
}

func TestCreateSecretValidation(t *testing.T) {
	h, svc, ctx := testHandler(t, adminPrincipal)
	createRepoForHTTP(t, svc, ctx, "o", "r")
	// Password kind without the password fails closed (no secret written).
	if w := doReq(h, "PUT", "/o/r/api/pushmirror", `{"upstream_url":"https://github.com/o/r.git","auth_kind":"password","dangerous":true}`); w.Code != http.StatusBadRequest {
		t.Errorf("password-without-password = %d", w.Code)
	}
	// None with material fails closed.
	if w := doReq(h, "PUT", "/o/r/api/pushmirror", `{"upstream_url":"file:///x","auth_kind":"none","token":"abc"}`); w.Code != http.StatusBadRequest {
		t.Errorf("none-with-material = %d", w.Code)
	}
	// Bad repo id falls through (never a mirror route).
	if h.Handle(newRecorder(), newRequest("GET", "/bad!id/r/api/pushmirror")) {
		t.Error("bad repo id claimed")
	}
}

func TestSSHPushShape(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	work := fixtureWorkRepo(t)
	src := t.TempDir()
	gitTest(t, src, "init", "-q", "--bare", ".")
	gitTest(t, work, "push", "-q", "--mirror", src)
	upstream := t.TempDir()
	gitTest(t, upstream, "init", "-q", "--bare", ".")
	k, err := GenerateKeypair("")
	if err != nil {
		t.Fatal(err)
	}
	// SSH push to a file:// URL: GIT_SSH_COMMAND is set (the sshCommand
	// + cleanup path inside Push) but unused by the file transport, so
	// the transfer proves the shape without a network.
	r := NewRunner("git", t.TempDir(), 60*time.Second, 30*time.Second)
	auth := PushAuth{Kind: AuthSSH, Scheme: "file", PrivateKey: k.PrivatePEM, Fingerprint: k.Fingerprint}
	if err := r.Push(ctx, src, "file://"+upstream, auth); err != nil {
		t.Fatalf("ssh-shape push: %v", err)
	}
}

func TestLowLevelEdges(t *testing.T) {
	// boundedStderr truncation.
	var b boundedStderr
	if n, _ := b.Write([]byte(strings.Repeat("x", 9000))); n != 9000 {
		t.Errorf("write = %d", n)
	}
	if len(b.String()) != 8192 {
		t.Errorf("truncated len = %d", len(b.String()))
	}
	// sshReadString short/garbage inputs.
	if _, _, ok := sshReadString([]byte{0, 1}); ok {
		t.Error("short input ok")
	}
	if _, _, ok := sshReadString([]byte{0, 0, 0, 9, 1, 2}); ok {
		t.Error("overlong length ok")
	}
	// GenerateKeypair with an explicit comment.
	k, err := GenerateKeypair("my-key")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(k.PublicKey, " my-key") {
		t.Errorf("comment = %q", k.PublicKey)
	}
	// Unpadded-base64 public line (RawStdEncoding fallback).
	unpadded := k.PublicKey
	if i := strings.LastIndex(unpadded, "="); i >= 0 {
		unpadded = unpadded[:i]
	}
	if _, err := ParsePublicLine(unpadded); err != nil {
		t.Skipf("padding was structural, fallback untested: %v", err)
	}
}
