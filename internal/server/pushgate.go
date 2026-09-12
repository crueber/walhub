package server

import (
	"context"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/wal"
)

// PushGate is the repo-scoped push admission hook (Forgejo #347),
// consulted at receive-pack dispatch on both transports. Implemented by
// internal/identity; nil → legacy host-flag gating (law 8: the server
// never imports the feature package — composition in cmd/walhub wires it,
// exactly the ReadGate shape in bind_api.go).
type PushGate interface {
	// CheckPush enforces the repo-scoped write rule on one EXISTING
	// repo: owner / owning-org-attached / explicitly bound / host admin,
	// else deny (anonymous → 401, foreign → 403 naming the repo).
	CheckPush(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
	// CheckCreateOwner enforces the #346 admission rule on the owner
	// segment of a push that would auto-create a repo (self / member
	// org / host admin; 401 / 403 / 503 — the verbatim reuse contract).
	CheckCreateOwner(ctx context.Context, owner string, p auth.Principal) *auth.AuthError
}

// checkPushWrite enforces the existing-repo push rule. A nil gate keeps the
// legacy host-flag check (requireWrite); a wired gate is the authority and
// the host write flag is NOT consulted (identity strips it — the #347 fix).
func (s *Server) checkPushWrite(ctx context.Context, id git.RepoId, p auth.Principal) *auth.AuthError {
	if s.pushGate == nil {
		return requireWrite(p)
	}
	return s.pushGate.CheckPush(ctx, id.Owner, id.Name, p)
}

// checkPushCreate enforces the auto-create admission BEFORE any namespace
// write. A nil gate keeps the legacy behavior (auto-create open); a wired
// gate applies the #346 rule verbatim.
func (s *Server) checkPushCreate(ctx context.Context, owner string, p auth.Principal) *auth.AuthError {
	if s.pushGate == nil {
		return nil
	}
	return s.pushGate.CheckCreateOwner(ctx, owner, p)
}

// gatePush runs the push gate at receive-pack dispatch on both transports
// (mirroring the requireWrite position it replaces), where the repo's
// existence is not yet known. Allowed pushes proceed; a denied write
// proceeds ONLY when auto-create is on AND the repo is missing AND the #346
// admission passes — everything else is denied here, before any
// placement/sync work. The Sync probe runs on the deny path only (law 6);
// a missing repo surfaces as a Sync NotFound exactly like the discovery
// below will see it.
func (s *Server) gatePush(ctx context.Context, id git.RepoId, p auth.Principal, createOn bool) *auth.AuthError {
	if aerr := s.checkPushWrite(ctx, id, p); aerr == nil {
		return nil
	} else if !createOn {
		return aerr
	} else if serr := s.engine.Sync(ctx, id, wal.LevelRefs); serr == nil {
		return aerr // exists: the write deny stands
	} else if !isNotFound(serr) {
		return &auth.AuthError{Kind: auth.ErrUnavailable, Why: serr.Error()}
	}
	// Missing + auto-create: the #346 admission decides (same 403 shape
	// as explicit create — a foreign-owner squat dies here, not later).
	return s.checkPushCreate(ctx, id.Owner, p)
}
