package notify

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

// failStore fault-injects Put failures for selected keys (transient
// store errors — plain errors, never 412s, so every writer surfaces
// them as real failures). Reads pass through untouched.
type failStore struct {
	store.ObjectStore
	fail func(key string) bool
}

func (f *failStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if f.fail != nil && f.fail(key) {
		return store.ObjectMeta{}, fmt.Errorf("injected store failure for %s", key)
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

// captureLog wires a text logger on svc and returns the sink.
func captureLog(svc *Service) *bytes.Buffer {
	var buf bytes.Buffer
	svc.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	return &buf
}

// TestEmitReserveFailureLogsNoFanout (issue #92): a reserveSeq failure
// must log the drop and arm nothing — no notifications, no activity,
// no phantom fanout task.
func TestEmitReserveFailureLogsNoFanout(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com", "carol@example.com")
	x.writeThread(t, "acme", "repo", 7, "Bug title", "amy@example.com")
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, fail: func(key string) bool {
		return strings.Contains(key, "collab_state")
	}}
	buf := captureLog(x.svc)

	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", []string{"carol@example.com"})

	if got := buf.String(); !strings.Contains(got, "seq reserve failed") {
		t.Fatalf("reserve failure not logged: %q", got)
	}
	if got := countNotifs(t, x, "carol@example.com"); got != 0 {
		t.Fatalf("carol notifications = %d, want 0", got)
	}
	if ev := x.svc.readActivity(ctx(), "acme", "repo", 1); ev != nil {
		t.Fatalf("phantom activity event: %+v", ev)
	}
	if rec := x.svc.TaskStatus("acme/repo", TaskKindFanout); rec != nil {
		t.Fatalf("phantom fanout task armed: %+v", rec)
	}
}

// TestEmitOverflowAppendFailureLogsNoPhantomFanout (issue #92): an
// overflow emission whose activity append fails must log and must NOT
// arm a fanout for the nonexistent event.
func TestEmitOverflowAppendFailureLogsNoPhantomFanout(t *testing.T) {
	x := newHarness(t)
	recips := make([]string, 0, MaxSyncRecipients+5)
	names := []string{"amy@example.com", "bob@example.com"}
	for i := 0; i < MaxSyncRecipients+5; i++ {
		names = append(names, fmt.Sprintf("user%03d@example.com", i))
	}
	x.addProfile(names...)
	x.writeThread(t, "acme", "repo", 7, "T", "amy@example.com")
	for _, n := range names {
		if n != "amy@example.com" && n != "bob@example.com" {
			recips = append(recips, n)
		}
	}
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, fail: func(key string) bool {
		return strings.Contains(key, "collab-events/")
	}}
	buf := captureLog(x.svc)

	x.svc.EmitIssue(ctx(), "acme/repo", 7, "mentioned", "bob@example.com", "", "", recips)

	if got := buf.String(); !strings.Contains(got, "activity append failed") || !strings.Contains(got, "fanout not armed") {
		t.Fatalf("overflow append failure not logged: %q", got)
	}
	if ev := x.svc.readActivity(ctx(), "acme", "repo", 1); ev != nil {
		t.Fatalf("phantom activity event: %+v", ev)
	}
	if rec := x.svc.TaskStatus("acme/repo", TaskKindFanout); rec != nil {
		t.Fatalf("phantom fanout task armed for nonexistent event: %+v", rec)
	}
	if got := countNotifs(t, x, recips[0]); got != 0 {
		t.Fatalf("overflow creates nothing sync: %d", got)
	}
}

// TestEmitShortfallAppendFailureLogsNoPhantomFanout (issue #92): a
// sync-path shortfall whose backfill append also fails must log and
// must NOT arm a fanout for the nonexistent event.
func TestEmitShortfallAppendFailureLogsNoPhantomFanout(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com", "carol@example.com")
	x.writeThread(t, "acme", "repo", 7, "Bug title", "amy@example.com")
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, fail: func(key string) bool {
		if strings.Contains(key, "collab-events/") {
			return true
		}
		// Fail notification Creates (not the index CAS) to force the
		// shortfall path.
		return strings.Contains(key, "/notifications/") && !strings.HasSuffix(key, "index.json")
	}}
	buf := captureLog(x.svc)

	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", []string{"carol@example.com"})

	if got := buf.String(); !strings.Contains(got, "shortfall activity append failed") {
		t.Fatalf("shortfall append failure not logged: %q", got)
	}
	if ev := x.svc.readActivity(ctx(), "acme", "repo", 1); ev != nil {
		t.Fatalf("phantom activity event: %+v", ev)
	}
	if rec := x.svc.TaskStatus("acme/repo", TaskKindFanout); rec != nil {
		t.Fatalf("phantom fanout task armed for nonexistent event: %+v", rec)
	}
}

// TestEmitSyncAppendFailureLogsKeepsTray (issue #92): a sync-complete
// emission whose activity append fails keeps its tray entries, logs
// the lost webhook/backfill event, and arms no fanout.
func TestEmitSyncAppendFailureLogsKeepsTray(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com", "carol@example.com")
	x.writeThread(t, "acme", "repo", 7, "Bug title", "amy@example.com")
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, fail: func(key string) bool {
		return strings.Contains(key, "collab-events/")
	}}
	buf := captureLog(x.svc)

	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", []string{"carol@example.com"})

	if got := buf.String(); !strings.Contains(got, "activity append failed") {
		t.Fatalf("sync append failure not logged: %q", got)
	}
	if got := countNotifs(t, x, "carol@example.com"); got != 1 {
		t.Fatalf("tray entry must survive append failure: %d", got)
	}
	if ev := x.svc.readActivity(ctx(), "acme", "repo", 1); ev != nil {
		t.Fatalf("phantom activity event: %+v", ev)
	}
	if rec := x.svc.TaskStatus("acme/repo", TaskKindFanout); rec != nil {
		t.Fatalf("phantom fanout task armed for nonexistent event: %+v", rec)
	}
}

// TestEmitLogDiscardsWhenUnwired: the nil-Logger path (no test or
// composition wiring) must not panic on any drop path.
func TestEmitLogDiscardsWhenUnwired(t *testing.T) {
	x := newHarness(t)
	x.addProfile("amy@example.com", "bob@example.com", "carol@example.com")
	x.writeThread(t, "acme", "repo", 7, "Bug title", "amy@example.com")
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, fail: func(key string) bool {
		return strings.Contains(key, "collab_state") || strings.Contains(key, "collab-events/")
	}}
	x.svc.Logger = nil
	x.svc.EmitIssue(ctx(), "acme/repo", 7, "subscribed", "bob@example.com", "", "commented", []string{"carol@example.com"})
}
