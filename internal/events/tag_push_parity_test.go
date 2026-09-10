// tag_push_parity_test.go — Forgejo #263 acceptance: a PUSH-shaped
// refs/tags/* create (the annotated-tag publish: pack + txn with NewPeeled)
// emits the identical tag event as a REF_UPDATE create (the lightweight
// path). The bridge keys off txn ref updates by ref kind, never the entry
// kind — this test proves it instead of assuming it.
package events

import (
	"reflect"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func TestTagCreatePushRefUpdateParity(t *testing.T) {
	tagOid := strings.Repeat("a", 40)
	peeled := strings.Repeat("b", 40)
	meta := map[string]string{"principal": "jane", "request_id": "r1"}
	txn := func() *proto.RefTransaction {
		return &proto.RefTransaction{Updates: []*proto.RefUpdate{{
			Name:      "refs/tags/v2",
			OldOid:    testZero40,
			NewOid:    tagOid,
			NewPeeled: peeled,
		}}}
	}
	push := &proto.LogEntry{Seq: 7, Kind: proto.EntryKindPush, Txn: txn(), Meta: meta}
	refup := &proto.LogEntry{Seq: 7, Kind: proto.EntryKindRefUpdate, Txn: txn(), Meta: meta}

	pe := eventsFromEntry("o/r", push, git.Sha1)
	re := eventsFromEntry("o/r", refup, git.Sha1)
	if len(pe) != 1 || len(re) != 1 {
		t.Fatalf("events = %d/%d, want 1/1", len(pe), len(re))
	}
	// The tag fields are identical: create, tag kind, full ref, zero→tag
	// oid, principal attribution.
	for _, e := range []RefEvent{pe[0], re[0]} {
		if e.Action != ActionCreate || e.RefType != "tag" || e.RefName != "refs/tags/v2" {
			t.Fatalf("event = %+v", e)
		}
		if e.Old != testZero40 || e.New != tagOid || e.Pusher != "jane" || e.Repo != "o/r" {
			t.Fatalf("event = %+v", e)
		}
	}
	if pe[0].Walgit.EntryKind != KindPushWire || re[0].Walgit.EntryKind != KindRefUpdateWire {
		t.Fatalf("envelopes = %q/%q", pe[0].Walgit.EntryKind, re[0].Walgit.EntryKind)
	}
	// Modulo the envelope's entry_kind (which names the WAL kind by
	// construction), the two events are field-for-field identical.
	pe[0].Walgit.EntryKind, re[0].Walgit.EntryKind = "", ""
	if !reflect.DeepEqual(pe[0], re[0]) {
		t.Fatalf("push event %+v != ref_update event %+v", pe[0], re[0])
	}
}
