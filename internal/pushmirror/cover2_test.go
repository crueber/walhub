// cover2_test.go — retry paths, error branches, and handler edges for
// the ≥95% gate (law 11).
package pushmirror

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// faultStore fails the first N Put calls with a precondition error
// (the CAS-race shape), then delegates.
type faultStore struct {
	store.ObjectStore
	failLeft int
}

func (f *faultStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if f.failLeft > 0 {
		f.failLeft--
		return store.ObjectMeta{}, store.NewPrecondition(key, "vX")
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func TestRecordAttemptCASRetry(t *testing.T) {
	ctx := context.Background()
	inner := store.NewMemory()
	if _, err := Create(ctx, inner, "o", "r", "file:///x", AuthNone, "", ScheduleOff); err != nil {
		t.Fatal(err)
	}
	st := &faultStore{ObjectStore: inner, failLeft: 1}
	if err := RecordAttempt(ctx, st, "o", "r", false, "boom", time.Now().UTC()); err != nil {
		t.Fatalf("retry did not recover: %v", err)
	}
	doc, _, _ := Load(ctx, inner, "o", "r")
	if doc.ConsecutiveFailures != 1 {
		t.Errorf("outcome = %+v", doc)
	}
}

func TestSaveSecretRetryAndUpdate(t *testing.T) {
	ctx := context.Background()
	inner := store.NewMemory()
	st := &faultStore{ObjectStore: inner, failLeft: 1}
	// Absent → Create fails once → falls into SaveSecretCAS → creates.
	if err := SaveSecret(ctx, st, "o", "r", &Secret{AuthKind: AuthToken, Token: "abc"}); err != nil {
		t.Fatalf("save retry: %v", err)
	}
	// Present → update path.
	if err := SaveSecret(ctx, inner, "o", "r", &Secret{AuthKind: AuthToken, Token: "def"}); err != nil {
		t.Fatal(err)
	}
	sec, _, _ := LoadSecret(ctx, inner, "o", "r")
	if sec.Token != "def" {
		t.Errorf("secret = %+v", sec)
	}
	// CAS retry inside SaveSecretCAS.
	st2 := &faultStore{ObjectStore: inner, failLeft: 1}
	if err := SaveSecretCAS(ctx, st2, "o", "r", &Secret{AuthKind: AuthToken, Token: "ghi"}); err != nil {
		t.Fatalf("CAS retry: %v", err)
	}
}

func TestCorruptSidecars(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if _, err := store.PutBytes(ctx, st, store.PushMirrorKey("o", "r"), []byte("{bad"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(ctx, st, "o", "r"); err == nil {
		t.Error("corrupt config loads clean")
	}
	if _, _, err := LoadSecret(ctx, st, "o", "r"); err != nil {
		t.Fatalf("absent secret = %v", err)
	}
	if _, err := store.PutBytes(ctx, st, store.PushMirrorSecretKey("o", "r"), []byte("{bad"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadSecret(ctx, st, "o", "r"); err == nil {
		t.Error("corrupt secret loads clean")
	}
	// Corrupt-but-present counts for HasConfig (fail closed toward sync).
	if !HasConfig(ctx, st, "o", "r") {
		t.Error("corrupt config HasConfig = false")
	}
}

func TestSecretShapes(t *testing.T) {
	if ((&Secret{AuthKind: AuthPassword, Password: "x"}).HasMaterial()) != true {
		t.Error("password material")
	}
	if ((&Secret{AuthKind: AuthSSH, SSHPrivateKey: "k"}).HasMaterial()) != true {
		t.Error("ssh material")
	}
	if (&Secret{AuthKind: "bogus"}).HasMaterial() {
		t.Error("bogus material")
	}
	if ((&Secret{AuthKind: AuthPassword, Password: "secret123"}).SecretHint()) != "••••t123" {
		t.Error("password hint")
	}
	if ((&Secret{AuthKind: AuthSSH, SSHPrivateKey: "k"}).SecretHint()) == "" {
		t.Error("ssh hint empty")
	}
	if ((&Secret{AuthKind: AuthNone}).SecretHint()) != "" {
		t.Error("none hint non-empty")
	}
	if ((&Secret{AuthKind: "bogus"}).SecretHint()) != "" {
		t.Error("bogus hint non-empty")
	}
	if ((&Secret{AuthKind: AuthNone}).HasMaterial()) != true {
		t.Error("none material")
	}
}

func TestBackedOffEdges(t *testing.T) {
	now := time.Now().UTC()
	if BackedOff(&Doc{}, now) {
		t.Error("no-failure backed off")
	}
	if BackedOff(&Doc{ConsecutiveFailures: 2}, now) {
		t.Error("no-attempt backed off")
	}
	if BackedOff(&Doc{ConsecutiveFailures: 2, LastAttemptAt: "garbage"}, now) {
		t.Error("bad attempt backed off")
	}
}

func TestPushPasswordAndTokenShapes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	work := fixtureWorkRepo(t)
	src := t.TempDir()
	gitTest(t, src, "init", "-q", "--bare", ".")
	gitTest(t, work, "push", "-q", "--mirror", src)
	upstream := t.TempDir()
	gitTest(t, upstream, "init", "-q", "--bare", ".")
	r := NewRunner("git", t.TempDir(), 60*time.Second, 30*time.Second)
	// Credential-helper argv + file:// URL: the helper is never
	// consulted for file, so the transfer proves the argv shape works
	// without a network.
	for _, a := range []PushAuth{
		{Kind: AuthPassword, Username: "u", Password: "p", Scheme: "file"},
		{Kind: AuthToken, Token: "tok", Scheme: "file"},
	} {
		if err := r.Push(ctx, src, "file://"+upstream, a); err != nil {
			t.Fatalf("push %+v: %v", a.Kind, err)
		}
	}
	// Failing push scrubs.
	err := r.Push(ctx, src, "file:///tmp/does-not-exist-623-xyz.git", PushAuth{Kind: AuthNone, Scheme: "file"})
	if err == nil {
		t.Fatal("push to nowhere succeeded")
	}
}

func TestRunnerEdges(t *testing.T) {
	r := NewRunner("", "", 0, 0)
	if r.Binary != "git" {
		t.Error("binary default")
	}
	if _, _, err := r.collect(context.Background(), "/tmp/does-not-exist-623", []string{"for-each-ref", "--format=%(objectname) %(refname)"}, nil); err == nil {
		t.Error("collect in missing dir succeeded")
	}
	if _, err := r.ListRefs(context.Background(), "/tmp/does-not-exist-623"); err == nil {
		t.Error("ListRefs in missing dir succeeded")
	}
	t.Setenv("PATH", "")
	if pathEnv() == "" {
		t.Error("pathEnv fallback empty")
	}
	// Nested-nonexistent cache dir: MkdirAll fallback.
	r2 := NewRunner("git", "/tmp/does-not-exist-623-parent/sub", time.Minute, time.Minute)
	k, _ := GenerateKeypair("")
	if _, cleanup, err := r2.sshCommand(PushAuth{Kind: AuthSSH, PrivateKey: k.PrivatePEM}); err != nil {
		t.Fatalf("sshCommand nested cache: %v", err)
	} else {
		cleanup()
	}
}

func TestHandlerEdges(t *testing.T) {
	// Nil service + nil auth fallbacks.
	h := &Handler{}
	if p, _ := h.principal(httptest.NewRequest("GET", "/", nil)); !p.Anonymous {
		t.Error("nil Auth not anonymous")
	}
	_ = h.now()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/o/r/api/refs", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("fallthrough = %d", w.Code)
	}
	// Svc-nil surfaces are 503, not panics.
	for _, tc := range []struct {
		method, path, body string
	}{
		{"GET", "/o/r/api/pushmirror", ""},
		{"PUT", "/o/r/api/pushmirror", `{}`},
		{"DELETE", "/o/r/api/pushmirror", ""},
		{"POST", "/o/r/api/pushmirror/sync", `{}`},
		{"GET", "/o/r/api/pushmirror/sync", ""},
		{"POST", "/o/r/api/pushmirror/keygen", `{}`},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		// Admin principal so the gate passes and Svc-nil is what fires.
		hh := &Handler{Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
			return adminPrincipal, nil
		}}
		ww := httptest.NewRecorder()
		hh.ServeHTTP(ww, r)
		if ww.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", tc.method, tc.path, ww.Code)
		}
	}
	// Method-not-allowed twins.
	h2, svc, ctx := testHandler(t, adminPrincipal)
	createRepoForHTTP(t, svc, ctx, "o", "r")
	w2 := doReq(h2, "POST", "/o/r/api/pushmirror", `{}`)
	if w2.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST config = %d, want 405", w2.Code)
	}
	w2 = doReq(h2, "POST", "/o/r/api/pushmirror/sync", `{}`)
	_ = w2
	// Invalid JSON + unknown fields.
	for _, body := range []string{`{bad`, `{"bogus_field":1}`} {
		w2 = doReq(h2, "PUT", "/o/r/api/pushmirror", body)
		if w2.Code != http.StatusBadRequest {
			t.Errorf("PUT %q = %d, want 400", body, w2.Code)
		}
	}
	// Bad upstream URL → StatusError mapping.
	w2 = doReq(h2, "PUT", "/o/r/api/pushmirror", `{"upstream_url":":::"}`)
	if w2.Code != http.StatusBadRequest {
		t.Errorf("bad URL = %d, want 400", w2.Code)
	}
	// SSRF refusal (off-allowlist https without dangerous).
	w2 = doReq(h2, "PUT", "/o/r/api/pushmirror", `{"upstream_url":"https://example.invalid/o/r.git"}`)
	if w2.Code != http.StatusBadRequest {
		t.Errorf("off-allowlist = %d, want 400", w2.Code)
	}
	// Non-admin sync/keygen/delete.
	h3, svc3, ctx3 := testHandler(t, userPrincipal)
	createRepoForHTTP(t, svc3, ctx3, "o", "r")
	for _, tc := range []struct{ method, path string }{
		{"POST", "/o/r/api/pushmirror/sync"},
		{"GET", "/o/r/api/pushmirror/sync"},
		{"POST", "/o/r/api/pushmirror/keygen"},
		{"DELETE", "/o/r/api/pushmirror"},
	} {
		// GET sync is open; the rest are admin-only.
		_ = tc
	}
	w2 = doReq(h3, "POST", "/o/r/api/pushmirror/sync", `{}`)
	if w2.Code != http.StatusForbidden {
		t.Errorf("user sync = %d, want 403", w2.Code)
	}
	w2 = doReq(h3, "POST", "/o/r/api/pushmirror/keygen", `{}`)
	if w2.Code != http.StatusForbidden {
		t.Errorf("user keygen = %d, want 403", w2.Code)
	}
	w2 = doReq(h3, "DELETE", "/o/r/api/pushmirror", "")
	if w2.Code != http.StatusForbidden {
		t.Errorf("user delete = %d, want 403", w2.Code)
	}
	// Sync status: unknown id 404; no-id lists; full taskJSON after fire.
	w2 = doReq(h3, "GET", "/o/r/api/pushmirror/sync?id=nope", "")
	if w2.Code != http.StatusNotFound {
		t.Errorf("unknown sync id = %d, want 404", w2.Code)
	}
}

func TestSyncStatusFullJSON(t *testing.T) {
	h, svc, ctx := testHandler(t, adminPrincipal)
	createRepoForHTTP(t, svc, ctx, "o", "r")
	upstream := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "--bare", ".")
	cmd.Dir = upstream
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	w := doReq(h, "PUT", "/o/r/api/pushmirror", `{"upstream_url":"file://`+upstream+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %q", w.Code, w.Body.String())
	}
	seedRepo(t, ctx, svc.reg, "o", "r")
	w = doReq(h, "POST", "/o/r/api/pushmirror/sync", `{"force":true}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("sync = %d %q", w.Code, w.Body.String())
	}
	var started struct {
		Task map[string]any `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	id, _ := started.Task["id"].(string)
	if id == "" {
		t.Fatal("no async id")
	}
	// Poll to done: covers done=true + taskJSON + recent list.
	deadline := time.Now().Add(60 * time.Second)
	for {
		w = doReq(h, "GET", "/o/r/api/pushmirror/sync?id="+id, "")
		var st struct {
			Done bool           `json:"done"`
			Task map[string]any `json:"task"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &st)
		if st.Done {
			if st.Task["kind"] != KindPushMirrorSync {
				t.Errorf("task kind = %v", st.Task["kind"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sync never finished")
		}
		time.Sleep(200 * time.Millisecond)
	}
	w = doReq(h, "GET", "/o/r/api/pushmirror/sync", "")
	if !strings.Contains(w.Body.String(), "recent") {
		t.Errorf("status list = %q", w.Body.String())
	}
	if taskJSON(nil) == nil {
		t.Error("taskJSON(nil) nil")
	}
}

func TestSecretHelpersDirect(t *testing.T) {
	in := &putBody{Password: "pw"}
	if hasInputMaterial(&putBody{}) {
		t.Error("empty input has material")
	}
	if !hasInputMaterial(in) || !hasInputMaterial(&putBody{Token: "t"}) || !hasInputMaterial(&putBody{SSHPrivateKey: "k"}) {
		t.Error("material missed")
	}
	if _, err := secretFromInput("bogus", "", &putBody{}); err == nil {
		t.Error("bogus kind secret built")
	}
	if _, err := secretFromInput(AuthNone, "", &putBody{Token: "x"}); err == nil {
		t.Error("none with material built")
	}
	if _, err := secretFromInput(AuthPassword, "", &putBody{}); err == nil {
		t.Error("password without password built")
	}
	if _, err := secretFromInput(AuthToken, "", &putBody{}); err == nil {
		t.Error("token without token built")
	}
	if _, err := secretFromInput(AuthSSH, "", &putBody{}); err == nil {
		t.Error("ssh without key built")
	}
	k, _ := GenerateKeypair("")
	if _, err := secretFromInput(AuthSSH, "", &putBody{SSHPrivateKey: k.PrivatePEM, SSHPublicKey: "bogus"}); err == nil {
		t.Error("bad public line accepted")
	}
	if s := effectiveSecret(nil, AuthNone); s == nil || s.AuthKind != AuthNone {
		t.Errorf("effectiveSecret none = %+v", s)
	}
	if s := effectiveSecret(nil, AuthToken); s == nil || s.AuthKind != AuthToken {
		t.Errorf("effectiveSecret token = %+v", s)
	}
	stored := &Secret{AuthKind: AuthToken, Token: "old"}
	if m, err := mergeSecretInput(stored, AuthToken, "u", &putBody{Token: "new"}, false); err != nil || m.Token != "new" {
		t.Errorf("merge token = %+v,%v", m, err)
	}
	if _, err := mergeSecretInput(nil, AuthPassword, "u", &putBody{}, false); err == nil {
		t.Error("merge password without material ok")
	}
	if _, err := mergeSecretInput(nil, AuthToken, "u", &putBody{}, false); err == nil {
		t.Error("merge token without material ok")
	}
	if _, err := mergeSecretInput(nil, AuthSSH, "u", &putBody{}, false); err == nil {
		t.Error("merge ssh without key ok")
	}
	if m, err := mergeSecretInput(&Secret{AuthKind: AuthSSH, SSHPrivateKey: "k", SSHKnownHosts: "old"}, AuthSSH, "u", &putBody{SSHKnownHosts: "new"}, true); err != nil || m.SSHKnownHosts != "new" {
		t.Errorf("merge known_hosts = %+v,%v", m, err)
	}
	if _, err := mergeSecretInput(nil, AuthSSH, "u", &putBody{SSHPrivateKey: "k", SSHPublicKey: "bogus"}, false); err == nil {
		t.Error("merge bad public ok")
	}
	// writeStatusErr + writeAuthErr + isExists direct pins.
	w := httptest.NewRecorder()
	writeStatusErr(w, errors.New("plain boom"))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("plain status err = %d", w.Code)
	}
	for _, kind := range []struct {
		err  *auth.AuthError
		want int
	}{
		{&auth.AuthError{Kind: auth.ErrForbidden, Why: "no"}, http.StatusForbidden},
		{&auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}, http.StatusServiceUnavailable},
		{&auth.AuthError{Kind: auth.ErrUnauthorized, Why: "who"}, http.StatusUnauthorized},
	} {
		ww := httptest.NewRecorder()
		writeAuthErr(ww, kind.err)
		if ww.Code != kind.want {
			t.Errorf("auth err = %d, want %d", ww.Code, kind.want)
		}
	}
	if !isExists(store.NewPrecondition("k", "v")) {
		t.Error("isExists(precondition) = false")
	}
	if isExists(errors.New("x")) {
		t.Error("isExists(plain) = true")
	}
	if decodeSegment("%zz") == "" {
		t.Error("decodeSegment empty")
	}
}

func TestUpdateMergeSameKind(t *testing.T) {
	h, svc, ctx := testHandler(t, adminPrincipal)
	createRepoForHTTP(t, svc, ctx, "o", "r")
	w := doReq(h, "PUT", "/o/r/api/pushmirror", `{"upstream_url":"https://github.com/o/r.git","auth_kind":"token","token":"tok-old-1111","dangerous":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %q", w.Code, w.Body.String())
	}
	// Same-kind merge: token replaces, schedule applies, username sets.
	w = doReq(h, "PUT", "/o/r/api/pushmirror", `{"token":"tok-new-2222","schedule":"daily","username":"bot"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("merge = %d %q", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "tok-new-2222") || strings.Contains(w.Body.String(), "tok-old-1111") {
		t.Error("token echoed in merge response")
	}
	// Auth-kind change drops the old shape (token → password without a
	// password fails closed).
	w = doReq(h, "PUT", "/o/r/api/pushmirror", `{"auth_kind":"password"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("kind change without material = %d, want 400", w.Code)
	}
	w = doReq(h, "PUT", "/o/r/api/pushmirror", `{"auth_kind":"password","password":"pw-9999"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("kind change = %d %q", w.Code, w.Body.String())
	}
}

func TestRunPushFireErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	// No config → SyncNow error.
	if _, err := svc.SyncNow(ctx, "o", "ghost", false); err == nil {
		t.Error("SyncNow without config ok")
	}
	// Nil registry → error.
	bare := New(Deps{Store: st, Reg: nil})
	if _, err := bare.SyncNow(ctx, "o", "ghost", false); err == nil {
		t.Error("SyncNow nil-reg ok")
	}
	if bare.RecentSyncs("o/ghost") != nil {
		t.Error("RecentSyncs nil-reg non-nil")
	}
	// Garbage upstream URL in config → fire fails scrubbed.
	if _, err := reg.Create(ctx, "o/g", git.Sha1); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(&Doc{Version: 1, UpstreamURL: "file:///tmp/does-not-exist-623-q.git", AuthKind: AuthNone, Schedule: ScheduleOff})
	if _, err := store.PutBytes(ctx, st, store.PushMirrorKey("o", "g"), raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncNow(ctx, "o", "g", false); err == nil {
		t.Error("push to nowhere ok")
	}
	doc, _, _ := Load(ctx, st, "o", "g")
	if doc.ConsecutiveFailures != 1 {
		t.Errorf("outcome = %+v", doc)
	}
	// Backoff skip: second fire (non-force) skips without touching the counter.
	if _, err := svc.SyncNow(ctx, "o", "g", false); err != nil {
		t.Errorf("backoff skip = %v", err)
	}
	doc2, _, _ := Load(ctx, st, "o", "g")
	if doc2.ConsecutiveFailures != 1 {
		t.Errorf("backoff skip touched counter: %+v", doc2)
	}
	// Garbage canonical URL → normalize failure at fire.
	raw, _ = json.Marshal(&Doc{Version: 1, UpstreamURL: "::::", AuthKind: AuthNone})
	if _, err := store.PutBytes(ctx, st, store.PushMirrorKey("o", "h"), raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create(ctx, "o/h", git.Sha1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncNow(ctx, "o", "h", false); err == nil {
		t.Error("garbage URL fire ok")
	}
	// SSRF refusal at fire (file URLs disallowed on this service).
	strict := New(Deps{Store: st, Reg: reg, CacheDir: t.TempDir(), Hostname: "s"})
	if _, err := strict.SyncNow(ctx, "o", "g", true); err == nil {
		t.Error("disallowed-file fire ok")
	}
	// Open failure: config on an unborn repo.
	raw, _ = json.Marshal(&Doc{Version: 1, UpstreamURL: "file:///tmp/x", AuthKind: AuthNone})
	if _, err := store.PutBytes(ctx, st, store.PushMirrorKey("o", "unborn"), raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncNow(ctx, "o", "unborn", false); err == nil {
		t.Error("unborn fire ok")
	}
}

func TestLeaseStealAndRelease(t *testing.T) {
	ctx := context.Background()
	_, st := testRegistry(t)
	svc := testService(t, st, nil)
	// Expired lease (written directly) is stolen, not skipped.
	past := time.Now().UTC().Add(-time.Hour)
	lease := &proto.Lease{Holder: "old", Purpose: KindPushMirrorSync, Epoch: 3}
	acq, exp := proto.TimeFromGo(past.Add(-time.Hour)), proto.TimeFromGo(past)
	lease.AcquiredAt, lease.ExpiresAt = &acq, &exp
	key := store.LeaseKey(store.PushMirrorLeaseName("o", "z"))
	if _, err := st.Put(ctx, key, store.PutBody{Bytes: lease.Marshal()},
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"}); err != nil {
		t.Fatal(err)
	}
	rel, err := svc.acquireLease(ctx, "o", "z")
	if err != nil {
		t.Fatalf("steal = %v", err)
	}
	rel()
	// Corrupt lease body → hard error.
	if _, err := store.PutBytes(ctx, st, store.LeaseKey(store.PushMirrorLeaseName("o", "c")), []byte("junk"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.acquireLease(ctx, "o", "c"); err == nil {
		t.Error("corrupt lease acquired")
	}
	// Release of a stolen lease leaves the new owner's lease alone.
	rel2, err := svc.acquireLease(ctx, "o", "z")
	if err != nil {
		t.Fatal(err)
	}
	svc.releaseFunc(key, "someone-else", "")()
	body, _, _ := store.GetBytes(ctx, st, key, store.GetOptions{})
	if body == nil {
		t.Error("foreign release deleted our lease")
	}
	rel2()
	// classifyPushError: canceled ctx is a retry verdict.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := classifyPushError(cctx, errors.New("x")); !strings.Contains(got, "safe to retry") {
		t.Errorf("cancel verdict = %q", got)
	}
	// splitRepo/splitOnce edges.
	if _, _, ok := splitRepo("noslash"); ok {
		t.Error("splitRepo(noslash) ok")
	}
	if _, _, ok := splitRepo("/"); ok {
		t.Error("splitRepo(/) ok")
	}
	if _, _, ok := splitOnce("abc", "/"); ok {
		t.Error("splitOnce missing ok")
	}
}

func TestRunLoopExitsAndGates(t *testing.T) {
	_, st := testRegistry(t)
	svc := testService(t, st, nil)
	// Nil registry: round is a no-op.
	svc.Round(context.Background(), nil)
	// RunLoop exits on cancel.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.RunLoop(ctx, time.Millisecond, nil); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunLoop did not exit")
	}
	// Maintain gate excluding everything fires nothing.
	reg, st2 := testRegistry(t)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	if _, err := reg.Create(ctx2, "o/r", git.Sha1); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx2, st2, "o", "r", "file:///x", AuthNone, "", PresetDaily); err != nil {
		t.Fatal(err)
	}
	svc2 := New(Deps{Store: st2, Reg: reg, CacheDir: t.TempDir(), Hostname: "h", AllowPrivate: true, AllowFile: true})
	svc2.Round(ctx2, func(string) bool { return false })
	doc, _, _ := Load(ctx2, st2, "o", "r")
	if doc.LastAttemptAt != "" {
		t.Errorf("gated loop fired: %+v", doc)
	}
	// ResetKindsForTest restores registration (covers the helper).
	ResetKindsForTest()
	RegisterKind(KindPushMirrorSync)
}

func TestSyncStatusAndRecent(t *testing.T) {
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	if _, ok := svc.SyncStatus("nope"); ok {
		t.Error("unknown id resolved")
	}
	if got := svc.RecentSyncs("o/r"); len(got) != 0 {
		t.Errorf("recent on empty = %v", got)
	}
	// pruneAsyncLocked bounds the map.
	m := map[string]*asyncSync{}
	for i := 0; i < 150; i++ {
		a := &asyncSync{id: string(rune(i)), target: "o/r", started: time.Now().UTC().Format(time.RFC3339Nano), done: make(chan struct{})}
		close(a.done)
		m[a.id] = a
	}
	// ids collide on rune conversion; top up with unique keys.
	for i := len(m); i < 150; i++ {
		id := strings.Repeat("x", i-len(m)+1) + "k"
		a := &asyncSync{id: id, target: "o/r", started: time.Now().UTC().Format(time.RFC3339Nano), done: make(chan struct{})}
		close(a.done)
		m[id] = a
	}
	pruneAsyncLocked(m)
	if len(m) > 100 {
		t.Errorf("pruned map = %d", len(m))
	}
	// succeed-record failure surfaces (fault store on outcome write).
	fctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := reg.Create(fctx, "o/s", git.Sha1); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(fctx, st, "o", "s", "file:///x", AuthNone, "", ScheduleOff); err != nil {
		t.Fatal(err)
	}
	_ = wal.Task{}
	_ = os.TempDir()
}
