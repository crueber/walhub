package social

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// failStore fails configured ops (covers store-error branches).
type failStore struct {
	store.ObjectStore
	getErr       error
	getErrSubstr string
	putErr       error
	put412       bool
	listErr      error
	deleteErr    error
}

func (f *failStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
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

func (f *failStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	if f.listErr != nil {
		return f.listErr
	}
	return f.ObjectStore.List(ctx, prefix, startAfter, fn)
}

func (f *failStore) Delete(ctx context.Context, key string, v store.Version) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return f.ObjectStore.Delete(ctx, key, v)
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

func TestCoverStatusWriters(t *testing.T) {
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
		{ErrCorrupt, 500},
		{errors.New("boom"), 500},
	}
	for _, row := range rows {
		if got := statusFor(row.err); got != row.code {
			t.Fatalf("%v → %d want %d", row.err, got, row.code)
		}
	}
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
			t.Fatalf("kind %d → %d", tc.kind, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	writeJSON(rec, 200, func() {})
	if rec.Code != 500 {
		t.Fatalf("encode: %d", rec.Code)
	}
	rec2 := httptest.NewRecorder()
	writeCached(rec2, httptest.NewRequest("GET", "/", nil), ccSWR, "e", 200, func() {})
	if rec2.Code != 500 {
		t.Fatalf("cached encode: %d", rec2.Code)
	}
	for _, tc := range []struct {
		header, etag string
		want         bool
	}{
		{"", "x", false},
		{"*", "x", true},
		{`"a"`, "a", true},
		{`W/"a", "b"`, "a", true},
		{`"z"`, "a", false},
	} {
		if got := matchETag(tc.header, tc.etag); got != tc.want {
			t.Fatalf("etag %q: %v", tc.header, got)
		}
	}
	if decodeSegment("%zz") != "%zz" {
		t.Fatal("bad escape")
	}
}

func TestCoverSplitStarKey(t *testing.T) {
	prefix := StarredPrefix("jane")
	if o, r, ok := splitStarKey(prefix, prefix+"o/r.json"); !ok || o != "o" || r != "r" {
		t.Fatalf("split: %q %q %v", o, r, ok)
	}
	for _, bad := range []string{
		"other/prefix/o/r.json",
		prefix + "o/r.txt",
		prefix + "noslash.json",
		prefix + "a/b/c.json",
		prefix + "/r.json",
	} {
		if _, _, ok := splitStarKey(prefix, bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
	for _, tc := range []struct {
		after string
		ok    bool
	}{
		{"2026-09-04T12:00:00Z|o/r", true},
		{"bogus", false},
		{"2026-09-04T12:00:00Z|", false},
		{"|o/r", false},
		{"notatime|o/r", false},
	} {
		_, _, ok := splitStarCursor(tc.after)
		if ok != tc.ok {
			t.Fatalf("cursor %q: %v", tc.after, ok)
		}
	}
}

func TestCoverStarStoreErrors(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "o", "r")
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, getErr: errors.New("down")}
	if _, err := x.svc.Star(ctx(), jane(), "o", "r"); err == nil {
		t.Fatal("star read error swallowed")
	}
	if _, err := x.svc.Unstar(ctx(), jane(), "o", "r"); err == nil {
		t.Fatal("unstar read error swallowed")
	}
	if _, err := x.svc.Counts(ctx(), jane(), "o", "r"); err == nil {
		t.Fatal("counts read error swallowed")
	}
	// Record-create race lost (412) → count without increment.
	x2 := newHarness(t)
	seedRepo(t, x2, "o", "r")
	x2.svc.Store = &failStore{ObjectStore: x2.svc.Store, put412: true}
	n, err := x2.svc.Star(ctx(), jane(), "o", "r")
	if err != nil || n != 0 {
		t.Fatalf("412 star: %d %v", n, err)
	}
	// Record-create backend error.
	x3 := newHarness(t)
	seedRepo(t, x3, "o", "r")
	x3.svc.Store = &failStore{ObjectStore: x3.svc.Store, putErr: errors.New("down")}
	if _, err := x3.svc.Star(ctx(), jane(), "o", "r"); err == nil {
		t.Fatal("star put error swallowed")
	}
	// Unstar delete error.
	x4 := newHarness(t)
	seedRepo(t, x4, "o", "r")
	if _, err := x4.svc.Star(ctx(), jane(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	x4.svc.Store = &failStore{ObjectStore: x4.svc.Store, deleteErr: errors.New("down")}
	if _, err := x4.svc.Unstar(ctx(), jane(), "o", "r"); err == nil {
		t.Fatal("unstar delete error swallowed")
	}
	// Corrupt counters break the bump.
	x5 := newHarness(t)
	seedRepo(t, x5, "o", "r")
	seedSocialKey(t, x5, SocialKey("o", "r"), `{oops`)
	if _, err := x5.svc.Star(ctx(), jane(), "o", "r"); !isErr(err, ErrCorrupt) {
		t.Fatalf("bump corrupt: %v", err)
	}
	if err := x5.svc.IncForks(ctx(), "o", "r"); !isErr(err, ErrCorrupt) {
		t.Fatalf("forks corrupt: %v", err)
	}
	// CAS exhaustion → conflict.
	x6 := newHarness(t)
	x6.svc.Store = &failStore{ObjectStore: x6.svc.Store, put412: true}
	if err := x6.svc.IncForks(ctx(), "o", "r"); !isErr(err, ErrConflict) {
		t.Fatalf("cas loop: %v", err)
	}
}

func TestCoverCasUpdateErrors(t *testing.T) {
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
	if _, _, err := s.getJSON(ctx(), "k"); err == nil {
		t.Fatal("getJSON swallowed")
	}
	if err := s.putCreate(ctx(), "k", []byte("x")); err != nil {
		t.Fatal(err)
	}
}
func TestCoverRequireRead(t *testing.T) {
	nilS := New(store.NewMemory(), nil)
	if err := nilS.requireRead(ctx(), "o", "r", auth.Principal{Name: "a", Admin: true}); err != nil {
		t.Fatal(err)
	}
	if err := nilS.requireRead(ctx(), "o", "r", auth.Principal{Name: "w", Write: true}); err != nil {
		t.Fatal(err)
	}
	if err := nilS.requireRead(ctx(), "o", "r", auth.Anonymous()); !isErr(err, ErrUnauthorized) {
		t.Fatalf("nil anon: %v", err)
	}
	if err := nilS.requireRead(ctx(), "o", "r", auth.Principal{Name: "u"}); err != nil {
		t.Fatal(err)
	}
	if nilS.nowUTC().IsZero() {
		t.Fatal("nil clock")
	}
	for _, tc := range []struct {
		kind auth.AuthErrorKind
		want error
	}{
		{auth.ErrForbidden, ErrForbidden},
		{auth.ErrUnavailable, nil},
		{auth.ErrInvalid, ErrUnauthorized},
	} {
		s := New(store.NewMemory(), &stubRoles{checkErr: &auth.AuthError{Kind: tc.kind, Why: "x"}})
		err := s.requireRead(ctx(), "o", "r", auth.Principal{Name: "u"})
		if tc.want == nil {
			if err == nil {
				t.Fatalf("kind %d swallowed", tc.kind)
			}
		} else if !isErr(err, tc.want) {
			t.Fatalf("kind %d: %v", tc.kind, err)
		}
	}
	if err := requireAuthenticated(auth.Anonymous()); !isErr(err, ErrUnauthorized) {
		t.Fatalf("anon: %v", err)
	}
}

func TestCoverStarredEdges(t *testing.T) {
	x := newHarness(t)
	// LIST error.
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, listErr: errors.New("down")}
	if _, _, err := x.svc.Starred(ctx(), "jane", 10, ""); err == nil {
		t.Fatal("LIST error swallowed")
	}
	// Corrupt record skipped; key-derived repo fallback; unreadable skipped.
	x2 := newHarness(t)
	seedRepo(t, x2, "o", "a")
	seedRepo(t, x2, "o", "b")
	seedSocialKey(t, x2, StarredPrefix("jane")+"o/a.json", `{oops`)
	seedSocialKey(t, x2, StarredPrefix("jane")+"o/b.json", `{"starred_at":"2026-09-04T12:00:00Z"}`)
	if err := x2.svc.Store.Delete(ctx(), StarredPrefix("jane")+"o/a.json", ""); err != nil {
		t.Fatal(err)
	}
	// Recreate a: valid record without repo → key-derived fallback.
	seedSocialKey(t, x2, StarredPrefix("jane")+"o/a.json", `{"repo":"","starred_at":"2026-09-03T12:00:00Z"}`)
	entries, _, err := x2.svc.Starred(ctx(), "jane", 10, "")
	if err != nil || len(entries) != 2 || entries[0].Repo != "o/a" || entries[1].Repo != "o/b" {
		t.Fatalf("fallback: %+v %v", entries, err)
	}
	// Unreadable record skipped (selective read error).
	x2.svc.Store = &failStore{ObjectStore: x2.svc.Store, getErr: errors.New("flaky"), getErrSubstr: "o/b.json"}
	entries2, _, err := x2.svc.Starred(ctx(), "jane", 10, "")
	if err != nil || len(entries2) != 1 {
		t.Fatalf("skip: %+v %v", entries2, err)
	}
	// Corrupt record parse error direct.
	if _, err := parseStarRecord([]byte("{oops")); !isErr(err, ErrCorrupt) {
		t.Fatalf("record corrupt: %v", err)
	}
	// Empty-name viewer.
	if s, w := x2.svc.ViewerState(ctx(), auth.Principal{}, "o", "r"); s || w {
		t.Fatal("empty viewer")
	}
}

func TestCoverHandleFalse(t *testing.T) {
	x := newHarness(t)
	for _, path := range []string{
		"/api/v1/me",
		"/api/v1/users/jane",
		"/bad!o/r/api/star",
		"/o/r/api/star/extra",
	} {
		req := httptest.NewRequest("GET", path, nil)
		if x.handler.Handle(httptest.NewRecorder(), req) {
			t.Fatalf("claimed %q", path)
		}
	}
	// Lane auth error.
	x.handler.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Anonymous(), &auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}
	}
	rec := do(t, x, "PUT", "/o/r/api/star", nil, nil)
	if rec.Code != 503 {
		t.Fatalf("lane auth: %d", rec.Code)
	}
	// Service error through the lane.
	x2 := newHarness(t)
	seedRepo(t, x2, "o", "r")
	x2.svc.Store = &failStore{ObjectStore: x2.svc.Store, getErr: errors.New("down")}
	if rec := do(t, x2, "PUT", "/o/r/api/star", nil, asUser("jane")); rec.Code != 500 {
		t.Fatalf("star err: %d", rec.Code)
	}
	if rec := do(t, x2, "GET", "/o/r/api/social", nil, asUser("jane")); rec.Code != 500 {
		t.Fatalf("social err: %d", rec.Code)
	}
	// Blank principal on the users twin → 400.
	if rec := do(t, x2, "GET", "/api/v1/users//starred", nil, nil); rec.Code != 400 {
		t.Fatalf("blank principal: %d", rec.Code)
	}
	// Starred LIST error through the twin.
	x2.svc.Store = &failStore{ObjectStore: x2.svc.Store, listErr: errors.New("down")}
	if rec := do(t, x2, "GET", "/api/v1/me/starred", nil, asUser("jane")); rec.Code != 500 {
		t.Fatalf("twin err: %d", rec.Code)
	}
}

// headFailStore fails HEAD probes only (reads/writes delegate): the
// existence probe must fail OPEN, never mass-hide.
type headFailStore struct {
	store.ObjectStore
	err error
}

func (s *headFailStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	return nil, s.err
}

func TestCoverProbeFailOpen(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "o", "r")
	if _, err := x.svc.Star(ctx(), jane(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	x.svc.Store = &headFailStore{ObjectStore: x.svc.Store, err: errors.New("down")}
	// Manifest HEAD down: starred keeps the entry, viewer keeps flags,
	// restar serves the count (no hiding, no failure).
	entries, _, err := x.svc.Starred(ctx(), "jane", 10, "")
	if err != nil || len(entries) != 1 || entries[0].Repo != "o/r" {
		t.Fatalf("fail-open starred: %+v %v", entries, err)
	}
	if s, w := x.svc.ViewerState(ctx(), jane(), "o", "r"); !s || w {
		t.Fatalf("fail-open viewer: %v %v", s, w)
	}
	if n, err := x.svc.Star(ctx(), jane(), "o", "r"); err != nil || n != 1 {
		t.Fatalf("fail-open restar: %d %v", n, err)
	}
	// Malformed record repos are kept as-is (render what the record says).
	x2 := newHarness(t)
	seedSocialKey(t, x2, StarredPrefix("jane")+"o/x.json", `{"repo":"noslash","starred_at":"2026-09-04T12:00:00Z"}`)
	entries2, _, err := x2.svc.Starred(ctx(), "jane", 10, "")
	if err != nil || len(entries2) != 1 || entries2[0].Repo != "noslash" {
		t.Fatalf("malformed kept: %+v %v", entries2, err)
	}
	// Counter read error on the repair path surfaces (no silent zero).
	x3 := newHarness(t)
	seedRepo(t, x3, "o", "r")
	seedSocialKey(t, x3, StarKey("jane", "o", "r"), `{"repo":"o/r","starred_at":"2026-09-04T12:00:00Z"}`)
	x3.svc.Store = &failStore{ObjectStore: x3.svc.Store, getErr: errors.New("down"), getErrSubstr: "social.json"}
	if _, err := x3.svc.Star(ctx(), jane(), "o", "r"); err == nil {
		t.Fatal("repair read error swallowed")
	}
}
