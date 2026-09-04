package identity

import (
	"context"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// Bootstrap implements the Seam 5 access-bootstrap migration (01 §10):
// each sweep, for every repo still lacking access.json, Create the
// synthesized legacy default (creator binding user:<owner> admin).
// Idempotent (Create 412 → skip), restartable, orphan-tolerant. Edits to a
// repo with no access.json synthesize it themselves via the CAS path, so a
// bootstrap racing a first admin edit resolves to no-op for the loser.
//
// Returns created (repos materialized) and skipped (already present).
func (s *Service) Bootstrap(ctx context.Context) (created, skipped int, err error) {
	repos, err := s.Repos(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, rr := range repos {
		select {
		case <-ctx.Done():
			return created, skipped, ctx.Err()
		default:
		}
		c, sk, berr := s.bootstrapOne(ctx, rr[0], rr[1])
		created += c
		skipped += sk
		if berr != nil {
			return created, skipped, berr
		}
	}
	return created, skipped, nil
}

// BootstrapRepo materializes one repo's access.json when missing (the
// Seam 5 access-bootstrap op body, per-repo): true when created, false
// when already present.
func (s *Service) BootstrapRepo(ctx context.Context, owner, repo string) (bool, error) {
	c, _, err := s.bootstrapOne(ctx, owner, repo)
	return c == 1, err
}

// bootstrapOne Creates the synthesized default; 412 (already exists or a
// concurrent writer won) counts as skipped.
func (s *Service) bootstrapOne(ctx context.Context, owner, repo string) (int, int, error) {
	doc := SynthesizeDefault(owner)
	doc.Version = 1
	doc.UpdatedAt = s.nowUTC().Format(time.RFC3339)
	if _, err := store.PutBytes(ctx, s.Store, AccessKey(owner, repo), encodeAccess(doc),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		if store.IsPreconditionFailed(err) {
			return 0, 1, nil
		}
		return 0, 0, err
	}
	s.access.invalidate(owner, repo)
	return 1, 0, nil
}
