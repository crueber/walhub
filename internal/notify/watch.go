// watch.go — repo watches (07 §§4–5 shapes, written here until 07
// lands; 06 owns notifications, never the social shape beyond the
// watcher_list it consumes).
//
// Record: users/<principal>/watching/<o>/<r>.json (Create/Delete,
// idempotent). Count + watcher_list: repos/<o>/<r>/meta/social.json
// (CAS loop mutating ONLY the watch fields — the same loop discipline
// 07 §4.1 prescribes, so a concurrently landing 07 converges).
package notify

import (
	"context"
	"encoding/json"
	"fmt"

	"git.packden.us/crueber/walhub/internal/store"
)

// WatchState is the GET watch response.
type WatchState struct {
	Watching bool `json:"watching"`
	Watchers int  `json:"watchers"`
}

// GetWatch reports whether principal watches the repo (self only,
// no-store). Absent social.json counts zero watchers. A deleted repo
// reports not-watching with zero watchers (miss-tolerant: the stale
// userspace record must not render).
func (s *Service) GetWatch(ctx context.Context, principal, owner, repo string) WatchState {
	if !s.repoAlive(ctx, owner, repo) {
		return WatchState{Watching: false, Watchers: 0}
	}
	raw, _, err := s.getJSON(ctx, WatchingKey(principal, owner, repo))
	if err != nil || raw == nil {
		return WatchState{Watching: false, Watchers: s.watcherCount(ctx, owner, repo)}
	}
	return WatchState{Watching: true, Watchers: s.watcherCount(ctx, owner, repo)}
}

// SetWatch creates (on=true) or deletes (on=false) the watch record and
// CASes the social counters. Idempotent both ways. Watching a deleted
// repo is 404 (no fresh userspace records for ghosts); unwatching one
// still deletes the record (cleanup) but skips the counter CAS so no
// social.json is resurrected for a swept repo.
func (s *Service) SetWatch(ctx context.Context, principal, owner, repo string, on bool) (WatchState, error) {
	principal = normPrincipal(principal)
	key := WatchingKey(principal, owner, repo)
	if on {
		if !s.repoAlive(ctx, owner, repo) {
			return WatchState{}, fmt.Errorf("%w: repo %s/%s not found", ErrNotFound, owner, repo)
		}
		rec := WatchRecord{Repo: owner + "/" + repo, WatchedAt: s.nowUTC().Format(dateTimeFmt)}
		if err := s.putCreate(ctx, key, encode(rec)); err != nil {
			if !store.IsPreconditionFailed(err) {
				return WatchState{}, err
			}
		}
		if err := s.socialWatch(ctx, owner, repo, principal, true); err != nil {
			return WatchState{}, err
		}
	} else {
		_ = s.Store.Delete(ctx, key, "")
		if !s.repoAlive(ctx, owner, repo) {
			return WatchState{Watching: false, Watchers: 0}, nil
		}
		if err := s.socialWatch(ctx, owner, repo, principal, false); err != nil {
			return WatchState{}, err
		}
	}
	return WatchState{Watching: on, Watchers: s.watcherCount(ctx, owner, repo)}, nil
}

// socialWatch CASes ONLY the watch fields of social.json (stars/forks
// pass through untouched — the 07 §4.1 canonical loop). Beyond
// MaxWatchers the list caps and truncates (the record still stands;
// over-cap watches notify no one, §2).
func (s *Service) socialWatch(ctx context.Context, owner, repo, principal string, on bool) error {
	p := normPrincipal(principal)
	_, err := s.casUpdate(ctx, SocialKey(owner, repo), 8, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var soc SocialDoc
		if cur != nil {
			if err := json.Unmarshal(cur, &soc); err != nil {
				return nil, false, fmt.Errorf("%w: social: %v", ErrInvalid, err)
			}
		}
		have := false
		for _, w := range soc.WatcherList {
			if w == p {
				have = true
				break
			}
		}
		if on && have {
			return nil, false, nil
		}
		if !on && !have {
			// Reconcile the count downward when the list disagrees
			// (drift repair, same spirit as the tray reconcile).
			want := len(soc.WatcherList)
			if soc.Watchers != want {
				soc.Watchers = want
				soc.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
				return encode(soc), true, nil
			}
			return nil, false, nil
		}
		if on {
			soc.WatcherList = append(soc.WatcherList, p)
			sortStrings(soc.WatcherList)
			if len(soc.WatcherList) > MaxWatchers {
				soc.WatcherList = soc.WatcherList[:MaxWatchers]
				soc.WatchersTruncated = true
			}
		} else {
			next := soc.WatcherList[:0]
			for _, w := range soc.WatcherList {
				if w != p {
					next = append(next, w)
				}
			}
			soc.WatcherList = next
			if len(soc.WatcherList) < MaxWatchers {
				soc.WatchersTruncated = false
			}
		}
		soc.Watchers = len(soc.WatcherList)
		soc.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		return encode(soc), true, nil
	})
	return err
}

// watcherCount reads the denormalized count (0 when absent).
func (s *Service) watcherCount(ctx context.Context, owner, repo string) int {
	raw, _, err := s.getJSON(ctx, SocialKey(owner, repo))
	if err != nil || raw == nil {
		return 0
	}
	var soc SocialDoc
	if err := json.Unmarshal(raw, &soc); err != nil {
		return 0
	}
	return soc.Watchers
}
