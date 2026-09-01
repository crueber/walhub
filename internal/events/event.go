// Package events is the WAL events bridge (docs/go/09_events.md): one
// long-lived goroutine reads the WAL log per repo, classifies ref events, and
// delivers whole batches to sinks (one built-in webhook). Push paths never
// publish events; the bridge is the only producer.
package events

import (
	"strconv"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// RefEvent is the wire event (09 §3; this exact shape is the compatibility
// contract with the Rust implementation).
type RefEvent struct {
	Action        string `json:"action"`         // "create" | "update" | "delete" (force is NOT a wire action)
	RefType       string `json:"ref_type"`       // "branch" | "tag" | ""
	RefName       string `json:"ref_name"`       // full ref, e.g. "refs/heads/main"
	Old           string `json:"old"`            // hex OID; full zero OID on create
	New           string `json:"new"`            // hex OID; full zero OID on delete
	Pusher        string `json:"pusher"`         // log entry meta "principal"; "" when absent
	CorrelationID string `json:"correlation_id"` // user-visible request id (meta "request_id")
	Repo          string `json:"repo"`           // "owner/name"
	Walgit        Walgit `json:"_walgit"`
}

// Walgit is the "_walgit" envelope on the wire. Seq is a STRING — the
// uint64-as-string convention; the field name keeps the Rust wire name.
type Walgit struct {
	SchemaVersion int    `json:"schema_version"` // 1
	Seq           string `json:"seq"`            // STRING, decimal uint64 — never a JSON number
	EntryKind     string `json:"entry_kind"`     // "push" | "ref_update"
	RequestID     string `json:"request_id"`     // same value as CorrelationID
}

// Wire actions and entry kinds (Rust wire names, verbatim).
const (
	ActionCreate = "create"
	ActionUpdate = "update"
	ActionDelete = "delete"

	KindPushWire      = "push"
	KindRefUpdateWire = "ref_update"
)

// refType classifies a full ref name (09 §3): refs/heads/ → branch,
// refs/tags/ → tag, anything else → "".
func refType(ref string) string {
	switch {
	case len(ref) >= 11 && ref[:11] == "refs/heads/":
		return "branch"
	case len(ref) >= 10 && ref[:10] == "refs/tags/":
		return "tag"
	default:
		return ""
	}
}

// isZeroOID reports whether s is an all-zero (or empty) OID.
func isZeroOID(s string) bool {
	if s == "" {
		return true
	}
	for i := range len(s) {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

// zeroOID is the full zero OID of the given length (40 hex for sha1, 64 for
// sha256) — never "".
func zeroOID(hexLen int) string {
	b := make([]byte, hexLen)
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}

// oidHexLen maps the repo object format to the zero-OID length.
func oidHexLen(f git.ObjectFormat) int {
	if f == git.Sha256 {
		return 64
	}
	return 40
}

// classify implements the (old_oid, new_oid) → action table (09 §3, normative).
// old/new are already normalized (all-zero == absent). Equal values — including
// 0→0 — produce no event.
func classify(old, new string) string {
	oldZero, newZero := isZeroOID(old), isZeroOID(new)
	switch {
	case oldZero && newZero:
		return "" // no-op suppression
	case oldZero:
		return ActionCreate
	case newZero:
		return ActionDelete
	case old == new:
		return "" // no-op suppression
	default:
		return ActionUpdate
	}
}

// normalizeOID carries the literal OID but upgrades a zero/empty value to the
// full zero OID of the repo's length (09 §3: never "" on the wire).
func normalizeOID(s string, hexLen int) string {
	if isZeroOID(s) {
		return zeroOID(hexLen)
	}
	return s
}

// eventsFromEntry extracts the ref events of one WAL entry (09 §2, §3): only
// PUSH and REF_UPDATE entries with a txn emit; COMPACT, CHECKPOINT, SETTINGS,
// and symbolic HEAD retargets emit nothing. Updates emit in recorded order.
func eventsFromEntry(repo string, e *proto.LogEntry, format git.ObjectFormat) []RefEvent {
	if e == nil || e.Txn == nil {
		return nil
	}
	var kind string
	switch e.Kind {
	case proto.EntryKindPush:
		kind = KindPushWire
	case proto.EntryKindRefUpdate:
		kind = KindRefUpdateWire
	default:
		return nil
	}
	hexLen := oidHexLen(format)
	pusher := e.Meta["principal"]
	requestID := e.Meta["request_id"]
	var out []RefEvent
	for _, u := range e.Txn.Updates {
		if u == nil || u.NewSymbolicTarget != "" {
			continue // symbolic HEAD retarget emits nothing
		}
		old := normalizeOID(u.OldOid, hexLen)
		new := normalizeOID(u.NewOid, hexLen)
		action := classify(old, new)
		if action == "" {
			continue
		}
		out = append(out, RefEvent{
			Action:        action,
			RefType:       refType(u.Name),
			RefName:       u.Name,
			Old:           old,
			New:           new,
			Pusher:        pusher,
			CorrelationID: requestID,
			Repo:          repo,
			Walgit: Walgit{
				SchemaVersion: 1,
				Seq:           strconv.FormatUint(e.Seq, 10),
				EntryKind:     kind,
				RequestID:     requestID,
			},
		})
	}
	return out
}
