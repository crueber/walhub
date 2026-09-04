package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

// Index projection (P4): newest-first upserts, paged reads, compaction,
// and the open-PR head lookup for §8 fan-out.

func TestIndexProjection(t *testing.T) {
	e := newTestEnv()
	sha1, sha2 := hexSHA(10), hexSHA(11)
	e.knowSHA(sha1)
	e.knowSHA(sha2)
	// Absent index ⇒ empty page.
	page, err := e.svc.ListChecks(ctx(), "o", "r", reader(), "", 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Checks == nil || len(page.Checks) != 0 || page.More {
		t.Fatalf("page = %+v", page)
	}
	// Two reports on two shas: newest first.
	e.mustReport(t, sha1, "ci", StateSuccess)
	e.mustReport(t, sha2, "ci", StatePending)
	page, _ = e.svc.ListChecks(ctx(), "o", "r", reader(), "", 50)
	if len(page.Checks) != 2 || page.Checks[0].SHA != sha2 || page.Checks[1].SHA != sha1 {
		t.Fatalf("order = %+v", page.Checks)
	}
	if page.Checks[0].State != StatePending || page.Checks[1].State != StateSuccess {
		t.Fatalf("states = %+v", page.Checks)
	}
	// A re-report moves the sha back to the front with the fresh state.
	e.mustReport(t, sha1, "ci", StateFailure)
	page, _ = e.svc.ListChecks(ctx(), "o", "r", reader(), "", 50)
	if len(page.Checks) != 2 || page.Checks[0].SHA != sha1 || page.Checks[0].State != StateFailure {
		t.Fatalf("refresh = %+v", page.Checks)
	}
	// Cursor paging.
	page, _ = e.svc.ListChecks(ctx(), "o", "r", reader(), sha1, 50)
	if len(page.Checks) != 1 || page.Checks[0].SHA != sha2 || page.More {
		t.Fatalf("cursor = %+v", page)
	}
	page, _ = e.svc.ListChecks(ctx(), "o", "r", reader(), sha2, 50)
	if len(page.Checks) != 0 || page.More {
		t.Fatalf("tail = %+v", page)
	}
	// Unknown cursor ⇒ from the top.
	page, _ = e.svc.ListChecks(ctx(), "o", "r", reader(), hexSHA(99), 50)
	if len(page.Checks) != 2 {
		t.Fatalf("unknown cursor = %+v", page)
	}
}

func TestCompactIndex(t *testing.T) {
	e := newTestEnv()
	// Small index: no compaction.
	sha := hexSHA(20)
	e.knowSHA(sha)
	e.mustReport(t, sha, "ci", StateSuccess)
	if compacted, err := e.svc.CompactIndex(ctx(), "o", "r"); err != nil || compacted {
		t.Fatalf("compacted=%v err=%v", compacted, err)
	}
	// Oversized index: compaction prunes to the hot window and the object
	// fits again. Build it directly (2000 rows ≈ 360 KiB).
	ix := &IndexDoc{SHAs: []IndexSHA{}}
	for i := 0; i < 2000; i++ {
		sha := fmt.Sprintf("%040d", 1000+i)
		ix.SHAs = append(ix.SHAs, IndexSHA{
			SHA: sha, State: StateSuccess,
			Contexts:  []IndexContext{{Name: "ci", State: StateSuccess, UpdatedAt: "2026-09-04T12:00:00Z"}},
			UpdatedAt: "2026-09-04T12:00:00Z",
		})
	}
	raw := encodeIndex(ix)
	if len(raw) <= IndexSizeLimit {
		t.Fatalf("fixture too small: %d", len(raw))
	}
	if _, err := store.PutBytes(ctx(), e.store, IndexKey("o", "r"), raw,
		store.PutOptions{Mode: store.PutOverwrite, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	compacted, err := e.svc.CompactIndex(ctx(), "o", "r")
	if err != nil || !compacted {
		t.Fatalf("compacted=%v err=%v", compacted, err)
	}
	got, err := e.svc.loadIndex(ctx(), "o", "r")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(got.SHAs) > IndexHotWindow {
		t.Fatalf("window: %d", len(got.SHAs))
	}
	if len(encodeIndex(got)) > IndexSizeLimit {
		t.Fatal("still oversized")
	}
	// Newest-first order preserved (the head survives).
	if got.SHAs[0].SHA != ix.SHAs[0].SHA {
		t.Fatal("newest row evicted")
	}
}

func TestNotifyHeads(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(30)
	other := hexSHA(31)
	e.knowSHA(sha)
	e.knowSHA(other)
	// One open PR (num 7) with head == sha; one closed PR (num 8) with
	// head == other. Shared-family keys per the documented layouts
	// (02 P3/P4, 03 §2.1).
	seedPR(t, e, 7, "open", false, sha)
	seedPR(t, e, 8, "closed", false, other)
	var emitted []NotifyEvent
	e.svc.Notify = func(_ context.Context, ev NotifyEvent) { emitted = append(emitted, ev) }
	var streamed []StreamEvent
	e.svc.Stream = func(_ context.Context, ev StreamEvent) { streamed = append(streamed, ev) }
	// Failure on the open-PR head enqueues exactly one check_reported.
	e.mustReport(t, sha, "ci/build", StateFailure)
	if len(emitted) != 1 {
		t.Fatalf("emitted = %+v", emitted)
	}
	got := emitted[0]
	if got.Class != NotifyClass || got.SHA != sha || got.Context != "ci/build" || got.State != StateFailure || got.PR != 7 {
		t.Fatalf("event = %+v", got)
	}
	if len(streamed) != 1 || streamed[0].Name != StreamName || streamed[0].CombinedState != StateFailure {
		t.Fatalf("stream = %+v", streamed)
	}
	// Re-reporting failure (no transition) emits nothing more.
	emitted = nil
	e.mustReport(t, sha, "ci/build", StateError)
	if len(emitted) != 0 {
		t.Fatalf("no-transition emit = %+v", emitted)
	}
	// Success transitions emit nothing.
	emitted = nil
	e.mustReport(t, sha, "ci/build", StateSuccess)
	if len(emitted) != 0 {
		t.Fatalf("success emit = %+v", emitted)
	}
	// Failure on a non-head sha emits nothing.
	emitted = nil
	e.mustReport(t, other, "ci/build", StateFailure)
	if len(emitted) != 0 {
		t.Fatalf("non-head emit = %+v", emitted)
	}
}

// seedPR writes the shared-family objects for one PR thread: the thread
// header (P3), the shared index card (P4), and the pr.json sidecar
// (03 §2.1).
func seedPR(t *testing.T, e *testEnv, num int, state string, merged bool, headSHA string) {
	t.Helper()
	put := func(key, body string) {
		t.Helper()
		if _, err := store.PutBytes(ctx(), e.store, key, []byte(body),
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	threadKey := fmt.Sprintf("repos/o/r/issues/%06x/thread.json", num)
	prKey := fmt.Sprintf("repos/o/r/pulls/%06x/pr.json", num)
	put(threadKey, `{"num":`+itoa(num)+`,"kind":"pr","title":"t","state":"`+state+`","author":"jane@example.com","created_at":"2026-09-04T12:00:00Z","updated_at":"2026-09-04T12:00:00Z","labels":[],"assignees":[],"participants":["jane@example.com"],"next_event_seq":1,"comment_count":0,"version":1}`)
	mergedJSON := "false"
	if merged {
		mergedJSON = "true"
	}
	put(prKey, `{"num":`+itoa(num)+`,"kind":"pr","base":{"repo":"o/r","ref":"refs/heads/main","sha":"`+hexSHA(1)+`"},"head":{"repo":"o/r","ref":"refs/heads/topic","sha":"`+headSHA+`"},"merged":`+mergedJSON+`,"version":1}`)
	// Shared index card (open only) — read-modify-write the test index.
	if state == "open" {
		var doc struct {
			Open []json.RawMessage `json:"open"`
		}
		if raw, _, _ := e.svc.getJSON(ctx(), "repos/o/r/issues/index.json"); raw != nil {
			_ = json.Unmarshal(raw, &doc)
		}
		doc.Open = append(doc.Open, json.RawMessage(`{"num":`+itoa(num)+`,"kind":"pr","title":"t","state":"open","labels":[],"assignees":[],"author":"jane@example.com","created_at":"2026-09-04T12:00:00Z","updated_at":"2026-09-04T12:00:00Z","comment_count":0}`))
		out, _ := json.Marshal(map[string]any{"open": doc.Open, "closed_recent": []any{}, "version": 1})
		_, _ = store.PutBytes(ctx(), e.store, "repos/o/r/issues/index.json", out,
			store.PutOptions{Mode: store.PutOverwrite, ContentType: "application/json"})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
