package pulls

import (
	"context"
	"fmt"
	"strings"

	"git.packden.us/crueber/walhub/internal/server/auth"
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
