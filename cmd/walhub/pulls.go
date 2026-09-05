// pulls.go — Wave C1 composition (docs/features/03): the pulls service
// (Seam 1, both lanes) over the P6 roles owned by identity, the shared
// thread family owned by issues, stock git through the bounded pool, ref
// publishes through the WAL funnel, and WAL ref events through the events
// bridge. Nothing here is a second writer: numbering/threads/index reuse
// the shared issues keys; refs publish via RepoHandle.Publish with a
// REF_UPDATE txn (the manifest CAS arbitrates; the merge task never
// force-publishes).
package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"git.packden.us/crueber/walhub/internal/events"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/issues"
	"git.packden.us/crueber/walhub/internal/pulls"
	"git.packden.us/crueber/walhub/internal/server"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// newPullsService builds the pulls service over st/ident/issuesSvc. Git,
// Dirs, Refs, and Closer are wired by the caller (serveHTTP); Notify/Stream
// stay nil until internal/notify lands (documented no-op, P8 backfill via
// the timeline).
func newPullsService(st store.ObjectStore, ident *identity.Service, issuesSvc *issues.Service, reg *wal.Registry, gitBinary string) (*pulls.Service, *pulls.Handler) {
	svc := pulls.New(st, ident)
	svc.Git = pulls.NewSubprocessGit(gitBinary)
	svc.Dirs = &pullsDirs{reg: reg}
	svc.Refs = &pullsPublisher{reg: reg}
	if issuesSvc != nil {
		svc.Closer = issuesSvc
	}
	h := &pulls.Handler{Svc: svc}
	return svc, h
}

// chainPulls fronts the core mux with the pulls surface (Seam 1);
// authentication resolves through the server chain (Seam 2), including the
// §8.6 broker-forwarding rule — the forwarded principal replaces the
// broker's, never the broker itself.
func chainPulls(srv *server.Server, h *pulls.Handler) {
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().AuthenticateForwarded(r, srv.Config())
	}
	srv.ChainExtra(h)
}

// pullsDirs resolves a repo to its synced local git dir: open the WAL
// handle and sync to serve level (the same path any reader takes), then
// hand the bare repo path to the git runner. The read guard is released
// before returning — objects stay materialized on disk.
type pullsDirs struct {
	reg *wal.Registry
}

func (d *pullsDirs) Dir(ctx context.Context, repo string) (string, error) {
	h, err := d.reg.Open(ctx, repo)
	if err != nil {
		return "", err
	}
	if g, serr := h.Sync(ctx, wal.LevelServe); serr != nil {
		return "", serr
	} else {
		g.Release()
	}
	return h.Repo().Path, nil
}

// pullsPublisher publishes ref creates/updates/deletes through the normal
// WAL publish funnel (doc 05 CAS ladder): one REF_UPDATE txn per call with
// OldOid carried, so the manifest verify step arbitrates against
// concurrent pushes. Per-ref verdicts map back to Go errors (conflicts
// carry the "conflict" wording the merge task re-plans on).
type pullsPublisher struct {
	reg *wal.Registry
}

// zeroOid renders the all-zero absent marker for the repo's object format
// (sized from a known sha when available, else the manifest format).
func (p *pullsPublisher) zeroOid(ctx context.Context, repo, likeSHA string) string {
	if likeSHA != "" {
		return strings.Repeat("0", len(likeSHA))
	}
	h, err := p.reg.Open(ctx, repo)
	if err != nil {
		return strings.Repeat("0", 40)
	}
	m, _ := h.ManifestSnapshot()
	if m != nil && m.ObjectFormat == "sha256" {
		return strings.Repeat("0", 64)
	}
	return strings.Repeat("0", 40)
}

func (p *pullsPublisher) publish(ctx context.Context, repo string, updates []*proto.RefUpdate, meta map[string]string) error {
	h, err := p.reg.Open(ctx, repo)
	if err != nil {
		return err
	}
	res, err := h.Publish(ctx, wal.PublishRequest{
		Txn:  &proto.RefTransaction{Updates: updates},
		Meta: meta,
	})
	if err != nil {
		return err
	}
	for _, rr := range res.PerRef {
		if rr.Err != nil {
			if rr.Err.Kind == wal.RefErrConflict || rr.Err.Kind == wal.RefErrStale {
				return fmt.Errorf("CAS conflict: %s", rr.Err.Detail)
			}
			return fmt.Errorf("publish %s: %s", rr.Name, rr.Err.Detail)
		}
	}
	return nil
}

// liveSHA reads the live sha of one ref through the synced handle.
func (p *pullsPublisher) liveSHA(ctx context.Context, repo, ref string) (string, bool) {
	h, err := p.reg.Open(ctx, repo)
	if err != nil {
		return "", false
	}
	if g, serr := h.Sync(ctx, wal.LevelServe); serr != nil {
		return "", false
	} else {
		g.Release()
	}
	snap, serr := p.reg.GitLayer().Snapshot(h.Repo())
	if serr != nil {
		return "", false
	}
	if e, ok := snap.Get(ref); ok {
		return string(e.Oid), true
	}
	return "", false
}

// CreateRef creates ref → sha (idempotent: already-matching is a no-op,
// decided by a live read before the funnel).
func (p *pullsPublisher) CreateRef(ctx context.Context, repo, ref, sha string, meta map[string]string) error {
	if live, ok := p.liveSHA(ctx, repo, ref); ok && live == sha {
		return nil
	} else if ok {
		return fmt.Errorf("CAS conflict: %s exists at %s", ref, live)
	}
	agent := map[string]string{}
	for k, v := range meta {
		agent[k] = v
	}
	return p.publish(ctx, repo, []*proto.RefUpdate{{Name: ref, OldOid: p.zeroOid(ctx, repo, sha), NewOid: sha}}, agent)
}

// UpdateRef moves ref old → new (CAS: a moved ref fails, never forces).
func (p *pullsPublisher) UpdateRef(ctx context.Context, repo, ref, old, newSHA string, meta map[string]string) error {
	agent := map[string]string{}
	for k, v := range meta {
		agent[k] = v
	}
	return p.publish(ctx, repo, []*proto.RefUpdate{{Name: ref, OldOid: old, NewOid: newSHA}}, agent)
}

// DeleteRef deletes ref (policy-checked like any ref delete).
func (p *pullsPublisher) DeleteRef(ctx context.Context, repo, ref string, meta map[string]string) error {
	live, _ := p.liveSHA(ctx, repo, ref)
	zero := p.zeroOid(ctx, repo, live)
	agent := map[string]string{}
	for k, v := range meta {
		agent[k] = v
	}
	return p.publish(ctx, repo, []*proto.RefUpdate{{Name: ref, OldOid: live, NewOid: zero}}, agent)
}

// pullsSinkAdapter is the `pulls` event sink (Seam 4, §4 invalidation seam):
// WAL ref events flow from the bridge into HandleRefEvent, which enqueues
// matching open PRs onto the pull-mergeable recompute batch. Delivery never
// fails (invalidation is best-effort; the thread fetch re-verifies the
// stamp), so this sink never holds back the shared cursor. A per-sink
// cursor (events/cursors/pulls.json, 14 §14.6) is the follow-up: the bridge
// in this tree still advances one cursor per repo, which is safe here
// because recompute is idempotent and stamp-checked (replays converge).
type pullsSinkAdapter struct {
	svc *pulls.Service
}

// Name names the sink (metrics label).
func (s *pullsSinkAdapter) Name() string { return "pulls" }

// Deliver implements events.Sink.
func (s *pullsSinkAdapter) Deliver(ctx context.Context, repo string, batch []events.RefEvent) error {
	owner, name, ok := splitOwnerRepo(repo)
	if !ok {
		return nil
	}
	for _, e := range batch {
		if e.RefName == "" {
			continue
		}
		s.svc.HandleRefEvent(ctx, owner, name, pulls.RefEvent{
			Repo: repo, RefName: e.RefName, Old: e.Old, New: e.New,
		})
	}
	return nil
}

// splitOwnerRepo splits "owner/name".
func splitOwnerRepo(repo string) (string, string, bool) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// compile-time seam assertions: the composition consumes exactly the
// narrow interfaces pulls defines (core never imports pulls).
var (
	_ pulls.GitRunner    = (*pulls.SubprocessGit)(nil)
	_ pulls.RepoDirs     = (*pullsDirs)(nil)
	_ pulls.RefPublisher = (*pullsPublisher)(nil)
	_ pulls.IssueCloser  = (*issues.Service)(nil)
	_ pulls.RoleService  = (*identity.Service)(nil)
	_ events.Sink        = (*pullsSinkAdapter)(nil)
)
