// cover_test.go — branch coverage for error/edge paths: failing-store
// decorators, corrupt objects, auth-kind matrix, key shapes, task seams.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// --- failing-store decorators ---------------------------------------------------

type errStore struct {
	store.ObjectStore
	err error
}

func (s errStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	return nil, s.err
}
func (s errStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	return nil, s.err
}
func (s errStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	return store.ObjectMeta{}, s.err
}
func (s errStore) Delete(ctx context.Context, key string, v store.Version) error { return s.err }
func (s errStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	return s.err
}
func (s errStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	return s.err
}

// always412Store CAS-loops forever: Gets delegate, Update PUTs 412.
type always412Store struct {
	store.ObjectStore
}

func (s always412Store) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if opts.Mode == store.PutUpdate {
		return store.ObjectMeta{}, &store.StoreError{Kind: store.ErrKindPreconditionFailed, Key: key}
	}
	return s.ObjectStore.Put(ctx, key, body, opts)
}

// failCreateStore fails Create PUTs only (reads + CAS updates delegate).
type failCreateStore struct {
	store.ObjectStore
}

func (s failCreateStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if opts.Mode == store.PutCreate {
		return store.ObjectMeta{}, errors.New("create boom")
	}
	return s.ObjectStore.Put(ctx, key, body, opts)
}

// failListStore fails LIST only.
type failListStore struct {
	store.ObjectStore
}

func (s failListStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	return errors.New("list boom")
}

// failGetStore fails GET only (LIST/deletes delegate).
type failGetStore struct {
	store.ObjectStore
}

func (s failGetStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	return nil, errors.New("get boom")
}

// failDeleteStore fails Delete only.
type failDeleteStore struct {
	store.ObjectStore
}

func (s failDeleteStore) Delete(ctx context.Context, key string, v store.Version) error {
	return errors.New("delete boom")
}

func writeRaw(t *testing.T, st store.ObjectStore, key string, raw []byte) {
	t.Helper()
	if _, err := store.PutBytes(ctx(), st, key, raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
}

// --- status + roles ---------------------------------------------------------------

func TestStatusForTable(t *testing.T) {
	if statusFor(nil) != 200 {
		t.Fatal("nil → 200")
	}
	cases := map[error]int{
		ErrNotFound: 404, ErrUnauthorized: 401, ErrForbidden: 403,
		ErrInvalid: 400, ErrConflict: 409, errors.New("boom"): 500,
		fmt.Errorf("wrap: %w", ErrNotFound): 404,
	}
	for err, want := range cases {
		if got := statusFor(err); got != want {
			t.Errorf("statusFor(%v) = %d, want %d", err, got, want)
		}
	}
}

func TestRoleRankTable(t *testing.T) {
	cases := map[string]int{
		"read": 1, "triage": 2, "write": 3, "maintain": 4, "admin": 5, "bogus": 0, "": 0,
	}
	for r, want := range cases {
		if got := roleRank(r); got != want {
			t.Errorf("roleRank(%q) = %d", r, got)
		}
	}
}

type denyRoles struct{ kind auth.AuthErrorKind }

func (d denyRoles) Resolve(context.Context, string, string, auth.Principal) (identity.Role, *identity.AccessDoc) {
	return identity.RoleRead, nil
}
func (d denyRoles) CheckRead(context.Context, string, string, auth.Principal) *auth.AuthError {
	return &auth.AuthError{Kind: d.kind, Why: "denied"}
}

func TestRequireRoleMatrix(t *testing.T) {
	x := newHarness(t)
	admin := auth.Principal{Name: "root@example.com", Admin: true}
	if err := x.svc.requireRole(ctx(), "acme", "repo", admin, "admin"); err != nil {
		t.Fatalf("host admin must pass: %v", err)
	}
	x.roles.grant("acme", "repo", "amy@example.com", "admin")
	amy := auth.Principal{Name: "amy@example.com"}
	if err := x.svc.requireRole(ctx(), "acme", "repo", amy, "admin"); err != nil {
		t.Fatalf("granted admin must pass: %v", err)
	}
	if err := x.svc.requireRole(ctx(), "acme", "repo", auth.Anonymous(), "admin"); err == nil {
		t.Fatal("anon must fail")
	} else if statusFor(err) != 401 {
		t.Fatalf("anon = %v", err)
	}
	bob := auth.Principal{Name: "bob@example.com"}
	if err := x.svc.requireRole(ctx(), "acme", "repo", bob, "admin"); err == nil {
		t.Fatal("unprivileged must fail")
	} else if statusFor(err) != 403 {
		t.Fatalf("unprivileged = %v", err)
	}
}

func TestRequireReadMatrix(t *testing.T) {
	x := newHarness(t)
	admin := auth.Principal{Name: "root@example.com", Admin: true}
	writer := auth.Principal{Name: "w@example.com", Write: true}
	if err := x.svc.requireRead(ctx(), "acme", "repo", admin); err != nil {
		t.Fatal(err)
	}
	if err := x.svc.requireRead(ctx(), "acme", "repo", writer); err != nil {
		t.Fatal(err)
	}
	// Nil roles: legacy flag behavior.
	bare := New(store.NewMemory(), nil)
	if err := bare.requireRead(ctx(), "acme", "repo", auth.Anonymous()); statusFor(err) != 401 {
		t.Fatalf("bare anon = %v", err)
	}
	if err := bare.requireRead(ctx(), "acme", "repo", auth.Principal{Name: "u"}); err != nil {
		t.Fatalf("bare authed = %v", err)
	}
	if got := bare.roleOf(ctx(), "a", "r", auth.Principal{Name: "u"}); got != "" {
		t.Fatalf("bare role = %q", got)
	}
	// Deny gate kinds.
	for kind, want := range map[auth.AuthErrorKind]int{
		auth.ErrForbidden: 403, auth.ErrUnavailable: 500, auth.ErrInvalid: 401,
	} {
		svc := New(store.NewMemory(), denyRoles{kind: kind})
		err := svc.requireRead(ctx(), "acme", "repo", auth.Principal{Name: "u"})
		if statusFor(err) != want {
			t.Fatalf("kind %v = %v, want %d", kind, err, want)
		}
	}
	// Watch PUT behind a deny gate → 403, not a watch write.
	svc := New(store.NewMemory(), denyRoles{kind: auth.ErrForbidden})
	h := &Handler{Svc: svc}
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "u@example.com"}, nil
	}
	r := httptest.NewRequest("PUT", "/acme/repo/api/watch", nil)
	r.Header.Set("X-Test-Principal", "u@example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Fatalf("gated watch = %d", rec.Code)
	}
}

func TestNowUTCDefault(t *testing.T) {
	svc := &Service{}
	if svc.nowUTC().IsZero() {
		t.Fatal("nil clock must fall back to time.Now")
	}
	if got := svc.retentionDays(); got != DefaultRetentionDays {
		t.Fatalf("default retention = %d", got)
	}
	svc.RetentionDays = 7
	if got := svc.retentionDays(); got != 7 {
		t.Fatalf("override retention = %d", got)
	}
}

// --- key shapes -----------------------------------------------------------------------

func TestKeyShapes(t *testing.T) {
	if got := NotifKey("a@e.com", "id"); got != "users/a@e.com/notifications/id.json" {
		t.Fatal(got)
	}
	if got := NotifPrefix("a@e.com"); got != "users/a@e.com/notifications/" {
		t.Fatal(got)
	}
	if got := NotifIndexKey("a@e.com"); got != "users/a@e.com/notifications/index.json" {
		t.Fatal(got)
	}
	if got := WatchingKey("a@e.com", "o", "r"); got != "users/a@e.com/watching/o/r.json" {
		t.Fatal(got)
	}
	if got := SocialKey("o", "r"); got != "repos/o/r/meta/social.json" {
		t.Fatal(got)
	}
	if got := CollabStateKey("o", "r"); got != "repos/o/r/meta/collab_state.json" {
		t.Fatal(got)
	}
	if got := ActivityKey("o", "r", 1); got != "repos/o/r/collab-events/000000000001.json" {
		t.Fatal(got)
	}
	if got := ActivityPrefix("o", "r"); got != "repos/o/r/collab-events/" {
		t.Fatal(got)
	}
	if got := HookKey("o", "r", "h"); got != "repos/o/r/webhooks/h.json" {
		t.Fatal(got)
	}
	if got := WebhooksPrefix("o", "r"); got != "repos/o/r/webhooks/" {
		t.Fatal(got)
	}
	if got := CursorKey("o", "r", "h"); got != "repos/o/r/webhooks/cursors/h.json" {
		t.Fatal(got)
	}
	if got := CursorsPrefix("o", "r"); got != "repos/o/r/webhooks/cursors/" {
		t.Fatal(got)
	}
	if got := DeliveriesKey("o", "r", "h"); got != "repos/o/r/webhooks/h/deliveries/recent.json" {
		t.Fatal(got)
	}
	if got := threadKey("o", "r", 7); got != "repos/o/r/issues/000007/thread.json" {
		t.Fatal(got)
	}
}

// --- emit edges ----------------------------------------------------------------------------

func TestEmitBadRepo(t *testing.T) {
	x := newHarness(t)
	x.svc.EmitIssue(ctx(), "noslash", 1, "subscribed", "a", "", "commented", []string{"b"})
	if x.svc.readActivity(ctx(), "noslash", "", 1) != nil {
		t.Fatal("bad repo must not emit")
	}
	if _, _, err := store.GetBytes(ctx(), x.svc.Store, CollabStateKey("noslash", ""), store.GetOptions{}); !store.IsNotFound(err) {
		t.Fatal("bad repo must not reserve seq")
	}
}

func TestReserveSeqEdges(t *testing.T) {
	x := newHarness(t)
	writeRaw(t, x.svc.Store, CollabStateKey("acme", "repo"), []byte("{corrupt"))
	if _, err := x.svc.reserveSeq(ctx(), "acme", "repo"); err == nil {
		t.Fatal("corrupt collab_state must fail")
	}
	// Negative allocator clamps to 1.
	st2 := store.NewMemory()
	x2 := newHarness(t)
	x2.svc.Store = st2
	writeRaw(t, st2, CollabStateKey("acme", "repo"), mustEncode(t, CollabState{NextSeq: -5}))
	seq, err := x2.svc.reserveSeq(ctx(), "acme", "repo")
	if err != nil || seq != 1 {
		t.Fatalf("clamp = %d, %v", seq, err)
	}
	// Permanent CAS contention exhausts the loop.
	st3 := store.NewMemory()
	writeRaw(t, st3, CollabStateKey("acme", "repo"), mustEncode(t, CollabState{}))
	svc3 := New(always412Store{st3}, nil)
	if _, err := svc3.reserveSeq(ctx(), "acme", "repo"); err == nil {
		t.Fatal("CAS exhaustion must fail")
	}
}

func TestAppendActivityEdges(t *testing.T) {
	x := newHarness(t)
	e := Emission{Repo: "acme/repo", Kind: "issue", Class: "subscribed"}
	if err := x.svc.appendActivity(ctx(), "acme", "repo", 1, e, "commented", "T", "a", x.now.Format(dateTimeFmt), nil, false); err != nil {
		t.Fatal(err)
	}
	// Same seq twice → 412-tolerant success.
	if err := x.svc.appendActivity(ctx(), "acme", "repo", 1, e, "commented", "T", "a", x.now.Format(dateTimeFmt), nil, false); err != nil {
		t.Fatalf("replay append = %v", err)
	}
	svc2 := New(errStore{store.NewMemory(), errors.New("store down")}, nil)
	if err := svc2.appendActivity(ctx(), "acme", "repo", 1, e, "commented", "T", "a", "", nil, false); err == nil {
		t.Fatal("store error must surface")
	}
	// Corrupt activity reads as absent.
	writeRaw(t, x.svc.Store, ActivityKey("acme", "repo2", 9), []byte("{bad"))
	if x.svc.readActivity(ctx(), "acme", "repo2", 9) != nil {
		t.Fatal("corrupt activity must read nil")
	}
}

func TestResolveEdges(t *testing.T) {
	x := newHarness(t)
	// Nil Teams: team spellings drop silently.
	bare := New(x.svc.Store, nil)
	bare.Now = x.svc.Now
	bare.EmitIssue(ctx(), "acme/repo", 0, "mentioned", "bob@example.com", "", "", []string{"acme/backend"})
	// Nil prober: mentioned users drop (nothing valid to say).
	bare.EmitIssue(ctx(), "acme/repo", 0, "mentioned", "bob@example.com", "", "", []string{"amy@example.com"})
	if n := countNotifs(t, x, "amy@example.com"); n != 0 {
		t.Fatalf("nil prober must drop: %d", n)
	}
	// Probe error keeps the recipient (fail open; Create is idempotent).
	x.profiles.err = errors.New("store down")
	defer func() { x.profiles.err = nil }()
	x.addProfile("amy@example.com")
	x.svc.EmitIssue(ctx(), "acme/repo", 0, "mentioned", "bob@example.com", "", "", []string{"amy@example.com"})
	if n := countNotifs(t, x, "amy@example.com"); n != 1 {
		t.Fatalf("probe error must keep: %d", n)
	}
}

func TestWatchersCorruptSocial(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com")
	writeRaw(t, x.svc.Store, SocialKey("acme", "repo"), []byte("{bad"))
	x.writeThread(t, "acme", "repo", 7, "T", "amy@example.com")
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", []string{})
	if ev := x.svc.readActivity(ctx(), "acme", "repo", 1); ev == nil {
		t.Fatal("corrupt social must not stop fan-out")
	}
}

func TestReadThreadHeadEdges(t *testing.T) {
	x := newHarness(t)
	if x.svc.readThreadHead(ctx(), "acme", "repo", 1) != nil {
		t.Fatal("absent thread must read nil")
	}
	writeRaw(t, x.svc.Store, threadKey("acme", "repo", 2), []byte("{bad"))
	if x.svc.readThreadHead(ctx(), "acme", "repo", 2) != nil {
		t.Fatal("corrupt thread must read nil")
	}
}

func TestHasUnreadCorrupt(t *testing.T) {
	x := newHarness(t)
	writeRaw(t, x.svc.Store, NotifIndexKey("amy@example.com"), []byte("{bad"))
	if x.svc.hasUnread(ctx(), "amy@example.com", "acme/repo", 1, ReasonSubscribed) {
		t.Fatal("corrupt index must not dedup")
	}
}

func TestCreateAllCanceled(t *testing.T) {
	x := newHarness(t)
	cctx, cancel := context.WithCancel(ctx())
	cancel()
	done, failed := x.svc.createAll(cctx, "acme", "repo", Emission{}, "", "", "", 1,
		[]target{{principal: "a@example.com", reason: ReasonSubscribed}})
	if !failed || len(done) != 0 {
		t.Fatalf("canceled = %v/%d", failed, len(done))
	}
}

func TestCreateOneStoreError(t *testing.T) {
	st := store.NewMemory()
	writeRaw(t, st, CollabStateKey("acme", "repo"), mustEncode(t, CollabState{NextSeq: 1}))
	svc := New(failCreateStore{st}, nil)
	svc.Now = time.Now
	svc.EmitIssue(ctx(), "acme/repo", 0, "subscribed", "bob@example.com", "", "commented", []string{"amy@example.com"})
	// Shortfall ran, but the activity never landed (Create failed), so no
	// fanout is armed for the nonexistent event (issue #92: arming it
	// would drain a gap and lose the recipients with zero trace).
	if rec := svc.TaskStatus("acme/repo", TaskKindFanout); rec != nil {
		t.Fatalf("phantom fanout armed for nonexistent event: %+v", rec)
	}
}

func TestIndexAddCorrupt(t *testing.T) {
	x := newHarness(t)
	writeRaw(t, x.svc.Store, NotifIndexKey("amy@example.com"), []byte("{bad"))
	err := x.svc.indexAdd(ctx(), "amy@example.com", IndexEntry{ID: strings.Repeat("d", 32)})
	if err == nil {
		t.Fatal("corrupt index must fail the target")
	}
}

func TestCasUpdateEdges(t *testing.T) {
	x := newHarness(t)
	// attempts<=0 takes the default loop.
	if _, err := x.svc.casUpdate(ctx(), "acme/zero.json", 0, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		return []byte("{}"), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Non-412 store errors surface.
	svc2 := New(errStore{store.NewMemory(), errors.New("down")}, nil)
	if _, err := svc2.casUpdate(ctx(), "k", 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		return []byte("{}"), true, nil
	}); err == nil {
		t.Fatal("store error must surface")
	}
	if _, _, err := svc2.getJSON(ctx(), "k"); err == nil {
		t.Fatal("getJSON error must surface")
	}
}

func TestEmitPullAndRelease(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com", "carol@example.com")
	x.writeThread(t, "acme", "repo", 3, "PR title", "amy@example.com")
	x.svc.EmitPull(ctx(), "acme/repo", 3, "opened", "bob@example.com", "", []string{"carol@example.com"})
	raw, _, _ := store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("carol@example.com"), store.GetOptions{})
	var ix IndexDoc
	_ = json.Unmarshal(raw, &ix)
	if len(ix.Entries) != 1 || ix.Entries[0].Kind != "pull" || ix.Entries[0].Reason != ReasonSubscribed {
		t.Fatalf("pull opened = %+v", ix.Entries)
	}
	ev := x.svc.readActivity(ctx(), "acme", "repo", 1)
	if ev.Action != ActionOpened || ev.Kind != "pull" {
		t.Fatalf("pull activity = %+v", ev)
	}
	// Fork (num 0): repo-kind activity, no thread read, no recipients.
	x.svc.EmitPull(ctx(), "acme/repo", 0, "forked", "bob@example.com", "", []string{})
	if ev := x.svc.readActivity(ctx(), "acme", "repo", 2); ev == nil || ev.Kind != "repo" {
		t.Fatalf("fork activity = %+v", ev)
	}
	// Release publish (07's future call site).
	x.svc.EmitRelease(ctx(), "acme/repo", "v1.0", "bob@example.com", "")
	if ev := x.svc.readActivity(ctx(), "acme", "repo", 3); ev == nil || ev.Action != ActionReleasePublished {
		t.Fatalf("release activity = %+v", ev)
	}
}

func TestTeamCap(t *testing.T) {
	x := newHarness(t)
	x.addProfile("bob@example.com")
	members := []string{}
	for i := 0; i < MaxTeamFanout+50; i++ {
		p := fmt.Sprintf("m%03d@example.com", i)
		x.addProfile(p)
		members = append(members, p)
	}
	x.teams.members["acme/big"] = members
	x.svc.EmitIssue(ctx(), "acme/repo", 0, "mentioned", "bob@example.com", "", "", []string{"acme/big"})
	ev := x.svc.readActivity(ctx(), "acme", "repo", 1)
	var payload activityPayload
	_ = json.Unmarshal(ev.Payload, &payload)
	if len(payload.Recipients) != MaxTeamFanout {
		t.Fatalf("team capped at %d, got %d", MaxTeamFanout, len(payload.Recipients))
	}
}

// --- http edges ------------------------------------------------------------------------

func TestPrincipalFallbackAnonymous(t *testing.T) {
	x := newHarness(t)
	h := &Handler{Svc: x.svc} // nil Auth → anonymous
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/notifications", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("nil auth = %d", rec.Code)
	}
}

func TestWriteErrMatrix(t *testing.T) {
	kinds := map[auth.AuthErrorKind]int{
		auth.ErrForbidden: 403, auth.ErrUnavailable: 503, auth.ErrInvalid: 401,
	}
	for kind, want := range kinds {
		rec := httptest.NewRecorder()
		writeErr(rec, &auth.AuthError{Kind: kind, Why: "nope"})
		if rec.Code != want {
			t.Fatalf("kind %v = %d", kind, rec.Code)
		}
		if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
			t.Fatal("errors are plain text")
		}
	}
	rec := httptest.NewRecorder()
	writeErr(rec, errors.New("boom"))
	if rec.Code != 500 {
		t.Fatalf("plain err = %d", rec.Code)
	}
}

func TestWriters(t *testing.T) {
	rec := httptest.NewRecorder()
	writePlain(rec, http.StatusServiceUnavailable, "down")
	if rec.Header().Get("Retry-After") != "15" {
		t.Fatal("503 carries Retry-After")
	}
	rec = httptest.NewRecorder()
	writeJSON(rec, 200, make(chan int))
	if rec.Code != 500 {
		t.Fatalf("unmarshalable = %d", rec.Code)
	}
	if decodeSegment("%zz") != "%zz" {
		t.Fatal("undecodable segment survives verbatim")
	}
	if decodeSegment("a%20b") != "a b" {
		t.Fatal("segment decodes")
	}
	if err := readJSON(httptest.NewRequest("POST", "/", errReader{}), &HookSpec{}); err == nil {
		t.Fatal("body error must surface")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read boom") }
func (errReader) Close() error             { return nil }

func TestStreamUnsupported(t *testing.T) {
	x := newHarness(t)
	h := &Handler{Svc: x.svc}
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "amy@example.com"}, nil
	}
	// Writer without Flush → 406.
	rec := httptest.NewRecorder()
	w := noFlushWriter{rec: rec}
	r := httptest.NewRequest("GET", "/api/v1/notifications/stream", nil)
	h.ServeHTTP(w, r)
	if rec.Code != http.StatusNotAcceptable {
		t.Fatalf("non-flusher = %d", rec.Code)
	}
}

type noFlushWriter struct{ rec *httptest.ResponseRecorder }

func (w noFlushWriter) Header() http.Header         { return w.rec.Header() }
func (w noFlushWriter) Write(b []byte) (int, error) { return w.rec.Write(b) }
func (w noFlushWriter) WriteHeader(c int)           { w.rec.WriteHeader(c) }

type failWriter struct{ header http.Header }

func (w failWriter) Header() http.Header       { return w.header }
func (w failWriter) Write([]byte) (int, error) { return 0, errors.New("write boom") }
func (w failWriter) WriteHeader(int)           {}
func (w failWriter) Flush()                    {}

func TestSSEWriterEdges(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	w, ok := newSSEWriter(failWriter{header: http.Header{}}, r)
	if !ok {
		t.Fatal("failWriter flushes (Flush exists); writes fail later")
	}
	if w.event("x", "{}") {
		t.Fatal("failed write must report gone")
	}
	if w.comment("x") {
		t.Fatal("write after end must stop")
	}
	w.close() // idempotent
	w.close()
	// Event after close reports ended.
	rec := httptest.NewRecorder()
	w2, ok := newSSEWriter(rec, r)
	if !ok {
		t.Fatal("recorder must flush")
	}
	w2.close()
	if w2.event("x", "{}") {
		t.Fatal("event after close must fail")
	}
	// Direct comment path (the 10 s ticker never fires in tests).
	w3, ok := newSSEWriter(httptest.NewRecorder(), r)
	if !ok {
		t.Fatal("recorder must flush")
	}
	defer w3.close()
	if !w3.comment(": test") {
		t.Fatal("comment on open writer must succeed")
	}
}

func TestHandleRepoEdges(t *testing.T) {
	x := newHarness(t)
	x.roles.grant("acme", "repo", "amy@example.com", "admin")
	// Anonymous on webhook routes → 401.
	rec := do(x.handler, req(t, "GET", "/acme/repo/api/webhooks", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon webhooks = %d", rec.Code)
	}
	// Malformed JSON → 400.
	r := httptest.NewRequest("POST", "/acme/repo/api/webhooks", strings.NewReader("{bad"))
	r.Header.Set("X-Test-Principal", "amy@example.com")
	if rec := do(x.handler, r); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json = %d", rec.Code)
	}
	// Watch wrong method → 405.
	rec = do(x.handler, req(t, "POST", "/acme/repo/api/watch", "amy@example.com"))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("watch POST = %d", rec.Code)
	}
	// Lane twins + .git suffix route identically.
	for _, path := range []string{"/acme/repo.git/api/watch", "/acme/repo/api-browser/watch"} {
		rec = do(x.handler, req(t, "GET", path, "amy@example.com"))
		if rec.Code != 200 {
			t.Fatalf("%s = %d", path, rec.Code)
		}
	}
	// Invalid repo id falls through (core answers).
	badReq := &http.Request{Method: "GET", URL: &url.URL{Path: "/has space/repo/api/watch"}, Header: http.Header{}}
	badReq.Header.Set("X-Test-Principal", "amy@example.com")
	rec = do(x.handler, badReq)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bad repoid = %d", rec.Code)
	}
	// Unknown user sub-route + wrong methods → 404.
	for _, tc := range []struct{ method, path string }{
		{"DELETE", "/api/v1/notifications"},
		{"GET", "/api/v1/notifications/read_all"},
		{"PUT", "/api/v1/notifications/read_all"},
		{"GET", "/api/v1/notifications/" + strings.Repeat("0", 32)},
		{"POST", "/api/v1/notifications/stream"},
		{"POST", "/acme/repo/api/webhooks/x/bogus"},
	} {
		if rec := do(x.handler, req(t, tc.method, tc.path, "amy@example.com")); rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d", tc.method, tc.path, rec.Code)
		}
	}
	// Auth-chain errors map through writeErr.
	h := &Handler{Svc: x.svc}
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrForbidden, Why: "nope"}
	}
	rec = do(h, req(t, "GET", "/api/v1/notifications", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("auth chain = %d", rec.Code)
	}
}

// --- tray edges ----------------------------------------------------------------------------

func TestTrayEdges(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "acme", "repo")
	x.addProfile("amy@example.com", "bob@example.com")
	who := "amy@example.com"
	// Corrupt index + corrupt object: tray degrades to served entries.
	writeRaw(t, x.svc.Store, NotifIndexKey(who), []byte("{bad"))
	writeRaw(t, x.svc.Store, NotifPrefix(who)+"zz.json", []byte("{bad"))
	got, more := x.svc.Tray(ctx(), who, "", "", 50)
	if got == nil || more {
		t.Fatalf("corrupt tray = %+v/%v", got, more)
	}
	// Seed three notifications across two emissions (author + mentioned).
	x.writeThread(t, "acme", "repo", 7, "T", "amy@example.com")
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", []string{})
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "mentioned", "bob@example.com", "", "", []string{"amy@example.com"})
	// Unknown cursor → first page.
	got, _ = x.svc.Tray(ctx(), who, "", "nope", 50)
	if len(got) != 2 {
		t.Fatalf("unknown cursor = %d", len(got))
	}
	// n=1 → more.
	got, more = x.svc.Tray(ctx(), who, "", "", 1)
	if len(got) != 1 || !more {
		t.Fatalf("paged = %d/%v", len(got), more)
	}
	// n>200 caps.
	got, _ = x.svc.Tray(ctx(), who, "", "", 500)
	if len(got) != 2 {
		t.Fatalf("cap = %d", len(got))
	}
}

func TestSortNotificationsEqualAt(t *testing.T) {
	all := []Notification{
		{ID: "b", CreatedAt: "2026-09-04T12:00:00Z"},
		{ID: "a", CreatedAt: "2026-09-04T12:00:00Z"},
		{ID: "c", CreatedAt: "2026-09-04T13:00:00Z"},
	}
	sortNotifications(all)
	if all[0].ID != "c" || all[1].ID != "a" || all[2].ID != "b" {
		t.Fatalf("order = %+v", all)
	}
}

func TestUnreadCountCorrupt(t *testing.T) {
	x := newHarness(t)
	if got := x.svc.UnreadCount(ctx(), "nobody@example.com"); got != 0 {
		t.Fatalf("absent = %d", got)
	}
	writeRaw(t, x.svc.Store, NotifIndexKey("amy@example.com"), []byte("{bad"))
	if got := x.svc.UnreadCount(ctx(), "amy@example.com"); got != 0 {
		t.Fatalf("corrupt = %d", got)
	}
}

func TestSetStateEdges(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com")
	// Missing id → 404.
	if _, err := x.svc.SetState(ctx(), "amy@example.com", strings.Repeat("0", 32), true); err == nil {
		t.Fatal("missing must 404")
	}
	// Corrupt object → 404.
	writeRaw(t, x.svc.Store, NotifKey("amy@example.com", strings.Repeat("1", 32)), []byte("{bad"))
	if _, err := x.svc.SetState(ctx(), "amy@example.com", strings.Repeat("1", 32), true); err == nil {
		t.Fatal("corrupt must 404")
	}
	emitComment(t, x, "bob@example.com", []string{"amy@example.com"})
	raw, _, _ := store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("amy@example.com"), store.GetOptions{})
	var ix IndexDoc
	_ = json.Unmarshal(raw, &ix)
	id := ix.Entries[0].ID
	// Flip twice → second is a no-op success.
	if _, err := x.svc.SetState(ctx(), "amy@example.com", id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := x.svc.SetState(ctx(), "amy@example.com", id, true); err != nil {
		t.Fatal(err)
	}
	// Object without index entry: object flips, index no-ops.
	_ = x.svc.Store.Delete(ctx(), NotifIndexKey("amy@example.com"), "")
	if _, err := x.svc.SetState(ctx(), "amy@example.com", id, false); err != nil {
		t.Fatal(err)
	}
	// Index without the entry: no-op.
	writeRaw(t, x.svc.Store, NotifIndexKey("zed@example.com"), mustEncode(t, IndexDoc{Version: 1}))
	if err := x.svc.indexFlip(ctx(), "zed@example.com", strings.Repeat("2", 32), StateRead); err != nil {
		t.Fatal(err)
	}
	if err := x.svc.indexFlip(ctx(), "nobody@example.com", "x", StateRead); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, x.svc.Store, NotifIndexKey("corrupt@example.com"), []byte("{bad"))
	if err := x.svc.indexFlip(ctx(), "corrupt@example.com", "x", StateRead); err != nil {
		t.Fatal(err)
	}
}

func TestReadAllEdges(t *testing.T) {
	x := newHarness(t)
	if got := x.svc.ReadAll(ctx(), "nobody@example.com"); got != 0 {
		t.Fatalf("absent = %d", got)
	}
	writeRaw(t, x.svc.Store, NotifIndexKey("amy@example.com"), []byte("{bad"))
	if got := x.svc.ReadAll(ctx(), "amy@example.com"); got != 0 {
		t.Fatalf("corrupt = %d", got)
	}
	// Index entry whose object is gone: skipped, others update.
	x.addProfile("amy@example.com", "bob@example.com", "carol@example.com")
	x.writeThread(t, "acme", "repo", 9, "T", "bob@example.com")
	x.svc.EmitIssue(ctx(), "acme/repo", 9, "subscribed", "bob@example.com", "", "commented", []string{"carol@example.com"})
	raw, _, _ := store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("carol@example.com"), store.GetOptions{})
	var ix IndexDoc
	_ = json.Unmarshal(raw, &ix)
	_ = x.svc.Store.Delete(ctx(), NotifKey("carol@example.com", ix.Entries[0].ID), "")
	if got := x.svc.ReadAll(ctx(), "carol@example.com"); got != 0 {
		t.Fatalf("missing object skips: %d", got)
	}
}

// --- watch/social edges ------------------------------------------------------------------------

func TestSetWatchEdges(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "acme", "repo")
	seedRepo(t, x, "acme", "repo2")
	seedRepo(t, x, "acme", "repo3")
	// Idempotent double-watch: no dup, count stable.
	if _, err := x.svc.SetWatch(ctx(), "amy@example.com", "acme", "repo", true); err != nil {
		t.Fatal(err)
	}
	st, err := x.svc.SetWatch(ctx(), "amy@example.com", "acme", "repo", true)
	if err != nil || !st.Watching || st.Watchers != 1 {
		t.Fatalf("re-watch = %+v, %v", st, err)
	}
	// Corrupt social fails the write.
	writeRaw(t, x.svc.Store, SocialKey("acme", "repo2"), []byte("{bad"))
	if _, err := x.svc.SetWatch(ctx(), "amy@example.com", "acme", "repo2", true); err == nil {
		t.Fatal("corrupt social must fail")
	}
	// Count-drift repair on unwatch of a non-member.
	writeRaw(t, x.svc.Store, SocialKey("acme", "repo3"), mustEncode(t, SocialDoc{Watchers: 5}))
	st, err = x.svc.SetWatch(ctx(), "amy@example.com", "acme", "repo3", false)
	if err != nil || st.Watchers != 0 {
		t.Fatalf("drift repair = %+v, %v", st, err)
	}
	// Store error surfaces.
	svc2 := New(errStore{store.NewMemory(), errors.New("down")}, nil)
	if _, err := svc2.SetWatch(ctx(), "a", "o", "r", true); err == nil {
		t.Fatal("store error must surface")
	}
	if got := svc2.watcherCount(ctx(), "o", "r"); got != 0 {
		t.Fatalf("err count = %d", got)
	}
	writeRaw(t, x.svc.Store, SocialKey("acme", "repo4"), []byte("{bad"))
	if got := x.svc.watcherCount(ctx(), "acme", "repo4"); got != 0 {
		t.Fatalf("corrupt count = %d", got)
	}
}

// --- webhook edges -------------------------------------------------------------------------------

func TestListHooksSkipsJunk(t *testing.T) {
	x := newHarness(t)
	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "a", HookSpec{URL: strPtr("https://example.com/h")})
	if err != nil {
		t.Fatal(err)
	}
	writeRaw(t, x.svc.Store, WebhooksPrefix("acme", "repo")+"junk.txt", []byte("{}"))
	writeRaw(t, x.svc.Store, WebhooksPrefix("acme", "repo")+"bad.json", []byte("{bad"))
	hooks, err := x.svc.ListHooks(ctx(), "acme", "repo")
	if err != nil || len(hooks) != 1 || hooks[0].ID != hk.ID {
		t.Fatalf("list = %+v, %v", hooks, err)
	}
	if _, err := x.svc.ListHooks(ctx(), "acme", "repo2"); err != nil {
		// Absent prefix lists empty (memory LIST of missing prefix).
		t.Fatalf("empty list = %v", err)
	}
	svc2 := New(failListStore{store.NewMemory()}, nil)
	if _, err := svc2.ListHooks(ctx(), "a", "r"); err == nil {
		t.Fatal("list error must surface")
	}
}

func TestPatchHookEdges(t *testing.T) {
	x := newHarness(t)
	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "a", HookSpec{URL: strPtr("https://example.com/h")})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range []HookSpec{
		{URL: strPtr("gopher://x")},
		{Events: []string{"bogus"}},
	} {
		if _, err := x.svc.PatchHook(ctx(), "acme", "repo", hk.ID, spec); err == nil {
			t.Fatalf("bad patch %+v must fail", spec)
		}
	}
	// Corrupt stored hook.
	writeRaw(t, x.svc.Store, HookKey("acme", "repo2", "h"), []byte("{bad"))
	if _, err := x.svc.PatchHook(ctx(), "acme", "repo2", "h", HookSpec{}); err == nil {
		t.Fatal("corrupt hook must fail")
	}
	if x.svc.GetHook(ctx(), "acme", "repo", "missing") != nil {
		t.Fatal("missing hook must read nil")
	}
	writeRaw(t, x.svc.Store, HookKey("acme", "repo3", "h"), []byte("{bad"))
	if x.svc.GetHook(ctx(), "acme", "repo3", "h") != nil {
		t.Fatal("corrupt hook must read nil")
	}
}

func TestPingEdges(t *testing.T) {
	x := newHarness(t)
	if _, err := x.svc.PingHook(ctx(), "acme", "repo", "missing", "a"); err == nil {
		t.Fatal("ping of missing hook must 404")
	}
	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "a", HookSpec{URL: strPtr("https://example.com/h")})
	if err != nil {
		t.Fatal(err)
	}
	inactive := false
	if _, err := x.svc.PatchHook(ctx(), "acme", "repo", hk.ID, HookSpec{Active: &inactive}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.svc.PingHook(ctx(), "acme", "repo", hk.ID, "a"); err == nil {
		t.Fatal("ping of inactive hook must fail")
	}
}

func TestDeliverHookGapAndBadURL(t *testing.T) {
	x := newHarness(t)
	// Gap: seqs 1 and 3 exist, 2 is an honest gap. Cursor must pass the
	// gap (counted) and deliver both sides.
	writeRaw(t, x.svc.Store, HookKey("acme", "repo", strings.Repeat("h", 24)),
		mustEncode(t, &Hook{ID: strings.Repeat("h", 24), URL: "http://127.0.0.1:9/hook", Active: true, Version: 1}))
	for _, seq := range []int{1, 3} {
		ev := ActivityEvent{Seq: seq, Repo: "acme/repo", Action: "commented", Kind: "issue", At: x.now.Format(dateTimeFmt)}
		writeRaw(t, x.svc.Store, ActivityKey("acme", "repo", seq), mustEncode(t, ev))
	}
	h := x.svc.GetHook(ctx(), "acme", "repo", strings.Repeat("h", 24))
	x.svc.deliverHook(ctx(), "acme", "repo", h)
	if cur := x.svc.readCursor(ctx(), "acme", "repo", h.ID); cur != 0 {
		t.Fatalf("failed posts hold cursor at %d", cur)
	}
	d := x.svc.ReadDeliveries(ctx(), "acme", "repo", h.ID)
	if len(d.Entries) != 1 {
		t.Fatalf("only attempted posts record: %+v", d.Entries)
	}
	// Bad-URL hook: postEvent NewRequest fails fast, no crash.
	writeRaw(t, x.svc.Store, HookKey("acme", "repo", strings.Repeat("i", 24)),
		mustEncode(t, &Hook{ID: strings.Repeat("i", 24), URL: "http://127.0.0.1:9/\x7f", Active: true, Version: 1}))
	hi := x.svc.GetHook(ctx(), "acme", "repo", strings.Repeat("i", 24))
	if status, err := x.svc.postEvent(ctx(), hi, &ActivityEvent{Seq: 1}); err == nil || status != 0 {
		t.Fatalf("bad url = %d, %v", status, err)
	}
}

func TestCursorAndDeliveriesEdges(t *testing.T) {
	x := newHarness(t)
	writeRaw(t, x.svc.Store, CursorKey("acme", "repo", "h"), []byte("{bad"))
	if got := x.svc.readCursor(ctx(), "acme", "repo", "h"); got != 0 {
		t.Fatalf("corrupt cursor = %d", got)
	}
	writeRaw(t, x.svc.Store, CursorKey("acme", "repo", "n"), mustEncode(t, CursorDoc{PublishedSeq: -3}))
	if got := x.svc.readCursor(ctx(), "acme", "repo", "n"); got != 0 {
		t.Fatalf("negative cursor = %d", got)
	}
	// Monotonic: never retreats.
	x.svc.advanceCursor(ctx(), "acme", "repo", "m", 5)
	x.svc.advanceCursor(ctx(), "acme", "repo", "m", 3)
	if got := x.svc.readCursor(ctx(), "acme", "repo", "m"); got != 5 {
		t.Fatalf("cursor = %d", got)
	}
	// Corrupt cursor: advance leaves it alone.
	writeRaw(t, x.svc.Store, CursorKey("acme", "repo", "c"), []byte("{bad"))
	x.svc.advanceCursor(ctx(), "acme", "repo", "c", 5)
	if got := x.svc.readCursor(ctx(), "acme", "repo", "c"); got != 0 {
		t.Fatalf("corrupt cursor advanced to %d", got)
	}
	// Ring trims to MaxDeliveries.
	ev := &ActivityEvent{Seq: 1, Action: "commented"}
	for i := 0; i < MaxDeliveries+5; i++ {
		x.svc.recordDelivery(ctx(), "acme", "repo", "r", ev, 200, nil)
	}
	d := x.svc.ReadDeliveries(ctx(), "acme", "repo", "r")
	if len(d.Entries) != MaxDeliveries {
		t.Fatalf("ring = %d", len(d.Entries))
	}
	if d := x.svc.ReadDeliveries(ctx(), "acme", "repo2", "missing"); d.Entries == nil {
		t.Fatal("absent ring must be [], never null")
	}
	writeRaw(t, x.svc.Store, DeliveriesKey("acme", "repo3", "bad"), []byte("{bad"))
	if d := x.svc.ReadDeliveries(ctx(), "acme", "repo3", "bad"); d.Entries == nil {
		t.Fatal("corrupt ring must be [], never null")
	}
}

// --- task edges ------------------------------------------------------------------------------------

func TestStartWebhooksBadRepo(t *testing.T) {
	x := newHarness(t)
	if rec := x.svc.StartWebhooks(ctx(), "noslash"); rec != nil {
		t.Fatalf("bad repo task = %+v", rec)
	}
	if rec := x.svc.TaskStatus("acme/repo", "never"); rec != nil {
		t.Fatalf("unknown task = %+v", rec)
	}
}

func TestEnqueueFanoutJoinedAndBadRepo(t *testing.T) {
	x := newHarness(t)
	// Manually begin the task, then enqueue joins it (attach path).
	e, joined := x.svc.tasks.begin("acme/repo", TaskKindFanout, x.now)
	if joined {
		t.Fatal("first begin must lead")
	}
	x.svc.enqueueFanout("acme/repo", 99)
	e2, joined := x.svc.tasks.begin("acme/repo", TaskKindFanout, x.now)
	if !joined || e2 != e {
		t.Fatal("enqueue must join the running task")
	}
	e.attach(99) // dup attach is a no-op
	x.svc.tasks.end("acme/repo", TaskKindFanout, "test", x.now)
	// Unsplittable repo ends the drain immediately.
	x.svc.enqueueFanout("noslash", 1)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if rec := x.svc.TaskStatus("noslash", TaskKindFanout); rec != nil && rec.State == TaskFinished {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bad-repo drain never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFanoutOneEdges(t *testing.T) {
	x := newHarness(t)
	// Missing activity → no-op.
	x.svc.fanoutOne(ctx(), "acme", "repo", "acme/repo", 42)
	// Existing unread → skip (no duplicate).
	x.addProfile("amy@example.com")
	ev := ActivityEvent{Seq: 7, Repo: "acme/repo", Action: "commented", Num: 1, Kind: "issue",
		Actor: "b", Title: "T", At: x.now.Format(dateTimeFmt),
		Payload: mustEncode(t, activityPayload{Class: "subscribed", Recipients: []activityRecipient{{Principal: "amy@example.com", Reason: ReasonSubscribed}}})}
	writeRaw(t, x.svc.Store, ActivityKey("acme", "repo", 7), mustEncode(t, ev))
	id := NotificationID("amy@example.com", "acme/repo", 1, ReasonSubscribed, 7)
	writeRaw(t, x.svc.Store, NotifKey("amy@example.com", id), mustEncode(t, Notification{ID: id}))
	if err := x.svc.indexAdd(ctx(), "amy@example.com", IndexEntry{ID: id, Repo: "acme/repo", Num: 1, Kind: "issue", Reason: ReasonSubscribed, State: StateUnread, At: x.now.Format(dateTimeFmt)}); err != nil {
		t.Fatal(err)
	}
	x.svc.fanoutOne(ctx(), "acme", "repo", "acme/repo", 7)
	if n := countNotifs(t, x, "amy@example.com"); n != 1 {
		t.Fatalf("fanout dup: %d", n)
	}
	// Create error path (failCreateStore): loads, then fails the write.
	st2 := store.NewMemory()
	writeRaw(t, st2, ActivityKey("acme", "repo", 8), mustEncode(t, ActivityEvent{Seq: 8, Repo: "acme/repo",
		Payload: mustEncode(t, activityPayload{Recipients: []activityRecipient{{Principal: "z@example.com", Reason: ReasonSubscribed}}})}))
	svc2 := New(failCreateStore{st2}, nil)
	svc2.fanoutOne(ctx(), "acme", "repo", "acme/repo", 8)
}

func TestSweepAndRun(t *testing.T) {
	x := newHarness(t)
	// Seed a hookless repo with activity + a hooked repo.
	writeRaw(t, x.svc.Store, ActivityKey("acme", "repo", 1),
		mustEncode(t, ActivityEvent{Seq: 1, Repo: "acme/repo", Action: "commented"}))
	if _, err := x.svc.CreateHook(ctx(), "acme", "repo2", "a", HookSpec{URL: strPtr("http://127.0.0.1:9/h")}); err != nil {
		t.Fatal(err)
	}
	x.svc.sweepWebhooks(ctx())
	// Run serves a wake then exits on cancel.
	rctx, cancel := context.WithCancel(ctx())
	go x.svc.Run(rctx)
	x.svc.wakeRepo("acme/repo2")
	time.Sleep(50 * time.Millisecond)
	cancel()
}

func TestRetentionCorruptUserIndex(t *testing.T) {
	x := newHarness(t)
	writeRaw(t, x.svc.Store, NotifPrefix("amy@example.com")+"index.json", []byte("{bad"))
	x.svc.RunRetention(ctx()) // must not crash; user skipped
}

func TestRetainRepoListError(t *testing.T) {
	st := store.NewMemory()
	writeRaw(t, st, CursorKey("acme", "repo", "h"), mustEncode(t, CursorDoc{PublishedSeq: 4}))
	svc := New(failListStore{st}, nil)
	svc.retainRepoEvents(ctx(), "acme", "repo", time.Now()) // ListHooks fails → return
	svc.RunRetention(ctx())                                 // enumerations fail → no-op
}

func TestTaskEndTrim(t *testing.T) {
	x := newHarness(t)
	for i := 0; i < 140; i++ {
		e, joined := x.svc.tasks.begin(fmt.Sprintf("r%d", i), TaskKindWebhooks, x.now)
		if joined {
			t.Fatal("distinct keys must not join")
		}
		_ = e
		x.svc.tasks.end(fmt.Sprintf("r%d", i), TaskKindWebhooks, "n", x.now)
	}
	x.svc.tasks.mu.Lock()
	n := len(x.svc.tasks.recent)
	x.svc.tasks.mu.Unlock()
	if n > 128 {
		t.Fatalf("recent cache unbounded: %d", n)
	}
	// Ending an unknown key is a no-op.
	x.svc.tasks.end("nope", TaskKindWebhooks, "n", x.now)
}
