// mirror.go — the pull-only mirror push refusal (Forgejo #240, R1 (c)):
// a repo with a live meta/mirror.json sidecar refuses every client
// push on every transport, for every principal (admins included).
// Fetches/clones are unaffected (read paths never consult this).
//
// Placement (the funnel, per the review): the check sits at the top
// of pushPipeline (internal/server/bind_ssh.go — the ONE function both
// HTTP receivePackLocal and SSH land in), plus the two advertisements
// (HTTP gitInfoRefs discovery → 403; SSHReceivePack → stderr error
// BEFORE the v0 advertisement, else the client hangs). In-pipeline
// refusal is git-wire (per-ref ng lines, the managed-ref precedent),
// since the HTTP body is a git stream by then.
//
// Server-side publishers (the PR merge task, mirror sync) publish via
// Publish/PublishRefs directly and never enter pushPipeline, so the
// sync cannot refuse itself (internal/git/managed.go header comment;
// follow §8.4 "configuration, not a principal").
//
// Core never imports the mirror package (law 8): the guard is an
// injected predicate over the same store, wired once in composition
// (cmd/walhub). Nil → legacy behavior (no refusals).
package server

import (
	"context"

	"git.packden.us/crueber/walhub/internal/git"
)

// MirrorRefusal is the push-refusal text on every transport: the
// discovery 403 body, the in-pipeline per-ref ng reason, and the SSH
// pre-advertisement stderr error (surfaced as "walhub: <msg>").
const MirrorRefusal = "this repository is a read-only mirror; pushes are rejected"

// isMirrorRepo reports whether id is a pull-only mirror (nil guard →
// false — legacy behavior, unchanged for instances without the mirror
// surface wired).
func (s *Server) isMirrorRepo(ctx context.Context, id git.RepoId) bool {
	if s.mirrorGuard == nil {
		return false
	}
	return s.mirrorGuard(ctx, id)
}
