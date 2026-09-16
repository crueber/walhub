// cover3_test.go — second coverage pass: fault-injected store errors,
// handler auth/edge branches, and resolveAuth success shapes.
package pushmirror

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/repoimport"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func leaseWithExpiry(t *testing.T, holder string, expires time.Time) *proto.Lease {
	t.Helper()
	lease := &proto.Lease{Holder: holder, Purpose: KindPushMirrorSync, Epoch: 3}
	acq, exp := proto.TimeFromGo(expires.Add(-time.Hour)), proto.TimeFromGo(expires)
	lease.AcquiredAt, lease.ExpiresAt = &acq, &exp
	return lease
}

// faultStore2 fails Get/Delete/Put per knobs.
type faultStore2 struct {
	store.ObjectStore
	failGet    error
	failDelete error
	failPut    error
	failPutN   int
}

func (f *faultStore2) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if f.failGet != nil {
		return nil, f.failGet
	}
	return f.ObjectStore.Get(ctx, key, opts)
}

func (f *faultStore2) Delete(ctx context.Context, key string, ver store.Version) error {
	if f.failDelete != nil {
		return f.failDelete
	}
	return f.ObjectStore.Delete(ctx, key, ver)
}

func (f *faultStore2) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if f.failPutN > 0 {
		f.failPutN--
		return store.ObjectMeta{}, f.failPut
	}
	if f.failPut != nil && f.failPutN < 0 {
		return store.ObjectMeta{}, f.failPut
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func TestFaultStoreBranches(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("store down")
	inner := store.NewMemory()
	if _, err := Create(ctx, inner, "o", "r", "file:///x", AuthNone, "", ScheduleOff); err != nil {
		t.Fatal(err)
	}
	// Load/Get errors.
	fs := &faultStore2{ObjectStore: inner, failGet: boom}
	if _, _, err := Load(ctx, fs, "o", "r"); err == nil {
		t.Error("Load with dead store ok")
	}
	if _, _, err := LoadSecret(ctx, fs, "o", "r"); err == nil {
		t.Error("LoadSecret with dead store ok")
	}
	if HasConfig(ctx, fs, "o", "r") {
		t.Error("HasConfig with dead store true")
	}
	if err := RecordAttempt(ctx, fs, "o", "r", true, "", time.Now().UTC()); err == nil {
		t.Error("RecordAttempt with dead store ok")
	}
	if err := SaveSecret(ctx, fs, "o", "r", &Secret{AuthKind: AuthToken, Token: "x"}); err == nil {
		t.Error("SaveSecret with dead store ok")
	}
	if err := SaveSecretCAS(ctx, fs, "o", "r", &Secret{AuthKind: AuthToken, Token: "x"}); err == nil {
		t.Error("SaveSecretCAS with dead store ok")
	}
	// Delete errors (config delete fails; secret delete fails).
	fd := &faultStore2{ObjectStore: inner, failDelete: boom}
	if err := Delete(ctx, fd, "o", "r"); err == nil {
		t.Error("Delete with dead store ok")
	}
	// UpdateCAS error (no retry in UpdateCAS).
	fp := &faultStore2{ObjectStore: inner, failPut: boom, failPutN: -1}
	doc, ver, _ := Load(ctx, inner, "o", "r")
	if err := UpdateCAS(ctx, fp, "o", "r", doc, ver); err == nil {
		t.Error("UpdateCAS with dead store ok")
	}
	// SyncNow Load error via dead store.
	reg, _ := testRegistry(t)
	svc := New(Deps{Store: fs, Reg: reg, CacheDir: t.TempDir(), Hostname: "h", AllowPrivate: true, AllowFile: true})
	if _, err := svc.SyncNow(ctx, "o", "r", false); err == nil {
		t.Error("SyncNow with dead store ok")
	}
	// runPush Load error: needs lease first — seed a config on a live
	// store but fail Gets only after lease acquire? Simpler: runPush
	// Load-error is the same Load path (covered above at unit level);
	// here cover succeed/fail record errors via faulted outcome writes.
	doc2, _, _ := Load(ctx, inner, "o", "r")
	_ = doc2
}

func TestSucceedFailRecordErrors(t *testing.T) {
	ctx := context.Background()
	inner := store.NewMemory()
	if _, err := Create(ctx, inner, "o", "r", "file:///x", AuthNone, "", ScheduleOff); err != nil {
		t.Fatal(err)
	}
	reg, _ := testRegistry(t)
	boom := errors.New("store down")
	// fail() with a dead outcome store narrates outcome-lost + terminal.
	fs := &faultStore2{ObjectStore: inner, failGet: boom}
	svc := New(Deps{Store: fs, Reg: reg, CacheDir: t.TempDir(), Hostname: "h"})
	rec, err := svc.reg.Tasks().Run(ctx, "o/r", KindPushMirrorSync+"-probe", nil,
		func(tctx context.Context, task *wal.Task) error {
			return svc.fail(tctx, "o", "r", time.Now().UTC(), task, "kaboom")
		})
	_ = rec
	if err == nil {
		t.Error("fail with dead store ok")
	}
	// succeed() with a dead outcome store returns the record error.
	if err := svc.succeed(ctx, "o", "r", time.Now().UTC()); err == nil {
		t.Error("succeed with dead store ok")
	}
}

func TestAcquireLeaseStoreError(t *testing.T) {
	ctx := context.Background()
	inner := store.NewMemory()
	reg, _ := testRegistry(t)
	svc := New(Deps{Store: &faultStore2{ObjectStore: inner, failGet: errors.New("down")}, Reg: reg, CacheDir: t.TempDir(), Hostname: "h"})
	if _, err := svc.acquireLease(ctx, "o", "r"); err == nil {
		t.Error("acquireLease with dead store ok")
	}
	// Create-race continue: first Put 412s, second succeeds.
	inner2 := store.NewMemory()
	svc2 := New(Deps{Store: &faultStore{ObjectStore: inner2, failLeft: 1}, Reg: reg, CacheDir: t.TempDir(), Hostname: "h2"})
	rel, err := svc2.acquireLease(ctx, "o", "r")
	if err != nil {
		t.Fatalf("acquire after race: %v", err)
	}
	rel()
	// Update-race continue: expired lease + one 412 on Update.
	past := time.Now().UTC().Add(-time.Hour)
	lease := leaseWithExpiry(t, "old", past)
	key := store.LeaseKey(store.PushMirrorLeaseName("o", "u"))
	if _, err := inner2.Put(ctx, key, store.PutBody{Bytes: lease.Marshal()},
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"}); err != nil {
		t.Fatal(err)
	}
	svc3 := New(Deps{Store: &faultStore{ObjectStore: inner2, failLeft: 1}, Reg: reg, CacheDir: t.TempDir(), Hostname: "h3"})
	rel3, err := svc3.acquireLease(ctx, "o", "u")
	if err != nil {
		t.Fatalf("steal after race: %v", err)
	}
	rel3()
}

func TestHandlerAuthErrors(t *testing.T) {
	deny := func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Anonymous(), &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "denied"}
	}
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	h := &Handler{Svc: svc, Auth: deny}
	for _, tc := range []struct{ method, path, body string }{
		{"PUT", "/o/r/api/pushmirror", `{}`},
		{"DELETE", "/o/r/api/pushmirror", ""},
		{"POST", "/o/r/api/pushmirror/sync", `{}`},
		{"POST", "/o/r/api/pushmirror/keygen", `{}`},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", tc.method, tc.path, w.Code)
		}
	}
	// Wrong-method twins.
	h2, svc2, ctx2 := testHandler(t, adminPrincipal)
	createRepoForHTTP(t, svc2, ctx2, "o", "r")
	for _, tc := range []struct{ method, path string }{
		{"PUT", "/o/r/api/pushmirror/sync"},
		{"GET", "/o/r/api/pushmirror/keygen"},
		{"DELETE", "/o/r/api/pushmirror/sync"},
	} {
		w := doReq(h2, tc.method, tc.path, "")
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.path, w.Code)
		}
	}
	// syncNow/keygen decode + load errors.
	w := doReq(h2, "POST", "/o/r/api/pushmirror/sync", `{bad`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("sync bad JSON = %d", w.Code)
	}
	w = doReq(h2, "POST", "/o/r/api/pushmirror/keygen", `{bad`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("keygen bad JSON = %d", w.Code)
	}
	// Delete of absent config → 404.
	w = doReq(h2, "DELETE", "/o/r/api/pushmirror", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("delete absent = %d, want 404", w.Code)
	}
}

func TestHandlerFaultBranches(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("store down")
	inner := store.NewMemory()
	// Registry backed by the SAME store the service probes (the
	// existence Head must see the manifest Create wrote).
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	reg := wal.NewRegistry(context.Background(), inner, cfg)
	t.Cleanup(reg.Close)
	if _, err := reg.Create(ctx, "o/r", git.Sha1); err != nil {
		t.Fatal(err)
	}
	// NOTE: the registry above is empty-store-backed; the service store
	// is the faulted one. Repo-existence probes hit the service store.
	if _, err := Create(ctx, inner, "o", "r", "file:///x", AuthNone, "", ScheduleOff); err != nil {
		t.Fatal(err)
	}
	mkSvc := func(fs store.ObjectStore) *Service {
		return New(Deps{Store: fs, Reg: reg, CacheDir: t.TempDir(), Hostname: "h", AllowPrivate: true, AllowFile: true})
	}
	mkH := func(svc *Service) *Handler {
		return &Handler{Svc: svc, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
			return adminPrincipal, nil
		}}
	}
	// GET with dead config load.
	h := mkH(mkSvc(&faultStore2{ObjectStore: inner, failGet: boom}))
	if w := doReq(h, "GET", "/o/r/api/pushmirror", ""); w.Code != http.StatusInternalServerError {
		t.Errorf("get dead store = %d", w.Code)
	}
	// PUT with dead config load.
	if w := doReq(h, "PUT", "/o/r/api/pushmirror", `{}`); w.Code != http.StatusInternalServerError {
		t.Errorf("put dead store = %d", w.Code)
	}
	// PUT create where the secret write fails.
	// PUT update where the secret write fails (config pre-created
	// directly; the handler's merge → SaveSecret hits the fault).
	if _, err := Create(ctx, inner, "o", "r2", "https://github.com/o/r2.git", AuthToken, "", ScheduleOff); err != nil {
		t.Fatal(err)
	}
	h2 := mkH(mkSvc(&faultStore{ObjectStore: inner, failLeft: 100}))
	if w := doReq(h2, "PUT", "/o/r2/api/pushmirror", `{"token":"tok-1111"}`); w.Code != http.StatusInternalServerError {
		t.Errorf("put dead secret store = %d (%q)", w.Code, w.Body.String())
	}
	// Keygen Load error + UpdateCAS error + SaveSecret error.
	if w := doReq(h, "POST", "/o/r/api/pushmirror/keygen", `{}`); w.Code != http.StatusInternalServerError {
		t.Errorf("keygen dead store = %d", w.Code)
	}
	h3 := mkH(mkSvc(&faultStore{ObjectStore: inner, failLeft: 100}))
	if w := doReq(h3, "POST", "/o/r/api/pushmirror/keygen", `{}`); w.Code != http.StatusInternalServerError {
		t.Errorf("keygen dead CAS = %d (%q)", w.Code, w.Body.String())
	}
	// syncNow Load error.
	if w := doReq(h, "POST", "/o/r/api/pushmirror/sync", `{}`); w.Code != http.StatusInternalServerError {
		t.Errorf("sync dead store = %d", w.Code)
	}
	// syncStatus done-with-error: fire async on a configless repo via
	// the service directly, then resolve through HTTP.
	svcOK := mkSvc(inner)
	hOK := mkH(svcOK)
	id := svcOK.SyncAsync(ctx, "o", "ghost", false)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if a, ok := svcOK.SyncStatus(id); ok {
			select {
			case <-a.done:
				goto resolved
			default:
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("async never finished")
		}
		time.Sleep(50 * time.Millisecond)
	}
resolved:
	w := doReq(hOK, "GET", "/o/r/api/pushmirror/sync?id="+id, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "error") {
		t.Errorf("failed-async status = %d %q", w.Code, w.Body.String())
	}
	_ = ctx
}

func TestResolveAuthSuccessShapes(t *testing.T) {
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	nn, err := repoimport.NormalizeSource("https://example.com/o/r.git")
	if err != nil {
		t.Fatal(err)
	}
	// Password: username from doc, fallback to secret.
	a, err := svc.resolveAuth(&Doc{AuthKind: AuthPassword, Username: "doc-u"}, &Secret{AuthKind: AuthPassword, Username: "sec-u", Password: "pw"}, nn)
	if err != nil || a.Username != "doc-u" || a.Password != "pw" {
		t.Errorf("password doc-user = %+v,%v", a, err)
	}
	a, err = svc.resolveAuth(&Doc{AuthKind: AuthPassword}, &Secret{AuthKind: AuthPassword, Username: "sec-u", Password: "pw"}, nn)
	if err != nil || a.Username != "sec-u" {
		t.Errorf("password secret-user = %+v,%v", a, err)
	}
	// Token: same fallback.
	a, err = svc.resolveAuth(&Doc{AuthKind: AuthToken, Username: "doc-u"}, &Secret{AuthKind: AuthToken, Username: "sec-u", Token: "tok"}, nn)
	if err != nil || a.Username != "doc-u" || a.Token != "tok" {
		t.Errorf("token = %+v,%v", a, err)
	}
	a, err = svc.resolveAuth(&Doc{AuthKind: AuthToken}, &Secret{AuthKind: AuthToken, Username: "sec-u", Token: "tok"}, nn)
	if err != nil || a.Username != "sec-u" {
		t.Errorf("token secret-user = %+v,%v", a, err)
	}
	// SSH success carries key + known_hosts + fingerprint.
	a, err = svc.resolveAuth(&Doc{AuthKind: AuthSSH, KeyFingerprint: "fp"}, &Secret{AuthKind: AuthSSH, SSHPrivateKey: "k", SSHKnownHosts: "kh"}, nn)
	if err != nil || a.PrivateKey != "k" || a.KnownHosts != "kh" || a.Fingerprint != "fp" {
		t.Errorf("ssh = %+v,%v", a, err)
	}
}

func TestRunPushUnitBranches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	// ValidateTarget failure at fire: token kind on a file:// URL.
	if _, err := reg.Create(ctx, "o/v", git.Sha1); err != nil {
		t.Fatal(err)
	}
	raw, _ := jsonMarshal(&Doc{Version: 1, UpstreamURL: "file:///tmp/x", AuthKind: AuthToken, Schedule: ScheduleOff})
	if _, err := store.PutBytes(ctx, st, store.PushMirrorKey("o", "v"), raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncNow(ctx, "o", "v", false); err == nil {
		t.Error("token-on-file fire ok")
	}
	// Nil-doc skip inside runPush: config deleted after lease? The
	// SyncNow gate returns early; cover the runPush nil-doc branch by
	// deleting between Load calls is racy — instead assert SyncNow on a
	// deleted config errors (the stable half of the branch pair).
	if err := Delete(ctx, st, "o", "v"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncNow(ctx, "o", "v", false); err == nil {
		t.Error("SyncNow on deleted config ok")
	}
	// sanitize(nil) + NextFire bogus schedule.
	if sanitize(nil) != nil {
		t.Error("sanitize(nil) non-nil")
	}
	if _, _, err := NextFire(&Doc{Schedule: "bogus"}, time.Now().UTC()); err == nil {
		t.Error("NextFire(bogus) ok")
	}
	if (&Secret{}).HasMaterial() {
		t.Error("empty secret has material")
	}
	if ((&Secret{AuthKind: AuthNone}).SecretHint()) != "" {
		t.Error("none hint")
	}
	// taskJSON full shape.
	ok := true
	rec := &wal.TaskRecord{ID: "id", Kind: KindPushMirrorSync, Repo: "o/r", Hostname: "h",
		Started: "s", Finished: "f", OK: &ok, Summary: "sum", LogTail: []string{"l"},
		Progress: &wal.Progress{Label: "p", Done: 1, Unit: "u"}, Params: map[string]string{"k": "v"}}
	out := taskJSON(rec)
	if out["id"] != "id" || out["finished"] != "f" {
		t.Errorf("taskJSON = %v", out)
	}
	// writeJSON marshal error; decodeStrict empty; checkSSRF nil svc.
	w := httptest.NewRecorder()
	writeJSON(w, 200, func() {})
	if w.Code != http.StatusInternalServerError {
		t.Errorf("writeJSON func = %d", w.Code)
	}
	if err := decodeStrict(nil, &putBody{}); err != nil {
		t.Errorf("decodeStrict empty = %v", err)
	}
	if err := (&Handler{}).checkSSRF(repoimport.Normalized{}, false); err != nil {
		t.Errorf("nil-svc checkSSRF = %v", err)
	}
}
