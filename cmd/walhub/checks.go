// checks.go — Wave 05 composition (docs/features/05): the checks service
// (Seam 1, both lanes) over the P6 roles owned by identity, sha validation
// through the serve-synced git dirs, and the wct_ CI-token credential
// shape (Seam 2) resolved to an unprivileged ci:<id> principal with the
// scoped capability checked handler-side. The require_checks merge-time
// half is consulted by the pull-merge task through pulls' ChecksGate seam
// (see internal/pulls/checks.go — the merge logic is NOT forked).
// Notifications/stream stay nil until internal/notify lands (documented
// no-op, P8 backfill via the per-context objects).
package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"git.packden.us/crueber/walhub/internal/checks"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/pulls"
	"git.packden.us/crueber/walhub/internal/server"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// newChecksService builds the checks service over st/ident. Sha validation
// rides the serve-synced dirs plus stock git (git rev-parse --verify
// --quiet <sha>^{commit} — the same argv pulls resolves through); the
// merge task consults the gate through pullsSvc.Checks. Notify/Stream
// stay nil until internal/notify lands.
func newChecksService(st store.ObjectStore, ident *identity.Service, pullsSvc *pulls.Service, reg *wal.Registry, gitBinary string) (*checks.Service, *checks.Handler) {
	svc := checks.New(st, ident)
	git := pulls.NewSubprocessGit(gitBinary)
	dirs := &pullsDirs{reg: reg}
	svc.Commits = &checksCommits{git: git, dirs: dirs}
	if pullsSvc != nil {
		pullsSvc.Checks = svc
	}
	h := &checks.Handler{Svc: svc}
	return svc, h
}

// chainChecks fronts the core mux with the checks surface (Seam 1) and
// registers the wct_ credential shape on the server chain (Seam 2):
// wct_ credentials resolve to the unprivileged ci:<id> principal at the
// apiServe layer (so token/oidc modes don't 401 them before dispatch)
// and again in the handler's Auth wrapper (so the handler sees the same
// principal the git paths resolve). The secret itself is verified
// handler-side per repo — the chain never sees it. Startup validates the
// wct_ prefix overlaps no core prefix (wgt_); overlap panics.
func chainChecks(srv *server.Server, h *checks.Handler) {
	checks.AssertPrefixDisjoint("wgt_", "Bearer ", "Basic ")
	srv.Auth().ExtraCredential = func(token string) (auth.Principal, *auth.AuthError, bool) {
		if !checks.ClaimToken(token) {
			return auth.Principal{}, nil, false
		}
		id, _, err := checks.ParseCIToken(token)
		if err != nil {
			return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "invalid CI token"}, true
		}
		return auth.Principal{Name: checks.CIPrincipalName(id)}, nil, true
	}
	h.Auth = checks.WrapAuth(func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().Authenticate(r, srv.Config())
	})
	srv.ChainExtra(h)
}

// checksCommits resolves a sha to a commit through the serve-synced dir.
// Unknown or non-commit shas map to the checks 404 class; transport
// failures (pool, timeout, missing binary) map to 503 — never
// misreported as unknown (the same contract pulls' ResolveRef keeps).
type checksCommits struct {
	git  *pulls.SubprocessGit
	dirs *pullsDirs
}

func (c *checksCommits) ResolveCommit(ctx context.Context, repo, sha string) (string, error) {
	dir, err := c.dirs.Dir(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("%w: repo unavailable: %v", checks.ErrUnavailable, err)
	}
	resolved, rerr := c.git.ResolveRef(ctx, dir, sha)
	if rerr != nil {
		msg := strings.ToLower(rerr.Error())
		if strings.Contains(msg, "unknown revision") || strings.Contains(msg, "unknown pull") {
			return "", fmt.Errorf("%w: unknown sha %q", checks.ErrNotFound, sha)
		}
		return "", fmt.Errorf("%w: %v", checks.ErrUnavailable, rerr)
	}
	return resolved, nil
}

// compile-time seam assertions: the composition consumes exactly the
// narrow interfaces checks defines (core never imports checks).
var (
	_ checks.RoleService   = (*identity.Service)(nil)
	_ checks.CommitChecker = (*checksCommits)(nil)
	_ pulls.ChecksGate     = (*checks.Service)(nil)
)
