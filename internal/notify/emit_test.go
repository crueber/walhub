package notify

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

func TestNotificationID(t *testing.T) {
	a := NotificationID("amy@example.com", "acme/repo", 7, "subscribed", 3)
	b := NotificationID("amy@example.com", "acme/repo", 7, "subscribed", 3)
	if a != b || len(a) != 32 {
		t.Fatalf("id not deterministic/32-hex: %q", a)
	}
	for _, c := range a {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("id not lowercase hex: %q", a)
		}
	}
	if c := NotificationID("amy@example.com", "acme/repo", 7, "mentioned", 3); c == a {
		t.Fatal("reason must discriminate ids")
	}
	if c := NotificationID("zed@example.com", "acme/repo", 7, "subscribed", 3); c == a {
		t.Fatal("principal must discriminate ids")
	}
	if c := NotificationID("amy@example.com", "acme/repo", 7, "subscribed", 4); c == a {
		t.Fatal("event seq must discriminate ids")
	}
}

// emitComment seeds a thread and emits a subscribed-class emission like an
// issue comment would.
func emitComment(t *testing.T, x *harness, actor string, recips []string) {
	t.Helper()
	x.writeThread(t, "acme", "repo", 7, "Bug title", "amy@example.com")
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", actor, "", "commented", recips)
}

func readNotif(t *testing.T, x *harness, principal, id string) *Notification {
	t.Helper()
	raw, _, err := store.GetBytes(ctx(), x.svc.Store, NotifKey(principal, id), store.GetOptions{})
	if err != nil {
		t.Fatalf("read notification: %v", err)
	}
	var n Notification
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatal(err)
	}
	return &n
}

func countNotifs(t *testing.T, x *harness, principal string) int {
	t.Helper()
	n := 0
	_ = x.svc.Store.List(ctx(), NotifPrefix(principal), "", func(m store.ObjectMeta) error {
		if !strings.HasSuffix(m.Key, "/index.json") {
			n++
		}
		return nil
	})
	return n
}

func TestEmitSubscribedFansOut(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com", "carol@example.com")
	// Thread author amy, primary recipient carol, actor bob: carol gets
	// subscribed, amy gets the author reason, bob gets nothing.
	x.writeThread(t, "acme", "repo", 7, "Bug title", "amy@example.com")
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", []string{"carol@example.com"})

	if got := countNotifs(t, x, "carol@example.com"); got != 1 {
		t.Fatalf("carol notifications = %d, want 1", got)
	}
	if got := countNotifs(t, x, "amy@example.com"); got != 1 {
		t.Fatalf("author notifications = %d, want 1", got)
	}
	if got := countNotifs(t, x, "bob@example.com"); got != 0 {
		t.Fatalf("actor self-notified: %d", got)
	}
	// Index carries the tray entry + count.
	raw, _, err := store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("carol@example.com"), store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var ix IndexDoc
	if err := json.Unmarshal(raw, &ix); err != nil {
		t.Fatal(err)
	}
	if ix.UnreadCount != 1 || len(ix.Entries) != 1 {
		t.Fatalf("index = %+v", ix)
	}
	en := ix.Entries[0]
	if en.Reason != ReasonSubscribed || en.Title != "Bug title" || en.State != StateUnread {
		t.Fatalf("entry = %+v", en)
	}
	n := readNotif(t, x, "carol@example.com", en.ID)
	if n.Repo != "acme/repo" || n.Num != 7 || n.Kind != "issue" || n.Actor != "bob@example.com" || n.CreatedAt == "" {
		t.Fatalf("notification = %+v", n)
	}
	// Activity log appended (seq 1) with the recipient payload.
	ev := x.svc.readActivity(ctx(), "acme", "repo", 1)
	if ev == nil || ev.Action != "commented" || ev.Num != 7 {
		t.Fatalf("activity = %+v", ev)
	}
	var payload activityPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Class != "subscribed" || len(payload.Recipients) != 2 {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestEmitDedupsUnreadSameThreadReason(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com", "zed@example.com", "carol@example.com")
	x.writeThread(t, "acme", "repo", 7, "Bug title", "amy@example.com")
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", []string{"carol@example.com"})
	// Second event, same (user, thread, reason), first still unread → skip.
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "zed@example.com", "", "commented", []string{"carol@example.com"})
	if got := countNotifs(t, x, "carol@example.com"); got != 1 {
		t.Fatalf("dedup failed: %d notifications", got)
	}
	// After marking read, a new event creates a new notification.
	raw, _, _ := store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("carol@example.com"), store.GetOptions{})
	var ix IndexDoc
	_ = json.Unmarshal(raw, &ix)
	if _, err := x.svc.SetState(ctx(), "carol@example.com", ix.Entries[0].ID, true); err != nil {
		t.Fatal(err)
	}
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "zed@example.com", "", "commented", []string{"carol@example.com"})
	if got := countNotifs(t, x, "carol@example.com"); got != 2 {
		t.Fatalf("read must not block new: %d", got)
	}
}

func TestEmitMentionValidationAndAuthor(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com", "carol@example.com")
	x.writeThread(t, "acme", "repo", 7, "Bug title", "amy@example.com")
	// Mentioned: carol (valid) + ghost (no profile → dropped); author amy
	// is not the actor, but mentioned-class adds no author reason.
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "mentioned", "bob@example.com", "", "", []string{"carol@example.com", "ghost@example.com"})
	if got := countNotifs(t, x, "carol@example.com"); got != 1 {
		t.Fatalf("carol = %d", got)
	}
	if got := countNotifs(t, x, "ghost@example.com"); got != 0 {
		t.Fatalf("ghost must be dropped")
	}
	raw, _, _ := store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("carol@example.com"), store.GetOptions{})
	var ix IndexDoc
	_ = json.Unmarshal(raw, &ix)
	if ix.Entries[0].Reason != ReasonMentioned {
		t.Fatalf("reason = %q", ix.Entries[0].Reason)
	}
	// Subscribed-class: author amy gets the author reason (not subscribed).
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", []string{})
	raw, _, _ = store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("amy@example.com"), store.GetOptions{})
	ix = IndexDoc{}
	_ = json.Unmarshal(raw, &ix)
	if len(ix.Entries) != 1 || ix.Entries[0].Reason != ReasonAuthor {
		t.Fatalf("author entry = %+v", ix.Entries)
	}
}

func TestEmitTeamExpansion(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com", "m1@example.com", "m2@example.com")
	x.teams.members["acme/backend"] = []string{"m2@example.com", "m1@example.com", "ghost@example.com"}
	x.writeThread(t, "acme", "repo", 7, "T", "amy@example.com")
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "mentioned", "bob@example.com", "", "", []string{"acme/backend"})
	for _, m := range []string{"m1@example.com", "m2@example.com"} {
		if got := countNotifs(t, x, m); got != 1 {
			t.Fatalf("%s = %d", m, got)
		}
		raw, _, _ := store.GetBytes(ctx(), x.svc.Store, NotifIndexKey(m), store.GetOptions{})
		var ix IndexDoc
		_ = json.Unmarshal(raw, &ix)
		if ix.Entries[0].Reason != ReasonTeamMention {
			t.Fatalf("%s reason = %q", m, ix.Entries[0].Reason)
		}
	}
	if got := countNotifs(t, x, "ghost@example.com"); got != 0 {
		t.Fatal("unknown team member must be dropped")
	}
	// Unknown team → silently ignored, no crash, no activity recipients.
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "mentioned", "bob@example.com", "", "", []string{"acme/nonexistent"})
	ev := x.svc.readActivity(ctx(), "acme", "repo", 2)
	var payload activityPayload
	_ = json.Unmarshal(ev.Payload, &payload)
	if len(payload.Recipients) != 0 {
		t.Fatalf("unknown team recipients = %+v", payload.Recipients)
	}
}

func TestEmitWatchersUnion(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com", "watcher@example.com")
	x.writeThread(t, "acme", "repo", 7, "T", "amy@example.com")
	if _, err := x.svc.SetWatch(ctx(), "watcher@example.com", "acme", "repo", true); err != nil {
		t.Fatal(err)
	}
	emitComment(t, x, "bob@example.com", []string{})
	if got := countNotifs(t, x, "watcher@example.com"); got != 1 {
		t.Fatalf("watcher = %d", got)
	}
	// Watchers do NOT join mentioned-class emissions (those carry their
	// own recipients; the subscribed emission covers watchers).
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "mentioned", "bob@example.com", "", "", []string{"amy@example.com"})
	if got := countNotifs(t, x, "watcher@example.com"); got != 1 {
		t.Fatalf("watcher must not join mentioned: %d", got)
	}
}

func TestEmitIdempotentRetry(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com")
	emitComment(t, x, "bob@example.com", []string{"amy@example.com"})
	// Same activity seq replayed (crash between CAS and fan-out): the
	// deterministic Create 412s and the index CAS skips the present id.
	raw, _, _ := store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("amy@example.com"), store.GetOptions{})
	var ix IndexDoc
	_ = json.Unmarshal(raw, &ix)
	id := ix.Entries[0].ID
	dup := Notification{ID: id, Repo: "acme/repo", Num: 7, Kind: "issue", Reason: ReasonSubscribed}
	if err := x.svc.putCreate(ctx(), NotifKey("amy@example.com", id), encode(dup)); err == nil {
		t.Fatal("replay Create must 412")
	} else if !isPrecondition(err) {
		t.Fatalf("replay err = %v", err)
	}
	if err := x.svc.indexAdd(ctx(), "amy@example.com", IndexEntry{
		ID: id, Repo: "acme/repo", Num: 7, Kind: "issue",
		Reason: ReasonAuthor, Title: "Bug title", State: StateUnread, At: ix.Entries[0].At,
	}); err != nil {
		t.Fatal(err)
	}
	raw2, _, _ := store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("amy@example.com"), store.GetOptions{})
	var ix2 IndexDoc
	_ = json.Unmarshal(raw2, &ix2)
	if ix2.UnreadCount != 1 || len(ix2.Entries) != 1 {
		t.Fatalf("replay changed index: %+v", ix2)
	}
}

func isPrecondition(err error) bool {
	return err != nil && strings.Contains(err.Error(), "recondition")
}

func TestEmitAssignedAndReviewRequested(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com", "carol@example.com", "rv@example.com")
	x.writeThread(t, "acme", "repo", 7, "T", "amy@example.com")
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "assigned", "bob@example.com", "", "", []string{"carol@example.com"})
	x.svc.EmitReview(ctx(), "acme/repo", 7, "review_requested", "bob@example.com", "", []string{"rv@example.com"})
	for principal, reason := range map[string]string{"carol@example.com": ReasonAssigned, "rv@example.com": ReasonReviewRequested} {
		raw, _, _ := store.GetBytes(ctx(), x.svc.Store, NotifIndexKey(principal), store.GetOptions{})
		var ix IndexDoc
		_ = json.Unmarshal(raw, &ix)
		if len(ix.Entries) != 1 || ix.Entries[0].Reason != reason {
			t.Fatalf("%s = %+v", principal, ix.Entries)
		}
	}
}

func TestEmitCheckResolvesParticipants(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "ci@example.com", "bob@example.com")
	// Checks carry no recipients: participants resolve from thread.json.
	// Thread author amy + participant bob, actor ci.
	raw, _ := json.Marshal(map[string]any{
		"title": "PR title", "author": "amy@example.com",
		"participants": []string{"amy@example.com", "bob@example.com"},
	})
	if _, err := store.PutBytes(ctx(), x.svc.Store, threadKey("acme", "repo", 9), raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	x.svc.EmitCheck(ctx(), "acme/repo", fmt.Sprintf("%040d", 1), "ci/build", "failure", "broke", "", "ci@example.com", "", 9)
	raw, _, _ = store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("amy@example.com"), store.GetOptions{})
	var ix IndexDoc
	_ = json.Unmarshal(raw, &ix)
	if len(ix.Entries) != 1 || ix.Entries[0].Reason != ReasonAuthor || ix.Entries[0].Num != 9 || ix.Entries[0].Kind != "pull" {
		t.Fatalf("author entry = %+v", ix.Entries)
	}
	raw, _, _ = store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("bob@example.com"), store.GetOptions{})
	ix = IndexDoc{}
	_ = json.Unmarshal(raw, &ix)
	if len(ix.Entries) != 1 || ix.Entries[0].Reason != ReasonSubscribed {
		t.Fatalf("participant entry = %+v", ix.Entries)
	}
	ev := x.svc.readActivity(ctx(), "acme", "repo", 1)
	if ev.Action != ActionCheckReported {
		t.Fatalf("action = %q", ev.Action)
	}
	var payload activityPayload
	_ = json.Unmarshal(ev.Payload, &payload)
	if payload.Detail["state"] != "failure" || payload.Detail["context"] != "ci/build" {
		t.Fatalf("detail = %v", payload.Detail)
	}
}

func TestClassActionMapping(t *testing.T) {
	cases := map[string]string{
		"opened": "opened", "forked": "opened", "closed": "closed", "merged": "closed",
		"reopened": "reopened", "assigned": "assigned", "mentioned": "mentioned",
		"review_requested": "review_requested", "review_request_removed": "review_requested",
		"review_submitted": "review_posted", "review_dismissed": "review_posted",
		"check_reported": "check_reported", "release_published": "release_published",
		"subscribed": "commented", "thread_commented": "commented", "head_force_pushed": "commented",
	}
	for class, want := range cases {
		if got := classAction(class, ""); got != want {
			t.Errorf("classAction(%q) = %q, want %q", class, got, want)
		}
	}
	if got := classAction("subscribed", "opened"); got != "opened" {
		t.Errorf("override lost: %q", got)
	}
}
