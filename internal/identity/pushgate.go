// pushgate.go — the repo-scoped push admission rule (Forgejo #347): the
// receive-pack gate both transports enforce at dispatch.
//
// The push gate is P6 resolution WITHOUT the host write-flag grant: a key
// (or HTTP credential) may push to owner/repo iff the principal owns the
// repo, is attached to the owning org (org-owner role, or a team/explicit
// binding reaching write), holds an explicit binding reaching write, or is
// a host admin (documented bypass). A host-wide "write" flag alone grants
// nothing — that flag is the hole this rule closes.
//
// Auto-create-on-push is NOT decided here: a push that would create a repo
// gates the owner segment through CheckCreateOwner first (the #346 reuse
// contract on creategate.go — same 401/403/503 shape), and only an existing
// repo reaches CheckPush.
//
// ### Concurrency
// Hazard: a binding/roster edit racing the gate probe. Avoidance: the probe
// is whole-object reads (access.json conditional GET, roster/team exact-key
// GETs); a concurrent demotion affects the next request, never tears this
// one. No lock is held across any store call.
package identity

import (
	"context"
	"fmt"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// CheckPush enforces the repo-scoped push rule for one existing repo: host
// admin passes without touching the store (so auth-none and the push budget
// fast path cost zero reads); anonymous fails closed with 401 (real 401 —
// law 9, so git erases the credential); otherwise P6 resolution over the
// principal's NAME with the host write/admin flags stripped must reach
// write. Stripping is load-bearing: Resolve step 3 would otherwise re-grant
// exactly the host-wide write this gate removes. A deny names the repo and
// the required relationship (403); callers surface it on the git wire
// (HTTP: the §4.2 failure mapping; SSH: stderr + exit 1).
func (s *Service) CheckPush(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError {
	if p.Admin {
		return nil
	}
	if p.Anonymous {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	// Self-namespace (the #346 self rule, mirrored): the owner segment IS
	// the principal's username. Email owners are usually covered by the
	// synthesized default below, but slug namespaces (instances whose
	// token mapping mints slug names, 01 §5.2/#234) carry no
	// user:<owner> binding — without this, a user could create under
	// their own name yet never push to it.
	if normPrincipal(owner) == normPrincipal(p.Name) {
		return nil
	}
	// The name is the whole identity here: host flags stay off (see
	// above), so owner/org/binding resolution decides alone.
	role, _ := s.Resolve(ctx, owner, repo, auth.Principal{Name: p.Name})
	if role.atLeast(RoleWrite) {
		return nil
	}
	return &auth.AuthError{Kind: auth.ErrForbidden, Why: fmt.Sprintf(
		"write access to %q requires the repo owner, owning-org membership, or an explicit write binding",
		owner+"/"+repo)}
}
