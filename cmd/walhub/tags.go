// tags.go — Forgejo #253/#263 composition: the tags service (Seam 1, both
// lanes) over the P6 roles owned by identity, stock git through the bounded
// pool, repo dirs through the WAL registry, and ref creates through the WAL
// publish funnel. Nothing here is a second writer: lightweight tag creates
// publish via RepoHandle.Publish with a single-update REF_UPDATE txn, and
// annotated tag creates (#263) publish the pre-packed tag object with the
// same txn shape plus NewPeeled as one PUSH entry (the manifest CAS
// arbitrates; a present tag fails the create, never force-moves).
package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/tags"
	"git.packden.us/crueber/walhub/internal/wal"
)

// newTagsService builds the tags service over st/ident. Git, Dirs, and Refs
// are wired by the caller (serveHTTP).
func newTagsService(st store.ObjectStore, ident *identity.Service, reg *wal.Registry, gitBinary string) (*tags.Service, *tags.Handler) {
	svc := tags.New(st, ident)
	svc.Git = tags.NewSubprocessGit(gitBinary)
	svc.Dirs = &pullsDirs{reg: reg}
	svc.Refs = &tagsPublisher{reg: reg}
	h := &tags.Handler{Svc: svc}
	return svc, h
}

// chainTags fronts the core mux with the tags lane surface (Seam 1);
// authentication resolves through the server chain (Seam 2), including the
// §8.6 broker-forwarding rule — the forwarded principal replaces the
// broker's, never the broker itself.
func chainTags(srv *server.Server, h *tags.Handler) {
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().AuthenticateForwarded(r, srv.Config())
	}
	srv.ChainExtra(h)
}

// tagsPublisher publishes tag creates through the normal WAL publish funnel
// (doc 05 CAS ladder): one single-update REF_UPDATE txn with OldOid zero =
// create, so the manifest verify step arbitrates against concurrent pushes
// and concurrent API creates. A present tag fails the create (409 at the
// service); the publisher never force-moves.
type tagsPublisher struct {
	reg *wal.Registry
}

// zeroOid renders the all-zero absent marker for the repo's object format
// (sized from the target sha, else the manifest format).
func (p *tagsPublisher) zeroOid(ctx context.Context, repo, likeSHA string) string {
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

// CreateAnnotatedTag publishes an annotated tag (Forgejo #263, ruling (b)):
// the single-object pack (tag object, produced by `git pack-objects
// --stdout` in the tags package) is ingested through the existing
// Layer.Ingest path (complete pack, never thin; fsck on — the object passed
// mktag's strict fsck already), then published as one PUSH entry whose txn
// creates refs/tags/<name> at the tag oid with NewPeeled = the peeled
// commit. The pack PUT is the replication unit (law 4): a REF_UPDATE-only
// publish would advertise a ref whose object replicas lack. Per-ref verdicts
// map back to Go errors (conflicts carry the "conflict" wording the service
// maps to 409).
func (p *tagsPublisher) CreateAnnotatedTag(ctx context.Context, repo, name, tagOid, peeled string, pack []byte, meta map[string]string) error {
	h, err := p.reg.Open(ctx, repo)
	if err != nil {
		return err
	}
	prepared, err := ingestTagPack(ctx, p.reg, h, pack)
	if err != nil {
		return err
	}
	agent := map[string]string{}
	for k, v := range meta {
		agent[k] = v
	}
	res, err := h.Publish(ctx, wal.PublishRequest{
		Pack: prepared,
		Txn: &proto.RefTransaction{Updates: []*proto.RefUpdate{{
			Name:      "refs/tags/" + name,
			OldOid:    p.zeroOid(ctx, repo, tagOid),
			NewOid:    tagOid,
			NewPeeled: peeled,
		}}},
		Meta: agent,
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

// ingestTagPack ingests one complete single-object pack into the serving
// copy and renders the PreparedPack the publish funnel uploads (pack PUT +
// idx PUT, create-if-absent). The pack bytes already sit in memory and are
// O(KiB); maxBytes just covers them.
func ingestTagPack(ctx context.Context, reg *wal.Registry, h *wal.RepoHandle, pack []byte) (*wal.PreparedPack, error) {
	res, err := reg.GitLayer().Ingest(ctx, h.Repo(), bytes.NewReader(pack), int64(len(pack)+1), false, true)
	if err != nil {
		return nil, err
	}
	sum := string(res.Checksum)
	packDir := h.Repo().PackDir()
	packPath := filepath.Join(packDir, "pack-"+sum+".pack")
	idxPath := filepath.Join(packDir, "pack-"+sum+".idx")
	packFi, err := os.Stat(packPath)
	if err != nil {
		return nil, err
	}
	idxFi, err := os.Stat(idxPath)
	if err != nil {
		return nil, err
	}
	return &wal.PreparedPack{
		Checksum:    sum,
		PackPath:    packPath,
		IdxPath:     idxPath,
		PackSize:    uint64(packFi.Size()),
		IdxSize:     uint64(idxFi.Size()),
		ObjectCount: res.ObjectCount,
	}, nil
}

// CreateTag creates refs/tags/<name> → sha (CAS create: a live tag fails,
// never moves). Per-ref verdicts map back to Go errors (conflicts carry
// the "conflict" wording the service maps to 409).
func (p *tagsPublisher) CreateTag(ctx context.Context, repo, name, sha string, meta map[string]string) error {
	h, err := p.reg.Open(ctx, repo)
	if err != nil {
		return err
	}
	agent := map[string]string{}
	for k, v := range meta {
		agent[k] = v
	}
	res, err := h.Publish(ctx, wal.PublishRequest{
		Txn: &proto.RefTransaction{Updates: []*proto.RefUpdate{{
			Name:   "refs/tags/" + name,
			OldOid: p.zeroOid(ctx, repo, sha),
			NewOid: sha,
		}}},
		Meta: agent,
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

// compile-time seam assertions: composition consumes exactly the narrow
// interfaces the tags package defines (core never imports tags).
var (
	_ tags.RoleService   = (*identity.Service)(nil)
	_ tags.RepoDirs      = (*pullsDirs)(nil)
	_ tags.RefPublisher  = (*tagsPublisher)(nil)
	_ server.ExtraRoutes = (*tags.Handler)(nil)
)
