// orghooks_cover_test.go — Forgejo #363: branch coverage for the
// org-hook defensive paths (corrupt objects, gaps, transport failure,
// sweep prune/park, join, cancel). White-box, same harness.
package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

func putRaw(t *testing.T, x *harness, key string, body []byte) {
	t.Helper()
	if _, err := store.PutBytes(ctx(), x.svc.Store, key, body,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
}

func TestOrgHookCorruptObjects(t *testing.T) {
	x, _ := orgHarness(t)
	putRaw(t, x, OrgHookKey("acme", "junk"), []byte("{oops"))
	if got := x.svc.GetOrgHook(ctx(), "acme", "junk"); got != nil {
		t.Fatalf("corrupt get = %+v", got)
	}
	if _, err := x.svc.PatchOrgHook(ctx(), "acme", "junk", HookSpec{}); !isErr(err, ErrInvalid) {
		t.Fatalf("corrupt patch = %v, want invalid", err)
	}
	if _, err := x.svc.PatchOrgHook(ctx(), "acme", "junk",
		HookSpec{URL: strPtr("http://hooks.example/x")}); !isErr(err, ErrInvalid) {
		t.Fatalf("corrupt patch url = %v, want invalid", err)
	}
	// A corrupt entry is skipped by the list, not fatal.
	hk, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{URL: strPtr("https://hooks.example/x")})
	if err != nil {
		t.Fatal(err)
	}
	hooks, err := x.svc.ListOrgHooks(ctx(), "acme")
	if err != nil || len(hooks) != 1 || hooks[0].ID != hk.ID {
		t.Fatalf("list = %+v, %v", hooks, err)
	}
}

func TestOrgHookPatchValidation(t *testing.T) {
	x, _ := orgHarness(t)
	hk, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{URL: strPtr("https://hooks.example/x")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.svc.PatchOrgHook(ctx(), "acme", "nope", HookSpec{}); !isErr(err, ErrNotFound) {
		t.Fatalf("patch unknown = %v", err)
	}
	if _, err := x.svc.PatchOrgHook(ctx(), "acme", hk.ID,
		HookSpec{URL: strPtr("http://hooks.example/x")}); !isErr(err, ErrInvalid) {
		t.Fatalf("patch bad url = %v", err)
	}
	if _, err := x.svc.PatchOrgHook(ctx(), "acme", hk.ID,
		HookSpec{Events: []string{"push"}}); !isErr(err, ErrInvalid) {
		t.Fatalf("patch bad events = %v", err)
	}
	// Secret rotation + insecure flag round-trip (secret stays hidden).
	rot, err := x.svc.PatchOrgHook(ctx(), "acme", hk.ID,
		HookSpec{Secret: strPtr("n3w"), InsecureTLS: boolPtr(true), URL: strPtr("https://hooks.example/y")})
	if err != nil || rot.Secret != "n3w" || !rot.InsecureTLS || rot.URL != "https://hooks.example/y" {
		t.Fatalf("patch rotate = %+v, %v", rot, err)
	}
	// Reactivation arms a sweep (pending mark).
	if _, err := x.svc.PatchOrgHook(ctx(), "acme", hk.ID, HookSpec{Active: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	x.svc.hookMu.Lock()
	pending := x.svc.orgHookPending["acme"]
	x.svc.hookMu.Unlock()
	if !pending {
		t.Fatal("reactivation did not arm the org sweep")
	}
}

func TestOrgHookCreateBadOrg(t *testing.T) {
	x, _ := orgHarness(t)
	if _, err := x.svc.CreateOrgHook(ctx(), "ACME!", "amy@example.com",
		HookSpec{URL: strPtr("https://hooks.example/x")}); !isErr(err, ErrInvalid) {
		t.Fatalf("bad org create = %v", err)
	}
	// Inactive creates arm nothing.
	hk, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com",
		HookSpec{URL: strPtr("https://hooks.example/x"), Active: boolPtr(false)})
	if err != nil || hk.Active {
		t.Fatalf("inactive create = %+v, %v", hk, err)
	}
	x.svc.hookMu.Lock()
	pending := x.svc.orgHookPending["acme"]
	x.svc.hookMu.Unlock()
	if pending {
		t.Fatal("inactive create armed the org sweep")
	}
	// Insecure-TLS opt-in round-trips on create.
	ins, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com",
		HookSpec{URL: strPtr("https://hooks.example/i"), InsecureTLS: boolPtr(true)})
	if err != nil || !ins.InsecureTLS {
		t.Fatalf("insecure create = %+v, %v", ins, err)
	}
}

func TestOrgHookNilCheckerAnon(t *testing.T) {
	x := newHarness(t) // no OrgOwner wired
	rec := do(x.handler, req(t, "GET", "/api/v1/orgs/acme/webhooks", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("nil-checker anon = %d, want 401", rec.Code)
	}
	// The handler rejects anonymous before the gate; the gate itself is
	// fail-closed too (direct call).
	if err := x.svc.requireOrgOwner(ctx(), "acme", auth.Anonymous()); !isErr(err, ErrUnauthorized) {
		t.Fatalf("nil-checker gate anon = %v", err)
	}
	if err := x.svc.requireOrgOwner(ctx(), "acme", auth.Principal{Name: "bob@example.com"}); !isErr(err, ErrForbidden) {
		t.Fatalf("nil-checker gate authed = %v", err)
	}
}

// oddKindChecker returns a non-Forbidden, non-Unavailable denial.
type oddKindChecker struct{}

func (oddKindChecker) CheckOrgOwner(_ context.Context, _ string, _ auth.Principal) *auth.AuthError {
	return &auth.AuthError{Kind: auth.ErrInvalid, Why: "odd"}
}

func TestOrgHookCheckerOddKind(t *testing.T) {
	x := newHarness(t)
	x.svc.OrgOwner = oddKindChecker{}
	if err := x.svc.requireOrgOwner(ctx(), "acme", auth.Principal{Name: "bob@example.com"}); !isErr(err, ErrUnauthorized) {
		t.Fatalf("odd-kind gate = %v, want unauthorized", err)
	}
}

func TestOrgHookGarbageState(t *testing.T) {
	x, _ := orgHarness(t)
	hk, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{URL: strPtr("https://hooks.example/x")})
	if err != nil {
		t.Fatal(err)
	}
	putRaw(t, x, OrgHookStateKey("acme"), []byte("{oops"))
	// Reserve fails; emission drops with the head untouched.
	if _, err := x.svc.reserveOrgSeq(ctx(), "acme"); !isErr(err, ErrInvalid) {
		t.Fatalf("reserve corrupt = %v", err)
	}
	x.svc.EmitOrgEvent(ctx(), "acme", OrgActionMemberAdded, "", "x")
	if head := x.svc.orgHead(ctx(), "acme"); head != 0 {
		t.Fatalf("head after corrupt reserve = %d", head)
	}
	if _, err := x.svc.PingOrgHook(ctx(), "acme", hk.ID, "amy"); err == nil {
		t.Fatal("ping on corrupt state wants an error")
	}
	// A negative allocator clamps to 1 (never 0, never negative).
	putRaw(t, x, OrgHookStateKey("neg"), []byte(`{"next_seq":-5}`))
	seq, err := x.svc.reserveOrgSeq(ctx(), "neg")
	if err != nil || seq != 1 {
		t.Fatalf("negative clamp = %d, %v", seq, err)
	}
	// Corrupt repo state reads head -1 (never delivered, never compacted).
	seedRepo(t, x, "acme", "one")
	putRaw(t, x, CollabStateKey("acme", "one"), []byte("{oops"))
	if head := x.svc.repoHead(ctx(), "acme", "one"); head != -1 {
		t.Fatalf("corrupt repo head = %d, want -1", head)
	}
}

func TestOrgHookGapJump(t *testing.T) {
	x, _ := orgHarness(t)
	sink, srv := newSink(t, "")
	defer srv.Close()
	hk, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{URL: strPtr(srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	// Seq 1 is corrupt (reads as absent); seq 2 is valid: the pass
	// counts the gap and delivers from the oldest readable event.
	for _, seq := range []int{1, 2} {
		if _, err := x.svc.reserveOrgSeq(ctx(), "acme"); err != nil {
			t.Fatal(err)
		}
		_ = seq
	}
	putRaw(t, x, OrgEventKey("acme", 1), []byte("{oops"))
	x.svc.EmitOrgEvent(ctx(), "acme", OrgActionMemberAdded, "", "second")
	// EmitOrgEvent reserved seq 3 (1, 2 taken above) — rewrite: drop it
	// and hand-write seq 2 so the window is [corrupt, valid].
	_ = x.svc.Store.Delete(ctx(), OrgEventKey("acme", 3), "")
	ev := ActivityEvent{Seq: 2, Repo: "acme", Action: OrgActionMemberAdded, Kind: OrgHookKind, At: x.now.Format(dateTimeFmt)}
	putRaw(t, x, OrgEventKey("acme", 2), mustEncode(t, ev))
	x.svc.DeliverOrg(ctx(), "acme")
	if got := sink.count(); got != 1 {
		t.Fatalf("gap posts = %d, want 1", got)
	}
	if c := readCursorAt(ctx(), x.svc, OrgHookCursorKey("acme", hk.ID)); c != 2 {
		t.Fatalf("gap cursor = %d, want 2", c)
	}
}

func TestOrgHookRepoGapJump(t *testing.T) {
	x, _ := orgHarness(t)
	x.addProfile("amy@example.com", "bob@example.com")
	seedRepo(t, x, "acme", "one")
	sink, srv := newSink(t, "")
	defer srv.Close()
	if _, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{URL: strPtr(srv.URL)}); err != nil {
		t.Fatal(err)
	}
	// Crash-reserved seq 1 (gap); the emission lands on seq 2.
	if _, err := x.svc.reserveSeq(ctx(), "acme", "one"); err != nil {
		t.Fatal(err)
	}
	x.writeThread(t, "acme", "one", 7, "T", "amy@example.com")
	x.svc.EmitIssue(ctx(), "acme/one", 7, "subscribed", "bob@example.com", "", "commented", []string{})
	x.svc.DeliverOrg(ctx(), "acme")
	if got := sink.count(); got != 1 {
		t.Fatalf("repo-gap posts = %d, want 1", got)
	}
}

func TestOrgHookTransportFailure(t *testing.T) {
	x, _ := orgHarness(t)
	_, srv := newSink(t, "")
	url := srv.URL
	srv.Close() // refused dial → derr path (cursor held, error scrubbed)
	hk, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{URL: strPtr(url)})
	if err != nil {
		t.Fatal(err)
	}
	x.svc.EmitOrgEvent(ctx(), "acme", OrgActionMemberAdded, "", "x")
	x.svc.DeliverOrg(ctx(), "acme")
	if c := readCursorAt(ctx(), x.svc, OrgHookCursorKey("acme", hk.ID)); c != 0 {
		t.Fatalf("failed cursor = %d, want 0", c)
	}
	d := x.svc.ReadOrgDeliveries(ctx(), "acme", hk.ID)
	if len(d.Entries) != 1 || d.Entries[0].Error == "" {
		t.Fatalf("failed ring = %+v", d.Entries)
	}
}

func TestOrgHookPingBypass(t *testing.T) {
	x, _ := orgHarness(t)
	sink, srv := newSink(t, "")
	defer srv.Close()
	hk, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com",
		HookSpec{URL: strPtr(srv.URL), Events: []string{OrgActionMemberAdded}})
	if err != nil {
		t.Fatal(err)
	}
	// A ping org event bypasses the filter (repo-hook parity).
	seq, err := x.svc.reserveOrgSeq(ctx(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	ev := ActivityEvent{Seq: seq, Repo: "acme", Action: ActionPing, Kind: OrgHookKind, At: x.now.Format(dateTimeFmt)}
	putRaw(t, x, OrgEventKey("acme", seq), mustEncode(t, ev))
	x.svc.DeliverOrg(ctx(), "acme")
	if got := sink.count(); got != 1 {
		t.Fatalf("ping-bypass posts = %d, want 1", got)
	}
	if c := readCursorAt(ctx(), x.svc, OrgHookCursorKey("acme", hk.ID)); c != seq {
		t.Fatalf("ping cursor = %d, want %d", c, seq)
	}
}

func TestOrgHookCancelledPass(t *testing.T) {
	x, _ := orgHarness(t)
	sink, srv := newSink(t, "")
	defer srv.Close()
	// Two hooks: the first takes the semaphore, the second observes the
	// dead context and breaks the launch loop (then waits in-flight).
	for i := 0; i < 2; i++ {
		if _, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{URL: strPtr(srv.URL)}); err != nil {
			t.Fatal(err)
		}
	}
	x.svc.EmitOrgEvent(ctx(), "acme", OrgActionMemberAdded, "", "x")
	cancelled, cancel := context.WithCancel(ctx())
	cancel()
	x.svc.DeliverOrg(cancelled, "acme")
	if got := sink.count(); got != 0 {
		t.Fatalf("cancelled posts = %d", got)
	}
}

func TestOrgHookRepoLagPending(t *testing.T) {
	x, _ := orgHarness(t)
	x.addProfile("amy@example.com", "bob@example.com")
	seedRepo(t, x, "acme", "one")
	sink, srv := newSink(t, "")
	defer srv.Close()
	if _, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{URL: strPtr(srv.URL)}); err != nil {
		t.Fatal(err)
	}
	// Repo-only event with a failing sink: the org half is clean but the
	// repo half lags → the pass leaves pending armed.
	sink.fail = true
	x.writeThread(t, "acme", "one", 7, "T", "amy@example.com")
	x.svc.EmitIssue(ctx(), "acme/one", 7, "subscribed", "bob@example.com", "", "commented", []string{})
	x.svc.DeliverOrg(ctx(), "acme")
	x.svc.hookMu.Lock()
	pending := x.svc.orgHookPending["acme"]
	x.svc.hookMu.Unlock()
	if !pending {
		t.Fatal("repo-lagged pass did not arm pending")
	}
	// Recovery converges on the next pass.
	sink.fail = false
	x.svc.DeliverOrg(ctx(), "acme")
	if got := sink.count(); got != 1 {
		t.Fatalf("recovered posts = %d, want 1", got)
	}
}

func TestOrgHookStartJoin(t *testing.T) {
	x, _ := orgHarness(t)
	_, srv := newSink(t, "")
	defer srv.Close()
	if _, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{URL: strPtr(srv.URL)}); err != nil {
		t.Fatal(err)
	}
	first := x.svc.StartOrgWebhooks(ctx(), "acme")
	if first == nil {
		t.Fatal("first start = nil")
	}
	second := x.svc.StartOrgWebhooks(ctx(), "acme")
	if second == nil || second.ID != first.ID {
		t.Fatalf("join = %+v, want %s", second, first.ID)
	}
	waitOrgTask(t, x, "acme")
}

func TestOrgHookSweepPruneAndPark(t *testing.T) {
	x, _ := orgHarness(t)
	// Ghost watermarks for an org that has no hooks are dropped, and no
	// pass starts.
	x.svc.hookMu.Lock()
	x.svc.orgHookSeen["ghost"] = 9
	x.svc.orgHookPending["ghost"] = true
	x.svc.orgRepoSeen["ghost\x00one"] = 4
	x.svc.hookMu.Unlock()
	x.svc.sweepOrgHooks(ctx())
	x.svc.hookMu.Lock()
	_, seenKept := x.svc.orgHookSeen["ghost"]
	_, pendKept := x.svc.orgHookPending["ghost"]
	_, repoKept := x.svc.orgRepoSeen["ghost\x00one"]
	x.svc.hookMu.Unlock()
	if seenKept || pendKept || repoKept {
		t.Fatal("ghost watermarks survive the sweep")
	}
	if rec := x.svc.TaskStatus(orgTaskRepo("ghost"), TaskKindOrgWebhooks); rec != nil {
		t.Fatalf("ghost sweep started %+v", rec)
	}
	// A fully parked org (inactive hook, drained) advances seen without
	// starting a pass. Inactive hooks short-circuit delivery itself.
	x.svc.DeliverOrg(ctx(), "acme")
	_, srv := newSink(t, "")
	defer srv.Close()
	if _, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com",
		HookSpec{URL: strPtr(srv.URL), Active: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	x.svc.EmitOrgEvent(ctx(), "acme", OrgActionMemberAdded, "", "x")
	x.svc.sweepOrgHooks(ctx())
	if rec := x.svc.TaskStatus(orgTaskRepo("acme"), TaskKindOrgWebhooks); rec != nil {
		t.Fatalf("parked sweep started %+v", rec)
	}
	x.svc.hookMu.Lock()
	seen := x.svc.orgHookSeen["acme"]
	x.svc.hookMu.Unlock()
	if seen != 1 {
		t.Fatalf("parked seen = %d, want 1", seen)
	}
	// An org visited with hooks listed but none configured prunes its
	// watermarks: a cursor-only subtree (no hook file) lists zero hooks.
	putRaw(t, x, OrgHookCursorKey("lonely", "h"), mustEncode(t, CursorDoc{PublishedSeq: 3}))
	x.svc.hookMu.Lock()
	x.svc.orgHookSeen["lonely"] = 3
	x.svc.orgHookPending["lonely"] = true
	x.svc.orgRepoSeen["lonely\x00one"] = 2
	x.svc.hookMu.Unlock()
	x.svc.sweepOrgHooks(ctx())
	x.svc.hookMu.Lock()
	_, seenKept = x.svc.orgHookSeen["lonely"]
	_, pendKept = x.svc.orgHookPending["lonely"]
	_, repoKept = x.svc.orgRepoSeen["lonely\x00one"]
	x.svc.hookMu.Unlock()
	if seenKept || pendKept || repoKept {
		t.Fatal("hookless-org watermarks survive the sweep")
	}
	// A coalesced wake channel drops (never blocks the mutating path).
	for i := 0; i < 64; i++ {
		x.svc.wakeOrg("acme")
	}
	x.svc.wakeOrg("acme") // 65th: default branch
}

func TestOrgHookCursorUnits(t *testing.T) {
	x, _ := orgHarness(t)
	// Corrupt cursor reads 0 and never writes (leave-for-next-pass).
	putRaw(t, x, OrgHookCursorKey("acme", "h"), []byte("{oops"))
	if got := readCursorAt(ctx(), x.svc, OrgHookCursorKey("acme", "h")); got != 0 {
		t.Fatalf("corrupt cursor = %d", got)
	}
	advanceCursorAt(ctx(), x.svc, OrgHookCursorKey("acme", "h"), 3)
	if got := readCursorAt(ctx(), x.svc, OrgHookCursorKey("acme", "h")); got != 0 {
		t.Fatalf("corrupt cursor after advance = %d", got)
	}
	// Negative cursors read 0; monotonic advance never retreats.
	putRaw(t, x, OrgHookCursorKey("acme", "n"), mustEncode(t, CursorDoc{PublishedSeq: -1}))
	if got := readCursorAt(ctx(), x.svc, OrgHookCursorKey("acme", "n")); got != 0 {
		t.Fatalf("negative cursor = %d", got)
	}
	advanceCursorAt(ctx(), x.svc, OrgHookRepoCursorKey("acme", "n", "one"), 5)
	advanceCursorAt(ctx(), x.svc, OrgHookRepoCursorKey("acme", "n", "one"), 2)
	if got := readCursorAt(ctx(), x.svc, OrgHookRepoCursorKey("acme", "n", "one")); got != 5 {
		t.Fatalf("retreated cursor = %d, want 5", got)
	}
}

func TestOrgHookEmitAppendConflict(t *testing.T) {
	x, _ := orgHarness(t)
	// A pre-existing event at the reserved seq 412s into success (the
	// retried-emission path): the head still advances, nothing drops.
	putRaw(t, x, OrgEventKey("acme", 1), []byte(`{"seq":1}`))
	x.svc.EmitOrgEvent(ctx(), "acme", OrgActionMemberAdded, "", "x")
	if head := x.svc.orgHead(ctx(), "acme"); head != 1 {
		t.Fatalf("head after 412-append = %d, want 1", head)
	}
}

func TestOrgHookPingAppendConflict(t *testing.T) {
	x, _ := orgHarness(t)
	sink, srv := newSink(t, "")
	defer srv.Close()
	hk, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{URL: strPtr(srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	// A pre-existing event at the ping seq 412s into success.
	putRaw(t, x, OrgEventKey("acme", 1), []byte(`{"seq":1}`))
	ok, err := x.svc.PingOrgHook(ctx(), "acme", hk.ID, "amy@example.com")
	if err != nil || !ok {
		t.Fatalf("ping on 412 = %v, %v", ok, err)
	}
	if got := sink.count(); got != 1 {
		t.Fatalf("ping posts = %d", got)
	}
}

func TestOrgHookPingPostFailure(t *testing.T) {
	x, _ := orgHarness(t)
	_, srv := newSink(t, "")
	url := srv.URL
	srv.Close()
	hk, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{URL: strPtr(url)})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := x.svc.PingOrgHook(ctx(), "acme", hk.ID, "amy@example.com")
	if err != nil || ok {
		t.Fatalf("failed ping = %v, %v (want false, nil)", ok, err)
	}
	d := x.svc.ReadOrgDeliveries(ctx(), "acme", hk.ID)
	if len(d.Entries) != 1 || d.Entries[0].Error == "" {
		t.Fatalf("failed ping ring = %+v", d.Entries)
	}
}

func TestOrgHookHTTPDeliveriesNoStore(t *testing.T) {
	x, _ := orgHarness(t)
	rec := orgCreate(t, x, "acme", "amy@example.com", `{"url":"https://hooks.example/x"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}
	var created map[string]any
	mustJSON(t, rec, &created)
	id, _ := created["id"].(string)
	r := httptest.NewRequest("GET", "/api/v1/orgs/acme/webhooks/"+id+"/deliveries", nil)
	r.Header.Set("X-Test-Principal", "amy@example.com")
	rec = do(x.handler, r)
	if rec.Code != 200 {
		t.Fatalf("deliveries = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("deliveries cache = %q", cc)
	}
	var d DeliveriesDoc
	mustJSON(t, rec, &d)
	if d.Entries == nil {
		t.Fatal("deliveries entries must be [], never null")
	}
}
