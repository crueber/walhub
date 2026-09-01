// notify_test.go — the notify body parser table (09 §6.1) and the handler's
// report array (queued | dropped | ignored), 400 on invalid JSON, and the 503
// sink-failure path.
package events

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func manifestKey(repo string) string { return "repos/" + repo + "/manifest.pb" }

func TestRepoFromKey(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want string
		ok   bool
	}{
		{"repos/acme/monorepo/manifest.pb", "acme/monorepo", true},
		{"buckets/walhub/repos/acme/monorepo/manifest.pb", "acme/monorepo", true},
		{"some/prefix/repos/a/b/manifest.pb", "a/b", true},
		{"repos/acme/monorepo/other.pb", "", false},
		{"repos/acme/manifest.pb", "", false},    // missing name
		{"acme/monorepo/manifest.pb", "", false}, // no repos segment
		{"repos/a/b/c/manifest.pb", "", false},   // too many segments
		{"", "", false},
		{"manifest.pb", "", false},
	} {
		got, ok := repoFromKey(tc.key)
		if got != tc.want || ok != tc.ok {
			t.Errorf("repoFromKey(%q) = (%q, %v), want (%q, %v)", tc.key, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseNotify(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []trigger
	}{
		{
			name: "gcs_finalize_manifest_key",
			body: `{"message":{"attributes":{"eventType":"OBJECT_FINALIZE","objectId":"` + manifestKey("acme/monorepo") + `","bucketId":"b"}}}`,
			want: []trigger{{repo: "acme/monorepo", key: manifestKey("acme/monorepo")}},
		},
		{
			name: "gcs_finalize_objectid_under_message",
			body: `{"message":{"attributes":{"eventType":"OBJECT_FINALIZE"},"objectId":"x/repos/acme/monorepo/manifest.pb"}}`,
			want: []trigger{{repo: "acme/monorepo", key: "x/repos/acme/monorepo/manifest.pb"}},
		},
		{
			name: "gcs_other_event_type_ignored",
			body: `{"message":{"attributes":{"eventType":"OBJECT_DELETE","objectId":"` + manifestKey("acme/monorepo") + `"}}}`,
			want: nil,
		},
		{
			name: "gcs_finalize_non_manifest_key",
			body: `{"message":{"attributes":{"eventType":"OBJECT_FINALIZE","objectId":"repos/acme/monorepo/wal/x.pack"}}}`,
			want: []trigger{{key: "repos/acme/monorepo/wal/x.pack"}},
		},
		{
			name: "s3_object_created",
			body: `{"Records":[{"eventName":"ObjectCreated:Put","s3":{"object":{"key":"` + manifestKey("a/b") + `"}}},
				{"eventName":"ObjectRemoved:Delete","s3":{"object":{"key":"` + manifestKey("c/d") + `"}}},
				{"eventName":"ObjectCreated:Put","s3":{"object":{"key":"repos/a/b/wal/p.pack"}}}]}`,
			want: []trigger{
				{repo: "a/b", key: manifestKey("a/b")},
				{key: "repos/a/b/wal/p.pack"},
			},
		},
		{
			name: "s3_no_records",
			body: `{"Records":[]}`,
			want: nil,
		},
		{
			name: "glue_key",
			body: `{"key":"` + manifestKey("o/r") + `"}`,
			want: []trigger{{repo: "o/r", key: manifestKey("o/r")}},
		},
		{
			name: "glue_repo",
			body: `{"repo":"acme/monorepo"}`,
			want: []trigger{{repo: "acme/monorepo"}},
		},
		{
			name: "glue_bad_repo",
			body: `{"repo":"nope"}`,
			want: nil,
		},
		{
			name: "empty_object",
			body: `{}`,
			want: nil,
		},
		{
			name: "unparseable_but_valid_json",
			body: `{"something":"else entirely"}`,
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseNotify([]byte(tc.body))
			if len(got) != len(tc.want) {
				t.Fatalf("parseNotify = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i].repo != tc.want[i].repo || got[i].key != tc.want[i].key {
					t.Errorf("trigger[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseNotify_InvalidJSON(t *testing.T) {
	if got := parseNotify([]byte("not json")); got != nil {
		t.Errorf("invalid JSON → %+v, want nil", got)
	}
}

func postNotify(t *testing.T, br *Bridge, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/_events/notify", strings.NewReader(body))
	w := httptest.NewRecorder()
	br.HandleNotify(w, req)
	return w
}

func notifyRepoFor(t *testing.T, st store.ObjectStore) (*Bridge, *recMetrics, *fakeSink) {
	t.Helper()
	repo := &fakeRepo{minSeq: 1, head: 1, entries: []*proto.LogEntry{
		mkEntry(1, proto.EntryKindPush, nil, upd("refs/heads/main", testZero40, "aaaa")),
	}}
	src := newFakeSource(repo)
	sink := newFakeSink("webhook")
	met := newRecMetrics()
	br := New(Deps{Source: src, Store: st, Sinks: []Sink{sink}, Metrics: met})
	return br, met, sink
}

func TestHandleNotify_QueuedDroppedAndIgnored(t *testing.T) {
	br, _, _ := notifyRepoFor(t, store.NewMemory())

	w := postNotify(t, br, `{"key":"`+manifestKey("acme/monorepo")+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if got := w.Body.String(); got != `[{"repo":"acme/monorepo","status":"queued"}]`+"\n" {
		t.Errorf("body = %q", got)
	}

	// Without a running bridge goroutine the wake is still queued in the work
	// channel: a second wake for the same repo coalesces to dropped.
	w = postNotify(t, br, `{"repo":"acme/monorepo"}`)
	if w.Code != http.StatusOK || w.Body.String() != `[{"repo":"acme/monorepo","status":"dropped"}]`+"\n" {
		t.Errorf("coalesced report: status=%d body=%s", w.Code, w.Body)
	}

	// Glue repo form queues directly (different repo).
	w = postNotify(t, br, `{"repo":"other/repo"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"queued"`) {
		t.Errorf("glue repo: status=%d body=%s", w.Code, w.Body)
	}

	// A recognized object event with a non-trigger key is acked and ignored.
	w = postNotify(t, br, `{"key":"repos/acme/monorepo/wal/x.pack"}`)
	if w.Code != http.StatusOK || w.Body.String() != `[{"repo":"","status":"ignored"}]`+"\n" {
		t.Errorf("ignored report: status=%d body=%s", w.Code, w.Body)
	}
}

func TestHandleNotify_InvalidJSONIs400(t *testing.T) {
	br, _, _ := notifyRepoFor(t, store.NewMemory())
	w := postNotify(t, br, `{not json`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON → %d, want 400", w.Code)
	}
}

func TestHandleNotify_DroppedWhenChannelFull(t *testing.T) {
	br, _, _ := notifyRepoFor(t, store.NewMemory())
	// Fill the work channel; the handler must never block.
	for i := range workCap {
		if br.Wake("owner/r"+string(rune('a'+i))) != StatusQueued {
			t.Fatal("setup: expected queued")
		}
	}
	w := postNotify(t, br, `{"repo":"owner/overflow"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Body.String(); got != `[{"repo":"owner/overflow","status":"dropped"}]`+"\n" {
		t.Errorf("body = %q (dropped wake must be reported, not blocked on)", got)
	}
}

func TestHandleNotify_SinkFailureIs503(t *testing.T) {
	st := store.NewMemory()
	br, _, sink := notifyRepoFor(t, st)
	sink.failsLeft = 1

	// Prime: a catch-up that fails at the sink records the failure.
	if _, err := br.catchUp(context.Background(), "owner/r0"); err == nil {
		t.Fatal("setup: catch-up must fail")
	}
	// The next notify runs the catch-up synchronously and reports the failure.
	sink.failsLeft = 1
	w := postNotify(t, br, `{"repo":"owner/r0"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("sink failure → %d, want 503 (notifier redelivers)", w.Code)
	}

	// Recovery: with the sink healthy the same notify answers 200. (The failed
	// 503 request left the wake queued in the work channel — no bridge goroutine
	// is running in this test — so take() it out of the pending set first.)
	br.take("owner/r0")
	w = postNotify(t, br, `{"repo":"owner/r0"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("recovered notify → %d, body = %s", w.Code, w.Body)
	}
	if got := w.Body.String(); !strings.Contains(got, `"status":"queued"`) {
		t.Errorf("body = %q", got)
	}
}
