// event_test.go — the §3 classification table, zero-OID lengths for both object
// formats, _walgit.seq string encoding, meta plumbing, and the exact wire shape.
package events

import (
	"encoding/json"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func TestRefType(t *testing.T) {
	for _, tc := range []struct {
		ref, want string
	}{
		{"refs/heads/main", "branch"},
		{"refs/tags/v1.0", "tag"},
		{"refs/notes/commits", ""},
		{"refs/changes/1", ""},
		{"HEAD", ""},
		{"", ""},
	} {
		if got := refType(tc.ref); got != tc.want {
			t.Errorf("refType(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

func TestClassify(t *testing.T) {
	oidA, oidB := "48a0637aaaa", "cb38da1bbb"
	for _, tc := range []struct {
		name       string
		old, new   string
		wantAction string
	}{
		{"create", testZero40, oidB, ActionCreate},
		{"delete", oidA, testZero40, ActionDelete},
		{"update", oidA, oidB, ActionUpdate},
		{"noop_same_nonzero", oidA, oidA, ""},
		{"noop_zero_to_zero", testZero40, testZero40, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.old, tc.new); got != tc.wantAction {
				t.Fatalf("classify(%q, %q) = %q, want %q", tc.old, tc.new, got, tc.wantAction)
			}
		})
	}
}

func TestZeroOIDs(t *testing.T) {
	if got := zeroOID(40); len(got) != 40 || strings.Trim(got, "0") != "" {
		t.Errorf("zeroOID(40) = %q", got)
	}
	if got := zeroOID(64); len(got) != 64 || strings.Trim(got, "0") != "" {
		t.Errorf("zeroOID(64) = %q", got)
	}
	if !isZeroOID("") || !isZeroOID("00") || isZeroOID("0a0") {
		t.Error("isZeroOID broken")
	}
}

func TestEventsFromEntry_PushTable(t *testing.T) {
	a, b, c := "48a0637a", "cb38da1b", "deadbeef"
	e := mkEntry(42, proto.EntryKindPush,
		map[string]string{"principal": "alice@example.com", "request_id": "d1f916f7"},
		upd("refs/heads/main", testZero40, b), // create
		upd("refs/heads/dev", a, c),           // update
		upd("refs/heads/gone", a, testZero40), // delete
		upd("refs/tags/v1", testZero40, a),    // tag create
		upd("refs/notes/x", a, a),             // no-op suppressed
		upd("refs/notes/c", a, b),             // non-branch/tag ref_type
	)
	// Symbolic HEAD retarget emits nothing.
	e.Txn.Updates = append(e.Txn.Updates, &proto.RefUpdate{Name: "HEAD", NewSymbolicTarget: "refs/heads/dev"})

	events := eventsFromEntry("acme/monorepo", e, git.Sha1)
	if len(events) != 5 {
		t.Fatalf("len(events) = %d, want 5 (no-op + symbolic suppressed): %+v", len(events), events)
	}
	want := []struct {
		action, refType, refName, old, new string
	}{
		{ActionCreate, "branch", "refs/heads/main", testZero40, b},
		{ActionUpdate, "branch", "refs/heads/dev", a, c},
		{ActionDelete, "branch", "refs/heads/gone", a, testZero40},
		{ActionCreate, "tag", "refs/tags/v1", testZero40, a},
		{ActionUpdate, "", "refs/notes/c", a, b},
	}
	for i, w := range want {
		got := events[i]
		if got.Action != w.action || got.RefType != w.refType || got.RefName != w.refName ||
			got.Old != w.old || got.New != w.new {
			t.Errorf("event[%d] = %+v, want %+v", i, got, w)
		}
		if got.Repo != "acme/monorepo" {
			t.Errorf("event[%d].Repo = %q", i, got.Repo)
		}
		if got.Pusher != "alice@example.com" || got.CorrelationID != "d1f916f7" {
			t.Errorf("event[%d] meta plumbing = %q/%q", i, got.Pusher, got.CorrelationID)
		}
		wg := got.Walgit
		if wg.SchemaVersion != 1 || wg.Seq != "42" || wg.EntryKind != KindPushWire || wg.RequestID != "d1f916f7" {
			t.Errorf("event[%d]._walgit = %+v", i, wg)
		}
	}
}

func TestEventsFromEntry_RefUpdateKind(t *testing.T) {
	e := mkEntry(7, proto.EntryKindRefUpdate, nil, upd("refs/heads/main", testZero40, "aaa"))
	events := eventsFromEntry("o/r", e, git.Sha1)
	if len(events) != 1 || events[0].Walgit.EntryKind != KindRefUpdateWire {
		t.Fatalf("ref_update entry → %+v", events)
	}
	if events[0].Pusher != "" || events[0].CorrelationID != "" || events[0].Walgit.RequestID != "" {
		t.Error("absent meta must stay empty string")
	}
}

func TestEventsFromEntry_NonEmitters(t *testing.T) {
	a := "aaaa"
	for _, tc := range []struct {
		name  string
		entry *proto.LogEntry
	}{
		{"compact", &proto.LogEntry{Seq: 1, Kind: proto.EntryKindCompact}},
		{"checkpoint", &proto.LogEntry{Seq: 1, Kind: proto.EntryKindCheckpoint}},
		{"settings", &proto.LogEntry{Seq: 1, Kind: proto.EntryKindSettings}},
		{"push_without_txn", &proto.LogEntry{Seq: 1, Kind: proto.EntryKindPush}},
		{"nil_entry", nil},
		{"symbolic_only", &proto.LogEntry{Seq: 1, Kind: proto.EntryKindRefUpdate, Txn: &proto.RefTransaction{
			Updates: []*proto.RefUpdate{{Name: "HEAD", NewSymbolicTarget: "refs/heads/dev"}},
		}}},
		{"noop_only", &proto.LogEntry{Seq: 1, Kind: proto.EntryKindPush, Txn: &proto.RefTransaction{
			Updates: []*proto.RefUpdate{{Name: "refs/heads/main", OldOid: a, NewOid: a}},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventsFromEntry("o/r", tc.entry, git.Sha1); len(got) != 0 {
				t.Fatalf("non-emitter produced %+v", got)
			}
		})
	}
}

func TestEventsFromEntry_ZeroOIDLengths(t *testing.T) {
	for _, tc := range []struct {
		sha256  bool
		wantLen int
	}{
		{false, 40},
		{true, 64},
	} {
		format := git.Sha1
		if tc.sha256 {
			format = git.Sha256
		}
		zero := strings.Repeat("0", tc.wantLen)
		oid := strings.Repeat("a", tc.wantLen)
		// Empty-recorded and short zeros must come out full-length; literal
		// non-zero OIDs carry through verbatim.
		e := mkEntry(1, proto.EntryKindPush, nil,
			upd("refs/heads/main", "", oid),
			upd("refs/heads/gone", oid, zero),
			upd("refs/heads/new", zero, oid),
		)
		events := eventsFromEntry("o/r", e, format)
		if len(events) != 3 {
			t.Fatalf("sha256=%v: len = %d", tc.sha256, len(events))
		}
		for i, ev := range events {
			if ev.Old == "" || ev.New == "" {
				t.Errorf("event[%d] has empty OID: %+v", i, ev)
			}
			if isZeroOID(ev.Old) && len(ev.Old) != tc.wantLen {
				t.Errorf("event[%d].Old zero length = %d, want %d", i, len(ev.Old), tc.wantLen)
			}
			if isZeroOID(ev.New) && len(ev.New) != tc.wantLen {
				t.Errorf("event[%d].New zero length = %d, want %d", i, len(ev.New), tc.wantLen)
			}
			if ev.Old != zero && len(ev.Old) != tc.wantLen {
				t.Errorf("event[%d].Old = %q", i, ev.Old)
			}
		}
	}
}

// TestRefEventWireShape pins the exact JSON shape (09 §3: the compatibility
// contract), including _walgit.seq as a JSON string.
func TestRefEventWireShape(t *testing.T) {
	ev := RefEvent{
		Action:        ActionUpdate,
		RefType:       "branch",
		RefName:       "refs/heads/main",
		Old:           "48a0637aaaa",
		New:           "cb38da1bbbb",
		Pusher:        "alice@example.com",
		CorrelationID: "d1f916f7",
		Repo:          "acme/monorepo",
		Walgit:        Walgit{SchemaVersion: 1, Seq: "42", EntryKind: KindPushWire, RequestID: "d1f916f7"},
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"action", "ref_type", "ref_name", "old", "new", "pusher", "correlation_id", "repo", "_walgit"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("missing key %q in %s", key, raw)
		}
	}
	if len(doc) != 9 {
		t.Errorf("unexpected extra keys in %s", raw)
	}
	wire, _ := doc["_walgit"].(map[string]any)
	for _, key := range []string{"schema_version", "seq", "entry_kind", "request_id"} {
		if _, ok := wire[key]; !ok {
			t.Errorf("missing _walgit.%q in %s", key, raw)
		}
	}
	if seq, ok := wire["seq"].(string); !ok || seq != "42" {
		t.Errorf("_walgit.seq = %v (%T), want string \"42\"", wire["seq"], wire["seq"])
	}
	if v, ok := doc["action"].(string); !ok || v != "update" {
		t.Errorf("action = %v", doc["action"])
	}
}
