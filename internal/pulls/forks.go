package pulls

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// This file owns the fork list surface (§8 amendment, issue #424): the
// parent meta/forks.json index as a queryable relation. The index is the
// source of truth; the social counter derives from it (never independently
// maintained).

// ForkListEntry is one row of the live fork list.
type ForkListEntry struct {
	Repo     string `json:"repo"` // "owner/name" of the fork
	ForkedAt string `json:"forked_at"`
}

// ForkListPage is one GET …/forks page (index-first, like the PR list):
// entries in index order (fork-time ascending) after the cursor.
type ForkListPage struct {
	Forks   []ForkListEntry `json:"forks"`
	More    bool            `json:"more"`
	Version int             `json:"-"` // index version (the ETag stamp)
}

// MaxForkList bounds one forks page (the list-endpoint convention).
const MaxForkList = 100

// DefaultForkList is the default forks page size.
const DefaultForkList = 50

// ListForks returns the live fork list of owner/repo (read on the parent;
// absent index = no forks, never 404 — an unborn index is empty state, not
// an unknown repo; unknown repos fail at the read gate or serve empty).
func (s *Service) ListForks(ctx context.Context, owner, repo string, actor auth.Principal, after string, n int) (*ForkListPage, error) {
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	if n <= 0 {
		n = DefaultForkList
	}
	if n > MaxForkList {
		return nil, fmt.Errorf("%w: invalid n: max %d", ErrInvalid, MaxForkList)
	}
	if after != "" {
		ao, an, ok := strings.Cut(after, "/")
		if !ok || validateRepoPart(ao) != nil || validateRepoPart(an) != nil {
			return nil, fmt.Errorf("%w: invalid after: must be an owner/name cursor", ErrInvalid)
		}
	}
	raw, _, err := s.getJSON(ctx, ForksKey(owner, repo))
	if err != nil {
		return nil, err
	}
	fx := &ForksIndex{Forks: []ForkEntry{}}
	if raw != nil {
		var perr error
		fx, perr = parseForks(raw)
		if perr != nil {
			return nil, perr
		}
	}
	page := &ForkListPage{Forks: []ForkListEntry{}, Version: fx.Version}
	started := after == ""
	for _, f := range fx.Forks {
		if !started {
			if f.Repo == after {
				started = true
			}
			continue
		}
		if len(page.Forks) >= n+1 {
			break
		}
		page.Forks = append(page.Forks, ForkListEntry{Repo: f.Repo, ForkedAt: f.ForkedAt})
	}
	// The loop above collects n+1 rows when a further page exists; trim
	// the probe row and report it. (The break above stops one past the
	// page only when more rows remain — exactly the index-first pattern.)
	if len(page.Forks) > n {
		page.Forks = page.Forks[:n]
		page.More = true
	}
	return page, nil
}

// UnlistFork CAS-removes child ("owner/name") from the parent-side fork
// index (issue #457: the child-delete sweep — the caller invokes this only
// AFTER the child's manifest delete linearized, so a GC pass either sees
// the row and probes a 404 it already skips, or never sees the row at
// all). Idempotent: an absent index or an absent row returns (false, nil)
// with no write and no version bump (ETag-stable — a delete that was never
// a fork moves nothing). Returns true only when a row was actually
// removed; the caller decrements the social counter exactly then, so the
// counter cannot drift below the listed rows. A corrupt index errors (fail
// closed — the caller keeps the ghost row rather than guessing).
//
// ### Concurrency
//
// Hazard: a concurrent fork racing this unlist (CAS contention on the
// index). Avoidance: the canonical CAS loop — the 412 loser re-reads and
// converges (law 4 index-row discipline: Version++ only on an actual row
// removal). No lock is held (13 §2 rule 4 — pure store round trips).
func (s *Service) UnlistFork(ctx context.Context, owner, repo, child string) (bool, error) {
	removed := false
	_, err := s.casUpdate(ctx, ForksKey(owner, repo), 10, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		// Reset per attempt: only the landed write may report a removal.
		// A losing attempt that found the row (412, row gone on re-read)
		// must not report removed — otherwise two concurrent sweeps of
		// the same child would decrement the social counter twice for
		// one row (Registry.Delete is idempotent-success, so concurrent
		// double-deletes both reach the sweep).
		removed = false
		if cur == nil {
			return nil, false, nil
		}
		fx, perr := parseForks(cur)
		if perr != nil {
			return nil, false, perr
		}
		kept := make([]ForkEntry, 0, len(fx.Forks))
		found := false
		for _, f := range fx.Forks {
			if f.Repo == child {
				found = true
				continue
			}
			kept = append(kept, f)
		}
		if !found {
			return nil, false, nil
		}
		fx.Forks = kept
		fx.Version++
		out, _ := json.Marshal(fx)
		removed = true
		return out, true, nil
	})
	if err != nil {
		return false, err
	}
	return removed, nil
}
