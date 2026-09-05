// cover2_test.go — second wave: recipient guards, partial shortfall,
// webhook keeper edges, retention scan branches, bus rings.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

func TestResolveRecipientGuards(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "acme", "repo")
	x.addProfile("amy@example.com", "bob@example.com", "carol@example.com")
	x.writeThread(t, "acme", "repo", 7, "T", "amy@example.com")
	// Empty + actor + duplicate primaries hit every guard; carol lands once.
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented",
		[]string{"", "BOB@EXAMPLE.COM", "carol@example.com", "carol@example.com"})
	if n := countNotifs(t, x, "carol@example.com"); n != 1 {
		t.Fatalf("carol = %d", n)
	}
	if n := countNotifs(t, x, "bob@example.com"); n != 0 {
		t.Fatalf("actor = %d", n)
	}
	// Watcher who is the thread author takes the author branch, not subscribed.
	if _, err := x.svc.SetWatch(ctx(), "amy@example.com", "acme", "repo", true); err != nil {
		t.Fatal(err)
	}
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", []string{"carol@example.com"})
	raw, _, _ := store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("amy@example.com"), store.GetOptions{})
	var ix IndexDoc
	_ = json.Unmarshal(raw, &ix)
	for _, en := range ix.Entries {
		if en.Reason == ReasonSubscribed {
			t.Fatalf("author must not take subscribed: %+v", ix.Entries)
		}
	}
}

func TestTeamMemberGuards(t *testing.T) {
	x := newHarness(t)
	x.addProfile("bob@example.com", "m1@example.com")
	x.teams.members["acme/t"] = []string{"", "bob@example.com", "amy@example.com", "m1@example.com", "ghost@example.com"}
	x.writeThread(t, "acme", "repo", 7, "T", "amy@example.com")
	// Subscribed-class team expansion: empty/actor/author/probeless members
	// exercise every guard; m1 lands once.
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", []string{"acme/t"})
	if n := countNotifs(t, x, "m1@example.com"); n != 1 {
		t.Fatalf("m1 = %d", n)
	}
	if n := countNotifs(t, x, "amy@example.com"); n != 1 {
		t.Fatalf("author = %d (author reason only)", n)
	}
}

func TestEmitReserveFailure(t *testing.T) {
	x := newHarness(t)
	writeRaw(t, x.svc.Store, CollabStateKey("acme", "repo"), []byte("{bad"))
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", []string{"amy@example.com"})
	if ev := x.svc.readActivity(ctx(), "acme", "repo", 1); ev != nil {
		t.Fatal("reserve failure must emit nothing")
	}
}

func TestPartialShortfallPublishesDone(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com", "carol@example.com")
	x.writeThread(t, "acme", "repo", 7, "T", "amy@example.com")
	// Corrupt carol's index: her target fails (indexAdd), amy's completes.
	// Shortfall path publishes amy's frame + arms the task.
	writeRaw(t, x.svc.Store, NotifIndexKey("carol@example.com"), []byte("{bad"))
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented",
		[]string{"amy@example.com", "carol@example.com"})
	if n := countNotifs(t, x, "amy@example.com"); n != 1 {
		t.Fatalf("amy = %d", n)
	}
	if ev := x.svc.readActivity(ctx(), "acme", "repo", 1); ev == nil {
		t.Fatal("shortfall must still append activity")
	}
	if rec := x.svc.TaskStatus("acme/repo", TaskKindFanout); rec == nil {
		t.Fatal("shortfall must arm notify-fanout")
	}
}

func TestIndexInPlaceFlipAndTrim(t *testing.T) {
	x := newHarness(t)
	// Seed a read entry, then re-add same id as unread: in-place flip +1.
	id := strings.Repeat("e", 32)
	at := x.now.Format(dateTimeFmt)
	if err := x.svc.indexAdd(ctx(), "amy@example.com", IndexEntry{ID: id, State: StateRead, At: at}); err != nil {
		t.Fatal(err)
	}
	if err := x.svc.indexAdd(ctx(), "amy@example.com", IndexEntry{ID: id, State: StateUnread, At: at}); err != nil {
		t.Fatal(err)
	}
	raw, _, _ := store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("amy@example.com"), store.GetOptions{})
	var ix IndexDoc
	_ = json.Unmarshal(raw, &ix)
	if len(ix.Entries) != 1 || ix.Entries[0].State != StateUnread || ix.UnreadCount != 1 {
		t.Fatalf("in-place flip = %+v", ix)
	}
	// Flip back: count floors without going negative.
	if err := x.svc.indexAdd(ctx(), "amy@example.com", IndexEntry{ID: id, State: StateRead, At: at}); err != nil {
		t.Fatal(err)
	}
	raw, _, _ = store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("amy@example.com"), store.GetOptions{})
	ix = IndexDoc{}
	_ = json.Unmarshal(raw, &ix)
	if ix.UnreadCount != 0 {
		t.Fatalf("floor = %+v", ix)
	}
	// Window trims past TrayPageSize.
	for i := 0; i < TrayPageSize+5; i++ {
		if err := x.svc.indexAdd(ctx(), "zed@example.com", IndexEntry{ID: strings.Repeat("f", 31) + string(rune('a'+i%6)) + string(rune('0'+i/6)), State: StateUnread, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	raw, _, _ = store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("zed@example.com"), store.GetOptions{})
	ix = IndexDoc{}
	_ = json.Unmarshal(raw, &ix)
	if len(ix.Entries) != TrayPageSize {
		t.Fatalf("trimmed window = %d", len(ix.Entries))
	}
}

func TestTrayCapAndStateAfterUnknown(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "acme", "repo")
	rec := do(x.handler, req(t, "GET", "/api/v1/notifications?n=500", "amy@example.com"))
	if rec.Code != 200 {
		t.Fatalf("n cap = %d", rec.Code)
	}
	// After-unknown + state filter exercises the fallback filter loop.
	x.addProfile("amy@example.com", "bob@example.com")
	x.writeThread(t, "acme", "repo", 7, "T", "amy@example.com")
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", []string{})
	got, _ := x.svc.Tray(ctx(), "amy@example.com", StateUnread, "bogus-cursor", 50)
	if len(got) != 1 {
		t.Fatalf("fallback filter = %d", len(got))
	}
}

func TestHandleRepoErrorBranches(t *testing.T) {
	x := newHarness(t)
	x.roles.grant("acme", "repo", "amy@example.com", "admin")
	// ListHooks error → 500.
	svc2 := New(failListStore{store.NewMemory()}, x.roles)
	h2 := &Handler{Svc: svc2}
	h2.Auth = x.handler.Auth
	rec := do(h2, req(t, "GET", "/acme/repo/api/webhooks", "amy@example.com"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("list err = %d", rec.Code)
	}
	// Create with bad URL → 400 through the handler.
	r := httptest.NewRequest("POST", "/acme/repo/api/webhooks", strings.NewReader(`{"url":"http://example.com/h"}`))
	r.Header.Set("X-Test-Principal", "amy@example.com")
	if rec := do(x.handler, r); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad url = %d", rec.Code)
	}
	// PATCH with unreadable body → 400.
	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{URL: strPtr("https://example.com/h")})
	if err != nil {
		t.Fatal(err)
	}
	pr := httptest.NewRequest("PATCH", "/acme/repo/api/webhooks/"+hk.ID, nil)
	pr.Body = errReader{}
	pr.Header.Set("X-Test-Principal", "amy@example.com")
	if rec := do(x.handler, pr); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad patch body = %d", rec.Code)
	}
	// SetWatch store error → 4xx/5xx through the handler (corrupt social
	// is invalid → 400).
	seedRepo(t, x, "acme", "repo2")
	writeRaw(t, x.svc.Store, SocialKey("acme", "repo2"), []byte("{bad"))
	rec = do(x.handler, req(t, "PUT", "/acme/repo2/api/watch", "amy@example.com"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("watch err = %d", rec.Code)
	}
	// Auth-chain error on a repo route maps through writeErr.
	h3 := &Handler{Svc: x.svc}
	h3.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrForbidden, Why: "nope"}
	}
	if rec := do(h3, req(t, "GET", "/acme/repo/api/watch", "")); rec.Code != http.StatusForbidden {
		t.Fatalf("repo auth chain = %d", rec.Code)
	}
}

func TestStreamEventWriteFailure(t *testing.T) {
	x := newHarness(t)
	// Writer that accepts the opener then fails: the frame write fails
	// and the stream returns.
	w := &failAfterN{rec: httptest.NewRecorder(), allow: 1}
	r := httptest.NewRequest("GET", "/api/v1/notifications/stream", nil)
	r.Header.Set("X-Test-Principal", "amy@example.com")
	done := make(chan struct{})
	go func() {
		defer close(done)
		x.handler.ServeHTTP(w, r)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for x.svc.ubus.liveCount("amy@example.com") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("never subscribed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	x.svc.ubus.publish("amy@example.com", Notification{ID: strings.Repeat("a", 32)})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("failed write never ended the stream")
	}
}

type failAfterN struct {
	rec   *httptest.ResponseRecorder
	allow int
	n     int
}

func (w *failAfterN) Header() http.Header { return w.rec.Header() }
func (w *failAfterN) Write(b []byte) (int, error) {
	w.n++
	if w.n > w.allow {
		return 0, errWriteBoom
	}
	return w.rec.Write(b)
}
func (w *failAfterN) WriteHeader(c int) { w.rec.WriteHeader(c) }
func (w *failAfterN) Flush()            {}

var errWriteBoom = errString("write boom")

type errString string

func (e errString) Error() string { return string(e) }

func TestCommentWriteFailure(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	w, ok := newSSEWriter(failWriter{header: http.Header{}}, r)
	if !ok {
		t.Fatal("must construct")
	}
	if w.comment(": test") {
		t.Fatal("comment on failing writer must stop")
	}
}

func TestRepoRingTrimAndFill(t *testing.T) {
	x := newHarness(t)
	for i := 0; i < RepoRing+6; i++ {
		x.svc.PublishStream("issue", "acme/repo", "", "", "", i)
	}
	_, recent, unsub := x.svc.SubscribeRepo("acme/repo")
	defer unsub()
	if len(recent) != RepoRing || recent[0].Num != 6 {
		t.Fatalf("ring = %d/%+v", len(recent), recent[0])
	}
	// Fill a live subscriber's buffer without reading: drop-oldest sheds.
	ch, _, unsub2 := x.svc.SubscribeRepo("acme/repo2")
	defer unsub2()
	for i := 0; i < 40; i++ {
		x.svc.PublishStream("issue", "acme/repo2", "", "", "", i)
	}
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			goto drained
		}
	}
drained:
	if n != 16 {
		t.Fatalf("live buffer = %d", n)
	}
	// Unsubscribe with a peer still attached keeps the repo set.
	_, _, unsub3 := x.svc.SubscribeRepo("acme/repo2")
	unsub3()
}

func TestRepoRingEvictedOnLastUnsubscribe(t *testing.T) {
	x := newHarness(t)
	const repos = 10
	unsubs := make([]func(), 0, repos)
	for i := 0; i < repos; i++ {
		repo := fmt.Sprintf("acme/evict%d", i)
		x.svc.PublishStream("issue", repo, "", "", "", i)
		_, _, unsub := x.svc.SubscribeRepo(repo)
		unsubs = append(unsubs, unsub)
	}
	if got := x.svc.rbus.ringCount(); got != repos {
		t.Fatalf("ringCount = %d, want %d", got, repos)
	}
	for _, unsub := range unsubs {
		unsub()
	}
	if got := x.svc.rbus.ringCount(); got != 0 {
		t.Fatalf("ringCount after last unsubscribe = %d, want 0", got)
	}
	// Late subscriber after a fully idle period starts from the live tail.
	_, recent, unsub := x.svc.SubscribeRepo("acme/evict0")
	defer unsub()
	if len(recent) != 0 {
		t.Fatalf("replay after eviction = %d frames, want 0", len(recent))
	}
}

func TestRepoRingLRUCap(t *testing.T) {
	x := newHarness(t)
	total := RepoBusMaxRepos + 5
	for i := 0; i < total; i++ {
		x.svc.PublishStream("issue", fmt.Sprintf("acme/cap%d", i), "", "", "", i)
	}
	if got := x.svc.rbus.ringCount(); got != RepoBusMaxRepos {
		t.Fatalf("ringCount = %d, want %d", got, RepoBusMaxRepos)
	}
	// Oldest idle repos are evicted first: no replay.
	_, recent, unsub := x.svc.SubscribeRepo("acme/cap0")
	defer unsub()
	if len(recent) != 0 {
		t.Fatalf("evicted replay = %d frames, want 0", len(recent))
	}
	// Newest repos are retained with their frames.
	last := fmt.Sprintf("acme/cap%d", total-1)
	_, recent, unsub2 := x.svc.SubscribeRepo(last)
	defer unsub2()
	if len(recent) != 1 || recent[0].Num != total-1 {
		t.Fatalf("retained replay = %+v, want one frame num %d", recent, total-1)
	}
}

func TestRepoRingEvictPrefersIdle(t *testing.T) {
	x := newHarness(t)
	_, _, unsub := x.svc.SubscribeRepo("acme/old")
	defer unsub()
	x.svc.PublishStream("issue", "acme/old", "", "", "", 1)
	for i := 0; i < RepoBusMaxRepos; i++ {
		x.svc.PublishStream("issue", fmt.Sprintf("acme/busy%d", i), "", "", "", i)
	}
	if got := x.svc.rbus.ringCount(); got != RepoBusMaxRepos {
		t.Fatalf("ringCount = %d, want %d", got, RepoBusMaxRepos)
	}
	// The subscribed repo keeps its ring while idle ones are evicted.
	_, recent, unsub2 := x.svc.SubscribeRepo("acme/old")
	defer unsub2()
	if len(recent) != 1 || recent[0].Num != 1 {
		t.Fatalf("subscribed replay = %+v, want one frame", recent)
	}
	_, recent, unsub3 := x.svc.SubscribeRepo("acme/busy0")
	defer unsub3()
	if len(recent) != 0 {
		t.Fatalf("oldest idle replay = %d frames, want 0", len(recent))
	}
}

func TestRepoRingEvictAllSubscribedKeepsLive(t *testing.T) {
	x := newHarness(t)
	unsubs := make([]func(), 0, RepoBusMaxRepos+1)
	for i := 0; i < RepoBusMaxRepos+1; i++ {
		repo := fmt.Sprintf("acme/live%d", i)
		_, _, unsub := x.svc.SubscribeRepo(repo)
		unsubs = append(unsubs, unsub)
		x.svc.PublishStream("issue", repo, "", "", "", i)
	}
	defer func() {
		for _, unsub := range unsubs {
			unsub()
		}
	}()
	if got := x.svc.rbus.ringCount(); got != RepoBusMaxRepos {
		t.Fatalf("ringCount = %d, want %d", got, RepoBusMaxRepos)
	}
	// Eviction drops only the replay ring: the LRU repo's channel stays open.
	if got := x.svc.repoLiveCount("acme/live0"); got != 1 {
		t.Fatalf("live0 subscribers = %d, want 1", got)
	}
	_, recent, unsub := x.svc.SubscribeRepo("acme/live0")
	defer unsub()
	if len(recent) != 0 {
		t.Fatalf("evicted replay = %d frames, want 0", len(recent))
	}
	// Live delivery on the evicted repo still works; same-goroutine publish
	// lands in the buffer before PublishFrame returns (deterministic).
	ch, _, _ := x.svc.SubscribeRepo("acme/live1")
	x.svc.PublishStream("issue", "acme/live1", "", "", "", 99)
	select {
	case f := <-ch:
		if f.Num != 99 {
			t.Fatalf("live frame num = %d, want 99", f.Num)
		}
	default:
		t.Fatal("live subscriber must receive despite ring eviction")
	}
}

func TestUserBusMultiSub(t *testing.T) {
	x := newHarness(t)
	_, unsub1 := x.svc.ubus.subscribe("amy@example.com")
	ch2, unsub2 := x.svc.ubus.subscribe("amy@example.com")
	defer unsub2()
	unsub1() // peer remains: no map delete, no close of ch2
	x.svc.ubus.publish("amy@example.com", Notification{ID: "x"})
	select {
	case <-ch2:
	default:
		t.Fatal("peer must still receive")
	}
}

func TestHookCreateFullSpecAndWildcard(t *testing.T) {
	x := newHarness(t)
	inactive := false
	insecure := true
	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "a", HookSpec{
		URL: strPtr("https://example.com/h"), Events: []string{"*"},
		Secret: strPtr("s"), Active: &inactive, InsecureTLS: &insecure,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hk.Active || !hk.InsecureTLS || hk.Secret != "s" || len(hk.Events) != 1 {
		t.Fatalf("full spec = %+v", hk)
	}
	// Wildcard matches everything, including ping.
	if !hookMatches(hk.Events, "commented") || !hookMatches(hk.Events, "ping") {
		t.Fatal("wildcard must match all")
	}
}

func TestHookURLHostAndIPShapes(t *testing.T) {
	x := newHarness(t)
	if _, err := x.svc.CreateHook(ctx(), "acme", "repo", "a", HookSpec{URL: strPtr("https://")}); err == nil {
		t.Fatal("hostless URL must fail")
	}
	if _, err := x.svc.CreateHook(ctx(), "acme", "repo", "a", HookSpec{URL: strPtr("http://127.0.0.2:9/h")}); err != nil {
		t.Fatalf("loopback IP literal must pass: %v", err)
	}
	if _, err := x.svc.CreateHook(ctx(), "acme", "repo", "a", HookSpec{URL: strPtr("http://93.184.216.34/h")}); err == nil {
		t.Fatal("non-loopback IP http must fail")
	}
	if isLoopbackHost("example.com") {
		t.Fatal("public host is not loopback")
	}
}

func TestListHooksGetErrorSkips(t *testing.T) {
	st := store.NewMemory()
	writeRaw(t, st, HookKey("acme", "repo", strings.Repeat("k", 24)),
		encode(&Hook{ID: strings.Repeat("k", 24), URL: "https://example.com/h"}))
	svc := New(failGetStore{st}, nil)
	hooks, err := svc.ListHooks(ctx(), "acme", "repo")
	if err != nil || len(hooks) != 0 {
		t.Fatalf("get-error entries skip: %+v, %v", hooks, err)
	}
}

func TestPatchHookFull(t *testing.T) {
	x := newHarness(t)
	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "a", HookSpec{URL: strPtr("https://example.com/h")})
	if err != nil {
		t.Fatal(err)
	}
	insecure := true
	active := false
	got, err := x.svc.PatchHook(ctx(), "acme", "repo", hk.ID, HookSpec{
		URL: strPtr("https://example.com/r"), Events: []string{"ping"},
		Secret: strPtr("n"), Active: &active, InsecureTLS: &insecure,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://example.com/r" || got.Secret != "n" || got.Active || !got.InsecureTLS || got.Version != 2 {
		t.Fatalf("full patch = %+v", got)
	}
}

func TestDeleteHookStoreError(t *testing.T) {
	st := store.NewMemory()
	writeRaw(t, st, HookKey("acme", "repo", "h"), encode(&Hook{ID: "h"}))
	svc := New(failDeleteStore{st}, nil)
	if err := svc.DeleteHook(ctx(), "acme", "repo", "h"); err == nil {
		t.Fatal("delete error must surface")
	}
}

func TestDeliverRepoEdges(t *testing.T) {
	x := newHarness(t)
	svc2 := New(failListStore{store.NewMemory()}, nil)
	svc2.DeliverRepo(ctx(), "acme", "repo") // list error → no-op
	// Inactive hooks are skipped by the pass.
	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "a", HookSpec{URL: strPtr("https://example.com/h")})
	if err != nil {
		t.Fatal(err)
	}
	inactive := false
	if _, err := x.svc.PatchHook(ctx(), "acme", "repo", hk.ID, HookSpec{Active: &inactive}); err != nil {
		t.Fatal(err)
	}
	x.svc.DeliverRepo(ctx(), "acme", "repo") // no posts, no crash
	// Canceled pass exits its workers.
	cctx, cancel := context.WithCancel(ctx())
	cancel()
	x.svc.DeliverRepo(cctx, "acme", "repo")
}

func TestGapDeliveryWithLiveSink(t *testing.T) {
	x := newHarness(t)
	sink, srv := newSink(t, "")
	defer srv.Close()
	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "a", HookSpec{URL: strPtr(srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	// Honest gap at seq 2: the loop counts it and continues from 3.
	for _, seq := range []int{1, 3} {
		ev := ActivityEvent{Seq: seq, Repo: "acme/repo", Action: "commented", Kind: "issue", At: x.now.Format(dateTimeFmt)}
		writeRaw(t, x.svc.Store, ActivityKey("acme", "repo", seq), encode(ev))
	}
	x.svc.DeliverRepo(ctx(), "acme", "repo")
	if got := sink.count(); got != 2 {
		t.Fatalf("gap delivery posts = %d", got)
	}
	if cur := x.svc.readCursor(ctx(), "acme", "repo", hk.ID); cur != 3 {
		t.Fatalf("cursor past gap = %d", cur)
	}
}

func TestReadDeliveriesEmptyObject(t *testing.T) {
	x := newHarness(t)
	writeRaw(t, x.svc.Store, DeliveriesKey("acme", "repo", "h"), []byte(`{}`))
	if d := x.svc.ReadDeliveries(ctx(), "acme", "repo", "h"); d.Entries == nil {
		t.Fatal("{} ring must be [], never null")
	}
}

func TestPingStoreErrors(t *testing.T) {
	svc2 := New(errStore{store.NewMemory(), errWriteBoom}, nil)
	if _, err := svc2.PingHook(ctx(), "acme", "repo", "h", "a"); err == nil {
		t.Fatal("reserve error must surface")
	}
	st := store.NewMemory()
	writeRaw(t, st, CollabStateKey("acme", "repo"), encode(CollabState{}))
	writeRaw(t, st, HookKey("acme", "repo", strings.Repeat("p", 24)),
		encode(&Hook{ID: strings.Repeat("p", 24), URL: "http://127.0.0.1:9/h", Active: true}))
	svc3 := New(failCreateStore{st}, nil)
	if _, err := svc3.PingHook(ctx(), "acme", "repo", strings.Repeat("p", 24), "a"); err == nil {
		t.Fatal("ping append error must surface")
	}
}

func TestSetWatchUnwatchCorrupt(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "acme", "repo")
	writeRaw(t, x.svc.Store, SocialKey("acme", "repo"), []byte("{bad"))
	if _, err := x.svc.SetWatch(ctx(), "amy@example.com", "acme", "repo", false); err == nil {
		t.Fatal("corrupt social on unwatch must fail")
	}
}

func TestSocialTruncation(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "acme", "repo")
	// Pre-fill past the cap, then watch: the list trims and flags.
	members := make([]string, 0, MaxWatchers+1)
	for i := 0; i < MaxWatchers+1; i++ {
		members = append(members, fmt.Sprintf("w%06d@example.com", i))
	}
	writeRaw(t, x.svc.Store, SocialKey("acme", "repo"), encode(SocialDoc{Watchers: MaxWatchers + 1, WatcherList: members}))
	st, err := x.svc.SetWatch(ctx(), "newuser@example.com", "acme", "repo", true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Watchers != MaxWatchers {
		t.Fatalf("capped count = %d", st.Watchers)
	}
	raw, _, _ := store.GetBytes(ctx(), x.svc.Store, SocialKey("acme", "repo"), store.GetOptions{})
	var soc SocialDoc
	_ = json.Unmarshal(raw, &soc)
	if !soc.WatchersTruncated || len(soc.WatcherList) != MaxWatchers {
		t.Fatalf("truncated = %+v", soc.WatchersTruncated)
	}
	// Unwatching a member shrinks the list and clears the flag.
	st, err = x.svc.SetWatch(ctx(), members[0], "acme", "repo", false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Watchers != MaxWatchers-1 {
		t.Fatalf("unwatch count = %d", st.Watchers)
	}
}

func TestUnreadCountFloor(t *testing.T) {
	x := newHarness(t)
	// Count 0 with an unread entry + flip to read: delta floors at 0.
	writeRaw(t, x.svc.Store, NotifIndexKey("amy@example.com"), encode(IndexDoc{
		Version: 1, UnreadCount: 0,
		Entries: []IndexEntry{{ID: strings.Repeat("f", 32), State: StateUnread}},
	}))
	if err := x.svc.indexFlip(ctx(), "amy@example.com", strings.Repeat("f", 32), StateRead); err != nil {
		t.Fatal(err)
	}
	if got := x.svc.UnreadCount(ctx(), "amy@example.com"); got != 0 {
		t.Fatalf("floor = %d", got)
	}
}

func TestRetainUserNothingToWrite(t *testing.T) {
	x := newHarness(t)
	// Stale sweep stamp, consistent count, nothing old: early return.
	old := x.now.AddDate(0, 0, -2).Format(dateTimeFmt)
	writeRaw(t, x.svc.Store, NotifIndexKey("amy@example.com"), encode(IndexDoc{
		Version: 1, UnreadCount: 1, SweptAt: old,
		Entries: []IndexEntry{{ID: strings.Repeat("a", 32), State: StateUnread, At: x.now.Format(dateTimeFmt)}},
	}))
	x.svc.retainUser(ctx(), "amy@example.com", x.now, x.now.AddDate(0, 0, -30).Format(dateTimeFmt))
	raw, _, _ := store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("amy@example.com"), store.GetOptions{})
	var ix IndexDoc
	_ = json.Unmarshal(raw, &ix)
	if ix.SweptAt != old || len(ix.Entries) != 1 {
		t.Fatalf("consistent pass is a no-op write: %+v", ix)
	}
}

func TestRetainRepoScanBranches(t *testing.T) {
	x := newHarness(t)
	// Empty repo: no state → return.
	x.svc.retainRepoEvents(ctx(), "acme", "empty", x.now)
	// Inactive hook: skipped in the min-cursor computation.
	hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "a", HookSpec{URL: strPtr("https://example.com/h")})
	if err != nil {
		t.Fatal(err)
	}
	inactive := false
	if _, err := x.svc.PatchHook(ctx(), "acme", "repo", hk.ID, HookSpec{Active: &inactive}); err != nil {
		t.Fatal(err)
	}
	oldAt := x.now.AddDate(0, 0, -30).Format(dateTimeFmt)
	newAt := x.now.Format(dateTimeFmt)
	for seq, at := range map[int]string{1: oldAt, 2: newAt} {
		ev := ActivityEvent{Seq: seq, Repo: "acme/repo", Action: "commented", At: at}
		writeRaw(t, x.svc.Store, ActivityKey("acme", "repo", seq), encode(ev))
	}
	if _, err := x.svc.casUpdate(ctx(), CollabStateKey("acme", "repo"), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		return encode(CollabState{NextSeq: 5}), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Hookless (only inactive): floor is the head; seq 1 (old) deletes,
	// seq 2 (new) stops the scan.
	x.svc.retainRepoEvents(ctx(), "acme", "repo", x.now)
	if x.svc.readActivity(ctx(), "acme", "repo", 1) != nil {
		t.Fatal("old seq below head must compact")
	}
	if x.svc.readActivity(ctx(), "acme", "repo", 2) == nil {
		t.Fatal("new seq stops the scan and survives")
	}
	// Gap in the scan window: never delete what we cannot see.
	if _, err := x.svc.casUpdate(ctx(), CollabStateKey("acme", "repo2"), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		return encode(CollabState{NextSeq: 6}), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	ev := ActivityEvent{Seq: 3, Repo: "acme/repo2", Action: "commented", At: oldAt}
	writeRaw(t, x.svc.Store, ActivityKey("acme", "repo2", 3), encode(ev))
	x.svc.retainRepoEvents(ctx(), "acme", "repo2", x.now)
	if x.svc.readActivity(ctx(), "acme", "repo2", 3) != nil {
		t.Fatal("lone old event below head must compact")
	}
}

func TestCollabHeadEdges(t *testing.T) {
	x := newHarness(t)
	if got := x.svc.collabHead(ctx(), "acme", "missing"); got != 0 {
		t.Fatalf("absent = %d", got)
	}
	writeRaw(t, x.svc.Store, CollabStateKey("acme", "repo"), []byte("{bad"))
	if got := x.svc.collabHead(ctx(), "acme", "repo"); got != 0 {
		t.Fatalf("corrupt = %d", got)
	}
	svc2 := New(errStore{store.NewMemory(), errWriteBoom}, nil)
	if got := svc2.collabHead(ctx(), "a", "r"); got != 0 {
		t.Fatalf("err = %d", got)
	}
}

func TestRetainUserStoreError(t *testing.T) {
	svc := New(errStore{store.NewMemory(), errWriteBoom}, nil)
	svc.retainUser(ctx(), "amy@example.com", time.Now(), "") // getJSON fails → return
}

func TestFanoutOneCanceled(t *testing.T) {
	x := newHarness(t)
	ev := ActivityEvent{Seq: 7, Repo: "acme/repo",
		Payload: encode(activityPayload{Recipients: []activityRecipient{{Principal: "a@example.com", Reason: ReasonSubscribed}}})}
	writeRaw(t, x.svc.Store, ActivityKey("acme", "repo", 7), encode(ev))
	cctx, cancel := context.WithCancel(ctx())
	cancel()
	x.svc.fanoutOne(cctx, "acme", "repo", "acme/repo", 7) // precheck exits
}
