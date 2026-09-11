// visibility.go — public/private listing filters (Forgejo #345).
//
// Visibility is the read authority for repo surfaces: once the dispatch
// layer admits an anonymous caller to a public repo, the listings must not
// leak the private ones back. Every listing below omits repos the caller
// cannot read (CheckRead != nil), so no private repo name appears in a
// name, count, or activity/size aggregate.
//
// Cost (law 6): filtering is one CheckRead per candidate repo — behind it,
// one conditional access.json GET that the access LRU usually answers
// without a body (NotModified on a version hit). Callers holding the host
// write/admin flags skip the probes entirely (CheckRead early-allows them,
// so filtering would be a no-op). Listings are control-plane (SWR-cached
// client-side); the law-6 budgeted paths (push ≤ 5, warm refs 1, cold
// refs 2, checkpoint 4) never call here. Nil Access → legacy unfiltered
// behavior, byte-identical.
package api

import (
	"context"
	"errors"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// errNoRegistry maps through mapViewErr to 503 (the §2 unknown → 503 rule
// the listings already apply when the registry is unwired).
var errNoRegistry = errors.New("repo registry not configured")

// VisibleRepos returns the sorted short repo names under owner readable by
// p: the registry list minus every repo CheckRead denies. Nil Access or a
// host admin/write caller → the registry list unfiltered (+0 store trips).
func (e *Env) VisibleRepos(ctx context.Context, owner string, p auth.Principal) ([]string, error) {
	if e.Repos == nil {
		return nil, errNoRegistry
	}
	names, err := e.Repos.Repos(ctx, owner)
	if err != nil {
		return nil, err
	}
	return e.filterReadable(ctx, owner, names, p), nil
}

// VisibleOwners returns the sorted owner names having at least one repo
// readable by p. Nil Access or a host admin/write caller → the registry
// list unfiltered (+0 store trips).
func (e *Env) VisibleOwners(ctx context.Context, p auth.Principal) ([]string, error) {
	if e.Repos == nil {
		return nil, errNoRegistry
	}
	names, err := e.Repos.Owners(ctx)
	if err != nil {
		return nil, err
	}
	if e.unfiltered(p) {
		return names, nil
	}
	out := make([]string, 0, len(names))
	for _, owner := range names {
		repos, rerr := e.Repos.Repos(ctx, owner)
		if rerr != nil {
			return nil, rerr
		}
		if len(e.filterReadable(ctx, owner, repos, p)) > 0 {
			out = append(out, owner)
		}
	}
	return out, nil
}

// VisibleReposByOwner returns, for every registry owner, the repos readable
// by p, omitting owners left with none. It is the one-pass source for the
// owners/detailed surface (counts + rollup allowlist share the walk, law 6).
// Nil Access or a host admin/write caller → every owner's full list.
func (e *Env) VisibleReposByOwner(ctx context.Context, p auth.Principal) (map[string][]string, error) {
	if e.Repos == nil {
		return nil, errNoRegistry
	}
	names, err := e.Repos.Owners(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(names))
	for _, owner := range names {
		repos, rerr := e.Repos.Repos(ctx, owner)
		if rerr != nil {
			return nil, rerr
		}
		if visible := e.filterReadable(ctx, owner, repos, p); len(visible) > 0 {
			out[owner] = visible
		}
	}
	return out, nil
}

// unfiltered reports whether the caller bypasses per-repo visibility
// probes: no gate wired (legacy), or a host admin/write principal (the
// require_read hook early-allows them without consulting access.json).
func (e *Env) unfiltered(p auth.Principal) bool {
	if e.Access == nil {
		return true
	}
	return !p.Anonymous && (p.Admin || p.Write)
}

// filterReadable drops every repo CheckRead denies, preserving order.
// Unfiltered callers (see unfiltered) get the input slice untouched.
func (e *Env) filterReadable(ctx context.Context, owner string, names []string, p auth.Principal) []string {
	if e.unfiltered(p) {
		return names
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if e.Access.CheckRead(ctx, owner, n, p) == nil {
			out = append(out, n)
		}
	}
	return out
}

// repoVisibility reports the visibility spelling for projections (summary,
// listing rows): "public"|"private", ok=false when the hook is unwired or
// declines (marshal emits "" then — never null).
func (e *Env) repoVisibility(ctx context.Context, owner, repo string) (vis string, ok bool) {
	if e.RepoVisibility == nil {
		return "", false
	}
	return e.RepoVisibility(ctx, owner, repo)
}
