// cover4_test.go — third coverage pass: reader faults, anon gates,
// key-scoped store faults, and task-body error branches.
package pushmirror

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	return cfg
}

// errReader fails every read (the "unreadable body" branch).
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read boom") }

// keyFault fails Gets/Puts whose key contains a substring, else delegates.
// getLeft fails the first N Gets (any key), then delegates. getSeq, when
// non-empty, scripts per-Get outcomes in order (nil = delegate, err =
// fail) — e.g. [nil, boom] lets SyncNow's Load through and fails
// runPush's Load.
type keyFault struct {
	store.ObjectStore
	getKey  string
	putKey  string
	getLeft int
	getSeq  []error
}

func (f *keyFault) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if len(f.getSeq) > 0 {
		err := f.getSeq[0]
		f.getSeq = f.getSeq[1:]
		if err != nil {
			return nil, err
		}
		return f.ObjectStore.Get(ctx, key, opts)
	}
	if f.getLeft > 0 {
		f.getLeft--
		return nil, errors.New("get boom")
	}
	if f.getKey != "" && strings.Contains(key, f.getKey) {
		return nil, errors.New("get boom")
	}
	return f.ObjectStore.Get(ctx, key, opts)
}

func (f *keyFault) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if f.putKey != "" && strings.Contains(key, f.putKey) {
		return store.ObjectMeta{}, errors.New("put boom")
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func TestUnreadableBodies(t *testing.T) {
	h, svc, ctx := testHandler(t, adminPrincipal)
	createRepoForHTTP(t, svc, ctx, "o", "r")
	for _, tc := range []struct{ method, path string }{
		{"PUT", "/o/r/api/pushmirror"},
		{"POST", "/o/r/api/pushmirror/sync"},
		{"POST", "/o/r/api/pushmirror/keygen"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, errReader{})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s %s unreadable = %d, want 400", tc.method, tc.path, w.Code)
		}
	}
}

func TestAnonGates(t *testing.T) {
	h, svc, ctx := testHandler(t, anonPrincipal)
	createRepoForHTTP(t, svc, ctx, "o", "r")
	for _, tc := range []struct{ method, path, body string }{
		{"PUT", "/o/r/api/pushmirror", `{}`},
		{"DELETE", "/o/r/api/pushmirror", ""},
		{"POST", "/o/r/api/pushmirror/sync", `{}`},
		{"POST", "/o/r/api/pushmirror/keygen", `{}`},
	} {
		w := doReq(h, tc.method, tc.path, tc.body)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s anon = %d, want 401", tc.method, tc.path, w.Code)
		}
	}
}

func TestCreateBranches(t *testing.T) {
	h, svc, ctx := testHandler(t, adminPrincipal)
	createRepoForHTTP(t, svc, ctx, "o", "r")
	// Missing upstream.
	if w := doReq(h, "PUT", "/o/r/api/pushmirror", `{}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing upstream = %d", w.Code)
	}
	// Bad kind / bad schedule.
	if w := doReq(h, "PUT", "/o/r/api/pushmirror", `{"upstream_url":"file:///x","auth_kind":"bogus"}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad kind = %d", w.Code)
	}
	if w := doReq(h, "PUT", "/o/r/api/pushmirror", `{"upstream_url":"file:///x","schedule":"bogus"}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad schedule = %d", w.Code)
	}
	// Generic (non-412) Create failure → 500.
	inner := store.NewMemory()
	cfg := testConfig(t)
	reg := wal.NewRegistry(context.Background(), inner, cfg)
	t.Cleanup(reg.Close)
	if _, err := reg.Create(ctx, "o/g", git.Sha1); err != nil {
		t.Fatal(err)
	}
	svcG := New(Deps{Store: &keyFault{ObjectStore: inner, putKey: "pushmirror.json"}, Reg: reg, CacheDir: t.TempDir(), Hostname: "h", AllowPrivate: true, AllowFile: true})
	hG := &Handler{Svc: svcG, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return adminPrincipal, nil
	}}
	if w := doReq(hG, "PUT", "/o/g/api/pushmirror", `{"upstream_url":"file:///x"}`); w.Code != http.StatusInternalServerError {
		t.Errorf("create put-fail = %d, want 500", w.Code)
	}
	// Create conflict → 409 (pre-created config races the handler).
	if _, err := Create(ctx, inner, "o", "h", "file:///x", AuthNone, "", ScheduleOff); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create(ctx, "o/h", git.Sha1); err != nil {
		t.Fatal(err)
	}
	// Direct double-Create pins the 412→409 mapping helper path.
	if _, err := Create(ctx, inner, "o", "h", "file:///x", AuthNone, "", ScheduleOff); !store.IsPreconditionFailed(err) {
		t.Errorf("double create = %v, want 412", err)
	}
}

func TestUpdateBranches(t *testing.T) {
	h, svc, ctx := testHandler(t, adminPrincipal)
	createRepoForHTTP(t, svc, ctx, "o", "r")
	if w := doReq(h, "PUT", "/o/r/api/pushmirror", `{"upstream_url":"file:///x"}`); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	// Same-spelling upstream is not a change (canonical compare).
	if w := doReq(h, "PUT", "/o/r/api/pushmirror", `{"upstream_url":"file:///x"}`); w.Code != http.StatusOK {
		t.Errorf("same upstream = %d, want 200", w.Code)
	}
	// Bad kind on update.
	if w := doReq(h, "PUT", "/o/r/api/pushmirror", `{"auth_kind":"bogus"}`); w.Code != http.StatusBadRequest {
		t.Errorf("update bad kind = %d", w.Code)
	}
	// UpdateCAS failure → 500.
	inner := store.NewMemory()
	cfg := testConfig(t)
	reg := wal.NewRegistry(context.Background(), inner, cfg)
	t.Cleanup(reg.Close)
	if _, err := reg.Create(ctx, "o/u", git.Sha1); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, inner, "o", "u", "file:///x", AuthNone, "", ScheduleOff); err != nil {
		t.Fatal(err)
	}
	svcU := New(Deps{Store: &keyFault{ObjectStore: inner, putKey: "pushmirror.json"}, Reg: reg, CacheDir: t.TempDir(), Hostname: "h", AllowPrivate: true, AllowFile: true})
	hU := &Handler{Svc: svcU, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return adminPrincipal, nil
	}}
	if w := doReq(hU, "PUT", "/o/u/api/pushmirror", `{"schedule":"daily"}`); w.Code != http.StatusInternalServerError {
		t.Errorf("update CAS-fail = %d, want 500", w.Code)
	}
	// Normalize failure at update (stored garbage URL).
	raw, _ := jsonMarshal(&Doc{Version: 1, UpstreamURL: "::::", AuthKind: AuthNone})
	if _, err := store.PutBytes(ctx, inner, store.PushMirrorKey("o", "w"), raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create(ctx, "o/w", git.Sha1); err != nil {
		t.Fatal(err)
	}
	svcW := New(Deps{Store: inner, Reg: reg, CacheDir: t.TempDir(), Hostname: "h", AllowPrivate: true, AllowFile: true})
	hW := &Handler{Svc: svcW, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return adminPrincipal, nil
	}}
	if w := doReq(hW, "PUT", "/o/w/api/pushmirror", `{"schedule":"daily"}`); w.Code != http.StatusInternalServerError {
		t.Errorf("update garbage URL = %d, want 500", w.Code)
	}
	// ValidateTarget failure at update (token kind stranded on file URL).
	raw, _ = jsonMarshal(&Doc{Version: 1, UpstreamURL: "file:///x", AuthKind: AuthToken})
	if _, err := store.PutBytes(ctx, inner, store.PushMirrorKey("o", "v"), raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create(ctx, "o/v", git.Sha1); err != nil {
		t.Fatal(err)
	}
	if w := doReq(hW, "PUT", "/o/v/api/pushmirror", `{"schedule":"daily"}`); w.Code != http.StatusBadRequest {
		t.Errorf("update stranded kind = %d, want 400", w.Code)
	}
}

func TestDeleteFaultAndGetSecretFault(t *testing.T) {
	h, svc, ctx := testHandler(t, adminPrincipal)
	createRepoForHTTP(t, svc, ctx, "o", "r")
	if w := doReq(h, "PUT", "/o/r/api/pushmirror", `{"upstream_url":"file:///x"}`); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	// GET with failing secret load → 500.
	inner := store.NewMemory()
	cfg := testConfig(t)
	reg := wal.NewRegistry(context.Background(), inner, cfg)
	t.Cleanup(reg.Close)
	if _, err := reg.Create(ctx, "o/s", git.Sha1); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, inner, "o", "s", "file:///x", AuthNone, "", ScheduleOff); err != nil {
		t.Fatal(err)
	}
	svcS := New(Deps{Store: &keyFault{ObjectStore: inner, getKey: "secret"}, Reg: reg, CacheDir: t.TempDir(), Hostname: "h", AllowPrivate: true, AllowFile: true})
	hS := &Handler{Svc: svcS, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return adminPrincipal, nil
	}}
	if w := doReq(hS, "GET", "/o/s/api/pushmirror", ""); w.Code != http.StatusInternalServerError {
		t.Errorf("get secret-fail = %d, want 500", w.Code)
	}
	// DELETE with failing delete → 500.
	svcD := New(Deps{Store: &faultStore2{ObjectStore: inner, failDelete: errors.New("down")}, Reg: reg, CacheDir: t.TempDir(), Hostname: "h"})
	hD := &Handler{Svc: svcD, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return adminPrincipal, nil
	}}
	if w := doReq(hD, "DELETE", "/o/s/api/pushmirror", ""); w.Code != http.StatusInternalServerError {
		t.Errorf("delete fail = %d, want 500", w.Code)
	}
}

func TestSyncStatusActiveList(t *testing.T) {
	h, svc, ctx := testHandler(t, adminPrincipal)
	createRepoForHTTP(t, svc, ctx, "o", "r")
	// A planted in-flight entry renders in the active list.
	a := &asyncSync{id: "planted-1", target: "o/r", started: time.Now().UTC().Format(time.RFC3339Nano), done: make(chan struct{})}
	svc.asyncMu.Lock()
	if svc.async == nil {
		svc.async = map[string]*asyncSync{}
	}
	svc.async[a.id] = a
	svc.asyncMu.Unlock()
	w := doReq(h, "GET", "/o/r/api/pushmirror/sync", "")
	if !strings.Contains(w.Body.String(), "planted-1") {
		t.Errorf("active list = %q", w.Body.String())
	}
	close(a.done)
}

func TestKeygenSecretFault(t *testing.T) {
	ctx := context.Background()
	inner := store.NewMemory()
	cfg := testConfig(t)
	reg := wal.NewRegistry(context.Background(), inner, cfg)
	t.Cleanup(reg.Close)
	if _, err := reg.Create(ctx, "o/k", git.Sha1); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, inner, "o", "k", "file:///x", AuthNone, "", ScheduleOff); err != nil {
		t.Fatal(err)
	}
	// UpdateCAS succeeds, secret save fails (key-scoped fault).
	svc := New(Deps{Store: &keyFault{ObjectStore: inner, putKey: "secret"}, Reg: reg, CacheDir: t.TempDir(), Hostname: "h", AllowPrivate: true, AllowFile: true})
	h := &Handler{Svc: svc, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return adminPrincipal, nil
	}}
	if w := doReq(h, "POST", "/o/k/api/pushmirror/keygen", `{}`); w.Code != http.StatusInternalServerError {
		t.Errorf("keygen secret-fail = %d, want 500", w.Code)
	}
}

func TestRunPushGetCountdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	inner := store.NewMemory()
	cfg := testConfig(t)
	reg := wal.NewRegistry(context.Background(), inner, cfg)
	t.Cleanup(reg.Close)
	if _, err := reg.Create(ctx, "o/c", git.Sha1); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, inner, "o", "c", "file:///x", AuthNone, "", ScheduleOff); err != nil {
		t.Fatal(err)
	}
	// SyncNow's Load + the lease probe succeed; runPush's Load fails →
	// the runPush Load-error branch. (Gets in order: SyncNow Load,
	// lease probe, runPush Load.)
	svc := New(Deps{Store: &keyFault{ObjectStore: inner, getSeq: []error{nil, nil, errors.New("get boom")}}, Reg: reg, CacheDir: t.TempDir(), Hostname: "h", AllowPrivate: true, AllowFile: true})
	if _, err := svc.SyncNow(ctx, "o", "c", false); err == nil {
		t.Error("runPush Load-fail ok")
	}
	doc, _, _ := Load(ctx, inner, "o", "c")
	if doc.ConsecutiveFailures != 1 {
		t.Errorf("outcome = %+v", doc)
	}
}

func TestRoundBranches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	inner := store.NewMemory()
	cfg := testConfig(t)
	reg := wal.NewRegistry(context.Background(), inner, cfg)
	t.Cleanup(reg.Close)
	// Due repo whose fire fails at spawn (garbage URL): covers the
	// round's spawn-error branch without stalling the loop.
	if _, err := reg.Create(ctx, "o/g", git.Sha1); err != nil {
		t.Fatal(err)
	}
	raw, _ := jsonMarshal(&Doc{Version: 1, UpstreamURL: "::::", AuthKind: AuthNone, Schedule: PresetDaily})
	if _, err := store.PutBytes(ctx, inner, store.PushMirrorKey("o", "g"), raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	svc := New(Deps{Store: inner, Reg: reg, CacheDir: t.TempDir(), Hostname: "h", AllowPrivate: true, AllowFile: true})
	svc.Round(ctx, nil)
	// Canceled ctx with repos listed: covers the in-loop ctx check.
	cctx, stop := context.WithCancel(context.Background())
	stop()
	svc.Round(cctx, nil)
	// RunLoop with non-positive interval takes the default.
	done := make(chan struct{})
	rctx, rstop := context.WithCancel(context.Background())
	go func() { svc.RunLoop(rctx, 0, nil); close(done) }()
	rstop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunLoop(0) did not exit")
	}
}

func TestRunnerRunCanceled(t *testing.T) {
	r := NewRunner("git", t.TempDir(), time.Minute, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Saturate the pool so the ctx-done arm is the only ready one
	// (an empty pool leaves both arms ready — Go picks randomly).
	for i := 0; i < cap(r.pool); i++ {
		r.pool <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(r.pool); i++ {
			<-r.pool
		}
	}()
	ran := false
	if err := r.run(ctx, func() error { ran = true; return nil }); err == nil || ran {
		t.Errorf("run with canceled ctx = %v ran=%v", err, ran)
	}
	// CacheDir pointing at a file: ssh scratch fails closed.
	f := t.TempDir() + "/afile"
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r2 := NewRunner("git", f, time.Minute, time.Minute)
	k, _ := GenerateKeypair("")
	if _, _, err := r2.sshCommand(PushAuth{Kind: AuthSSH, PrivateKey: k.PrivatePEM}); err == nil {
		t.Error("sshCommand with file cache dir ok")
	}
}

func TestMiscBranches(t *testing.T) {
	// bodyHasKey malformed.
	if bodyHasKey([]byte("{bad"), "schedule") {
		t.Error("malformed body has key")
	}
	// taskJSON with nil LogTail.
	rec := &wal.TaskRecord{ID: "i", Kind: KindPushMirrorSync}
	if out := taskJSON(rec); out["id"] != "i" {
		t.Errorf("taskJSON = %v", out)
	}
	// mergeSecretInput password-set branch.
	m, err := mergeSecretInput(&Secret{AuthKind: AuthPassword}, AuthPassword, "u", &putBody{Password: "newpw"}, false)
	if err != nil || m.Password != "newpw" {
		t.Errorf("merge password = %+v,%v", m, err)
	}
	// decodeSegment bad escape survives verbatim.
	if decodeSegment("%zz") != "%zz" {
		t.Error("decodeSegment mangled")
	}
	// splitPath root shape.
	r := httptest.NewRequest("GET", "/o/r/api/pushmirror", nil)
	if segs := splitPath(r); len(segs) != 4 {
		t.Errorf("splitPath = %v", segs)
	}
	_ = io.Discard
}
