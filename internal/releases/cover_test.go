package releases

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// failStore fails configured ops (covers store-error branches without a network).
type failStore struct {
	store.ObjectStore
	getErr        error
	getErrSubstr  string // when set, Get fails only for keys containing it
	putErr        error
	put412        bool
	headErr       error
	listErr       error
	listErrPrefix string // when set, List fails only for this exact prefix
	deleteErr     error
	deleteErrSub  string // when set, Delete fails only for keys containing it
	notMod        bool
	notModSubstr  string // when set, NotModified only for keys containing it
	bodyErr       error
	bodyErrSubstr string // when set, Get returns an Object with a failing body for matching keys
}

func (f *failStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if f.bodyErr != nil && (f.bodyErrSubstr == "" || strings.Contains(key, f.bodyErrSubstr)) {
		return store.Object{Meta: store.ObjectMeta{Key: key}, Body: errReadCloser{f.bodyErr}}, nil
	}
	if f.notMod && (f.notModSubstr == "" || strings.Contains(key, f.notModSubstr)) {
		return store.NotModified{Version: "v"}, nil
	}
	if f.getErr != nil && (f.getErrSubstr == "" || strings.Contains(key, f.getErrSubstr)) {
		return nil, f.getErr
	}
	return f.ObjectStore.Get(ctx, key, opts)
}

func (f *failStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if f.put412 {
		return store.ObjectMeta{}, store.NewPrecondition(key, "other")
	}
	if f.putErr != nil {
		return store.ObjectMeta{}, f.putErr
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func (f *failStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	if f.headErr != nil {
		return nil, f.headErr
	}
	return f.ObjectStore.Head(ctx, key)
}

func (f *failStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	if f.listErr != nil && (f.listErrPrefix == "" || prefix == f.listErrPrefix) {
		return f.listErr
	}
	return f.ObjectStore.List(ctx, prefix, startAfter, fn)
}

func (f *failStore) Delete(ctx context.Context, key string, v store.Version) error {
	if f.deleteErr != nil && (f.deleteErrSub == "" || strings.Contains(key, f.deleteErrSub)) {
		return f.deleteErr
	}
	return f.ObjectStore.Delete(ctx, key, v)
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

type errReadCloser struct{ err error }

func (e errReadCloser) Read([]byte) (int, error) { return 0, e.err }
func (e errReadCloser) Close() error             { return nil }

func TestStatusForTable(t *testing.T) {
	rows := []struct {
		err  error
		code int
	}{
		{nil, 200},
		{ErrNotFound, 404},
		{ErrInvalid, 400},
		{ErrUnauthorized, 401},
		{ErrForbidden, 403},
		{ErrConflict, 409},
		{ErrTooLarge, 413},
		{ErrUnavailable, 503},
		{ErrCorrupt, 500},
		{fmt.Errorf("wrapped: %w", ErrNotFound), 404},
		{errors.New("boom"), 500},
	}
	for _, row := range rows {
		if got := statusFor(row.err); got != row.code {
			t.Fatalf("%v → %d want %d", row.err, got, row.code)
		}
	}
}

func TestWriteErrAuthKinds(t *testing.T) {
	for _, tc := range []struct {
		kind auth.AuthErrorKind
		code int
	}{
		{auth.ErrForbidden, 403},
		{auth.ErrUnavailable, 503},
		{auth.ErrInvalid, 401},
	} {
		rec := httptest.NewRecorder()
		writeErr(rec, &auth.AuthError{Kind: tc.kind, Why: "nope"})
		if rec.Code != tc.code {
			t.Fatalf("kind %d → %d want %d", tc.kind, rec.Code, tc.code)
		}
	}
	rec := httptest.NewRecorder()
	writeJSON(rec, 200, func() {})
	if rec.Code != 500 {
		t.Fatalf("unencodable: %d", rec.Code)
	}
}

func TestDecodeHelpersTable(t *testing.T) {
	if decodeSegment("%zz") != "%zz" {
		t.Fatal("bad escape should survive verbatim")
	}
	if decodeTag("%zz") != "%zz" {
		t.Fatal("bad tag escape should survive verbatim")
	}
	// matchETag variants.
	for _, tc := range []struct {
		header, etag string
		want         bool
	}{
		{"", "x", false},
		{"*", "x", true},
		{`"abc"`, "abc", true},
		{`W/"abc", "def"`, "abc", true},
		{`"zzz"`, "abc", false},
	} {
		if got := matchETag(tc.header, tc.etag); got != tc.want {
			t.Fatalf("matchETag(%q): %v", tc.header, got)
		}
	}
	// matchToken variants.
	if matchToken("") != "" || matchToken("*") != "*" {
		t.Fatal("empty/star tokens")
	}
	if matchToken(`W/"tok", "other"`) != "tok" {
		t.Fatal("weak multi token")
	}
	// decodeStrict errors.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/", errReader{errors.New("boom")})
	var v map[string]any
	if decodeStrict(rec, req, 100, map[string]bool{}, &v) {
		t.Fatal("unreadable body accepted")
	}
	badBodies := []string{`{oops`, `[1]`, `{"a":1}`}
	for _, b := range badBodies {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/", strings.NewReader(b))
		allowed := map[string]bool{"a": true}
		if b == `{"a":1}` {
			var typed struct {
				A string `json:"a"`
			}
			if decodeStrict(rec, req, 100, allowed, &typed) {
				t.Fatalf("type mismatch accepted: %s", b)
			}
			continue
		}
		if decodeStrict(rec, req, 100, allowed, &v) {
			t.Fatalf("accepted: %s", b)
		}
	}
}

func TestRoleHelpersCover(t *testing.T) {
	for _, tc := range []struct {
		role string
		rank int
	}{
		{"read", 1}, {"triage", 2}, {"write", 3}, {"maintain", 4}, {"admin", 5}, {"READ", 1}, {"bogus", 0},
	} {
		if roleRank(tc.role) != tc.rank {
			t.Fatalf("rank %q", tc.role)
		}
	}
	roles := newFakeRoles()
	s := New(store.NewMemory(), roles)
	if got := s.roleOf(ctx(), "o", "r", auth.Principal{Name: "x"}); got != "" {
		t.Fatalf("ungranted: %q", got)
	}
	roles.grant("o", "r", "x", "write")
	if got := s.roleOf(ctx(), "o", "r", auth.Principal{Name: "x"}); got != "write" {
		t.Fatalf("granted: %q", got)
	}
	// Nil-roles fallbacks.
	nilS := New(store.NewMemory(), nil)
	if nilS.roleOf(ctx(), "o", "r", auth.Principal{Name: "a", Admin: true}) != "admin" {
		t.Fatal("nil admin")
	}
	if nilS.roleOf(ctx(), "o", "r", auth.Principal{Name: "w", Write: true}) != "write" {
		t.Fatal("nil write")
	}
	if nilS.roleOf(ctx(), "o", "r", auth.Anonymous()) != "" {
		t.Fatal("nil anon")
	}
	if nilS.roleOf(ctx(), "o", "r", auth.Principal{Name: "u"}) != "read" {
		t.Fatal("nil user")
	}
	if nilS.nowUTC().IsZero() {
		t.Fatal("nil clock")
	}
	// requireRead branches.
	if err := nilS.requireRead(ctx(), "o", "r", auth.Principal{Name: "a", Admin: true}); err != nil {
		t.Fatal(err)
	}
	if err := nilS.requireRead(ctx(), "o", "r", auth.Principal{Name: "w", Write: true}); err != nil {
		t.Fatal(err)
	}
	if err := nilS.requireRead(ctx(), "o", "r", auth.Anonymous()); !isErr(err, ErrUnauthorized) {
		t.Fatalf("nil anon read: %v", err)
	}
	if err := nilS.requireRead(ctx(), "o", "r", auth.Principal{Name: "u"}); err != nil {
		t.Fatal(err)
	}
	deny := &stubRoles{checkErr: &auth.AuthError{Kind: auth.ErrForbidden, Why: "private"}}
	if err := New(store.NewMemory(), deny).requireRead(ctx(), "o", "r", auth.Principal{Name: "u"}); !isErr(err, ErrForbidden) {
		t.Fatalf("forbidden: %v", err)
	}
	deny.checkErr = &auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}
	if err := New(store.NewMemory(), deny).requireRead(ctx(), "o", "r", auth.Principal{Name: "u"}); err == nil {
		t.Fatal("unavailable swallowed")
	}
	deny.checkErr = &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad"}
	if err := New(store.NewMemory(), deny).requireRead(ctx(), "o", "r", auth.Principal{Name: "u"}); !isErr(err, ErrUnauthorized) {
		t.Fatalf("invalid: %v", err)
	}
}

type stubRoles struct {
	checkErr *auth.AuthError
}

func (s *stubRoles) Resolve(_ context.Context, _, _ string, _ auth.Principal) (identity.Role, *identity.AccessDoc) {
	return "", nil
}

func (s *stubRoles) CheckRead(_ context.Context, _, _ string, _ auth.Principal) *auth.AuthError {
	return s.checkErr
}

func TestCasUpdateStoreErrors(t *testing.T) {
	mem := store.NewMemory()
	s := New(&failStore{ObjectStore: mem, getErr: errors.New("boom")}, nil)
	if _, err := s.casUpdate(ctx(), "k", 0, func([]byte, store.Version) ([]byte, bool, error) {
		return []byte("x"), true, nil
	}); err == nil {
		t.Fatal("get error swallowed")
	}
	s2 := New(&failStore{ObjectStore: mem, putErr: errors.New("boom")}, nil)
	if _, err := s2.casUpdate(ctx(), "k", 1, func([]byte, store.Version) ([]byte, bool, error) {
		return []byte("x"), true, nil
	}); err == nil {
		t.Fatal("put error swallowed")
	}
	// 412 exhaustion → conflict.
	s3 := New(&failStore{ObjectStore: mem, put412: true}, nil)
	if _, err := s3.casUpdate(ctx(), "k", 2, func([]byte, store.Version) ([]byte, bool, error) {
		return []byte("x"), true, nil
	}); !isErr(err, ErrConflict) {
		t.Fatalf("412 loop: %v", err)
	}
	// getJSON error passthrough.
	if _, _, err := s.getJSON(ctx(), "k"); err == nil {
		t.Fatal("getJSON swallowed")
	}
}

func TestCorruptHeaders(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	bad := []byte("{oops")
	mustStore(t, x, ReleaseKey("o", "r", "bad"), bad)
	if _, _, err := x.svc.GetRelease(ctx(), "o", "r", writer(), "bad"); !isErr(err, ErrCorrupt) {
		t.Fatalf("corrupt get: %v", err)
	}
	mustStore(t, x, LatestKey("o", "r"), bad)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	// Corrupt pointer falls back to the scan.
	got, _, err := x.svc.LatestRelease(ctx(), "o", "r", writer(), false)
	if err != nil || got.Tag != "v1" {
		t.Fatalf("corrupt pointer: %+v %v", got, err)
	}
	// Corrupt release header breaks upload upfront (no orphan bytes).
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "bad", "f",
		strings.NewReader("x"), 1, shaOf([]byte("x")), ""); !isErr(err, ErrCorrupt) {
		t.Fatalf("corrupt upload: %v", err)
	}
	raw, _, _ := x.svc.getJSON(ctx(), AssetKey("o", "r", "bad", "f"))
	if raw != nil {
		t.Fatal("orphan bytes after corrupt-header upload")
	}
}

func mustStore(t *testing.T, x *harness, key string, raw []byte) {
	t.Helper()
	if _, err := store.PutBytes(ctx(), x.svc.Store, key, raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
}

func TestUploadEdgeBranches(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	body := []byte("data")
	sha := shaOf(body)
	// Empty tag / name / sha / content-type at the service layer.
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "", "f", strings.NewReader("x"), 1, sha, ""); !isErr(err, ErrInvalid) {
		t.Fatalf("empty tag: %v", err)
	}
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "a/b", strings.NewReader("x"), 1, sha, ""); !isErr(err, ErrInvalid) {
		t.Fatalf("slash name: %v", err)
	}
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "f", strings.NewReader("x"), 1, "zz", ""); !isErr(err, ErrInvalid) {
		t.Fatalf("bad sha: %v", err)
	}
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "f", strings.NewReader("x"), 1, sha, strings.Repeat("c", 201)); !isErr(err, ErrInvalid) {
		t.Fatalf("bad ct: %v", err)
	}
	// Spool dir unusable.
	x.svc.SpoolDir = "/proc/walhub-nope/spool"
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "f", strings.NewReader("x"), 1, shaOf([]byte("x")), ""); err == nil {
		t.Fatal("bad spool dir accepted")
	}
	x.svc.SpoolDir = x.spool
	// Body read error.
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "f", errReader{errors.New("boom")}, 1, sha, ""); err == nil {
		t.Fatal("body error swallowed")
	}
	// Backend put error (non-412).
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, putErr: errors.New("down")}
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "f",
		strings.NewReader("x"), 1, shaOf([]byte("x")), ""); err == nil {
		t.Fatal("put error swallowed")
	}
}

func TestOrphanAdoptionAndUnsettled(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	body := []byte("orphan-bytes")
	// Crash simulation: bytes Created, header never appended.
	if _, err := store.PutBytes(ctx(), x.svc.Store, AssetKey("o", "r", "v1", "orph"),
		body, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	got, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "orph",
		strings.NewReader(string(body)), int64(len(body)), shaOf(body), "")
	if err != nil || got.SHA256 != shaOf(body) {
		t.Fatalf("adopt: %+v %v", got, err)
	}
	// Clash resolver direct: header gone → 404.
	if _, _, err := x.svc.resolveAssetClash(ctx(), "o", "r", "gone", &AssetEntry{Name: "f"}); !isErr(err, ErrNotFound) {
		t.Fatalf("clash gone: %v", err)
	}
	// Corrupt header → ErrCorrupt.
	mustStore(t, x, ReleaseKey("o", "r", "bad"), []byte("{oops"))
	if _, _, err := x.svc.resolveAssetClash(ctx(), "o", "r", "bad", &AssetEntry{Name: "f"}); !isErr(err, ErrCorrupt) {
		t.Fatalf("clash corrupt: %v", err)
	}
	// Perpetually 412 + failing verify → 503, never a blind append.
	base := x.svc.Store
	x.svc.Store = &failStore{ObjectStore: base, put412: true, getErr: errors.New("flaky"), getErrSubstr: "/assets/"}
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "stuck",
		strings.NewReader("x"), 1, shaOf([]byte("x")), ""); !isErr(err, ErrUnavailable) {
		t.Fatalf("unsettled: %v", err)
	}
	x.svc.Store = base
	rel, _, _ := x.svc.GetRelease(ctx(), "o", "r", writer(), "v1")
	if findAsset(rel, "stuck") >= 0 {
		t.Fatal("blind append after unsettled store")
	}
}

func TestAppendDeleteAssetEdges(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	// Append with no header → 404.
	if _, err := x.svc.appendAssetEntry(ctx(), "o", "r", "gone", &AssetEntry{Name: "f"}); !isErr(err, ErrNotFound) {
		t.Fatalf("append gone: %v", err)
	}
	mustStore(t, x, ReleaseKey("o", "r", "bad"), []byte("{oops"))
	if _, err := x.svc.appendAssetEntry(ctx(), "o", "r", "bad", &AssetEntry{Name: "f"}); !isErr(err, ErrCorrupt) {
		t.Fatalf("append corrupt: %v", err)
	}
	x.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	body := []byte("abc")
	upload(t, x, "v1", "f", body)
	// Same-sha direct append converges; diff-sha 409s.
	same := &AssetEntry{Name: "f", SHA256: shaOf(body)}
	if _, err := x.svc.appendAssetEntry(ctx(), "o", "r", "v1", same); err != nil {
		t.Fatalf("converge: %v", err)
	}
	diff := &AssetEntry{Name: "f", SHA256: shaOf([]byte("other-body!!"))}
	if _, err := x.svc.appendAssetEntry(ctx(), "o", "r", "v1", diff); !isErr(err, ErrConflict) {
		t.Fatalf("clash append: %v", err)
	}
	// Delete with corrupt header → ErrCorrupt.
	if _, err := x.svc.DeleteAsset(ctx(), "o", "r", writer(), "bad", "f"); !isErr(err, ErrCorrupt) {
		t.Fatalf("delete corrupt: %v", err)
	}
}

func TestDeleteReleaseStoreErrors(t *testing.T) {
	x := newHarness(t)
	grantMaintain(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	base := x.svc.Store
	x.svc.Store = &failStore{ObjectStore: base, listErr: errors.New("down")}
	if err := x.svc.DeleteRelease(ctx(), "o", "r", writer(), "v1"); err == nil {
		t.Fatal("list error swallowed")
	}
	x.svc.Store = &failStore{ObjectStore: base, deleteErr: errors.New("down")}
	if err := x.svc.DeleteRelease(ctx(), "o", "r", writer(), "v1"); err == nil {
		t.Fatal("delete error swallowed")
	}
}

func TestAssetBytesMatchNotModified(t *testing.T) {
	x := newHarness(t)
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, notMod: true}
	if _, err := x.svc.assetBytesMatch(ctx(), "k", shaOf([]byte("x"))); !isErr(err, ErrConflict) {
		t.Fatalf("not-modified: %v", err)
	}
}

func TestServeAssetStoreErrors(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	upload(t, x, "v1", "f", []byte("data"))
	id := git.RepoId{Owner: "o", Name: "r"}
	// Head failure → 503.
	base := x.svc.Store
	x.svc.Store = &failStore{ObjectStore: base, headErr: errors.New("down")}
	rec := httptest.NewRecorder()
	x.handler.HandleRepo(rec, authedGet("/o/r/releases/v1/assets/f"), id, []string{"releases", "v1", "assets", "f"})
	if rec.Code != 503 {
		t.Fatalf("head down: %d", rec.Code)
	}
	x.svc.Store = base
	// Entry present but bytes gone → 404.
	if err := base.Delete(ctx(), AssetKey("o", "r", "v1", "f"), ""); err != nil {
		t.Fatal(err)
	}
	rec2 := httptest.NewRecorder()
	x.handler.HandleRepo(rec2, authedGet("/o/r/releases/v1/assets/f"), id, []string{"releases", "v1", "assets", "f"})
	if rec2.Code != 404 {
		t.Fatalf("bytes gone: %d", rec2.Code)
	}
	// Corrupt header → 500.
	mustStore(t, x, ReleaseKey("o", "r", "bad"), []byte("{oops"))
	rec3 := httptest.NewRecorder()
	x.handler.HandleRepo(rec3, authedGet("/o/r/releases/bad/assets/f"), id, []string{"releases", "bad", "assets", "f"})
	if rec3.Code != 500 {
		t.Fatalf("corrupt header: %d", rec3.Code)
	}
	// Invalid tag / name shapes.
	rec4 := httptest.NewRecorder()
	x.handler.HandleRepo(rec4, authedGet("/x"), id, []string{"releases", strings.Repeat("t", 600), "assets", "f"})
	if rec4.Code != 400 {
		t.Fatalf("long tag: %d", rec4.Code)
	}
	rec5 := httptest.NewRecorder()
	x.handler.HandleRepo(rec5, authedGet("/x"), id, []string{"releases", "v1", "assets", ".dot"})
	if rec5.Code != 400 {
		t.Fatalf("dot name: %d", rec5.Code)
	}
	// Authenticator error → mapped status.
	x.handler.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Anonymous(), &auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}
	}
	rec6 := httptest.NewRecorder()
	x.handler.HandleRepo(rec6, authedGet("/x"), id, []string{"releases", "v1", "assets", "f"})
	if rec6.Code != 503 {
		t.Fatalf("auth down: %d", rec6.Code)
	}
}

func authedGet(target string) *http.Request {
	req := httptest.NewRequest("GET", target, nil)
	req.Header.Set("X-Test-Principal", "bob")
	return req
}
