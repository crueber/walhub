package git

import "strings"

// Server-managed ref namespaces (docs/features/03 §3): client pushes to
// these namespaces are rejected on every transport with
// `ng "rejected by rule 'pull-refs-managed'"`. The merge task and the PR
// open path publish these refs server-side through the WAL publish funnel,
// which never consults this guard (server-side publishes bypass
// receive-pack/policy by construction).
//
// ### Concurrency
//
// Hazard: none — pure string predicate, no state, safe for concurrent use
// from every push goroutine.
const (
	// ManagedRefPrefix is the server-managed PR ref namespace.
	ManagedRefPrefix = "refs/pull/"
	// ManagedRefRule is the rule name carried on the wire refusal
	// (`rejected by rule '<name>'`, the policy Verdict convention).
	ManagedRefRule = "pull-refs-managed"
)

// IsManagedRef reports whether ref is server-managed (client pushes always
// refused; only server-side publishers may move it).
func IsManagedRef(ref string) bool {
	return strings.HasPrefix(ref, ManagedRefPrefix)
}

// ManagedRefReason is the per-ref ng reason for a refused managed-ref push.
func ManagedRefReason() string {
	return "rejected by rule '" + ManagedRefRule + "'"
}
