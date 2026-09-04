package social

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// This file owns the §§4–6 mutation policy: star/unstar idempotency, the
// field-scoped social.json CAS loops, viewer flags, and starred lists.

// Star creates the caller's star record (idempotent: repeat = same state)
// and CAS-increments the counter. Auth: authenticated AND repo-visible at
// read level (an anonymous_read repo is starrable by any signed-in user).
// A 412 on the record Create is "already starred" — no count change, so
// concurrent stars converge instead of double-counting. Starring a deleted
// repo is 404: without the manifest gate every star would mint a fresh
// userspace record the prefix sweep can never clean.
func (s *Service) Star(ctx context.Context, p auth.Principal, owner, repo string) (int, error) {
	if err := requireAuthenticated(p); err != nil {
		return 0, err
	}
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return 0, err
	}
	if !s.repoAlive(ctx, owner, repo) {
		return 0, fmt.Errorf("%w: repo %s not found", ErrNotFound, repoName(owner, repo))
	}
	who := normPrincipal(p.Name)
	key := StarKey(who, owner, repo)
	if raw, _, err := s.getJSON(ctx, key); err != nil {
		return 0, err
	} else if raw != nil {
		return s.reconcileStar(ctx, owner, repo)
	}
	rec, _ := json.Marshal(StarRecord{Repo: repoName(owner, repo), StarredAt: s.nowUTC().Format(dateTimeFmt)})
	if err := s.putCreate(ctx, key, rec); err != nil {
		if store.IsPreconditionFailed(err) {
			// Lost the record Create race: the concurrent Star owns
			// the bump — recount without repairing (the repair path
			// is only for records that pre-existed this call).
			return s.starCount(ctx, owner, repo), nil
		}
		return 0, err
	}
	return s.bumpStars(ctx, owner, repo, +1)
}

// reconcileStar serves the already-starred path. Healthy repos return the
// denormalized count untouched. A delete+recreate resets social.json while
// the userspace record survives the prefix sweep, so a record with an
// absent (or freshly zeroed) counter is repaired with a single +1 — the
// (c) resync. Only the zero state repairs: a nonzero count is assumed to
// include this star (no reverse index exists to verify — see Decisions),
// so a second stale starrer's star converges via unstar/restar instead.
// A corrupt counter keeps the old tolerance (record is the truth, 0).
func (s *Service) reconcileStar(ctx context.Context, owner, repo string) (int, error) {
	raw, _, err := s.getJSON(ctx, SocialKey(owner, repo))
	if err != nil {
		return 0, err
	}
	if raw == nil {
		return s.bumpStars(ctx, owner, repo, +1)
	}
	d, err := parseSocial(raw)
	if err != nil {
		return 0, nil
	}
	if d.Stars == 0 {
		return s.bumpStars(ctx, owner, repo, +1)
	}
	return d.Stars, nil
}

// Unstar deletes the caller's star record (idempotent) and CAS-decrements
// the counter (floor 0). Auth: authenticated only — unstar must always
// work, even on repos the principal can no longer see (§4). On a deleted
// repo the record delete still runs (cleanup) but the counter bump is
// skipped: bumping would lazily (re)create social.json for a repo the
// prefix sweep removed.
func (s *Service) Unstar(ctx context.Context, p auth.Principal, owner, repo string) (int, error) {
	if err := requireAuthenticated(p); err != nil {
		return 0, err
	}
	who := normPrincipal(p.Name)
	key := StarKey(who, owner, repo)
	raw, ver, err := s.getJSON(ctx, key)
	if err != nil {
		return 0, err
	}
	if raw == nil {
		return s.starCount(ctx, owner, repo), nil
	}
	// Version-conditional delete: the record is Create/Delete-only (never
	// rewritten, so no ABA) — success means THIS call removed the star and
	// owns the single decrement. A 412/404 means a concurrent unstar won
	// the removal (its decrement stands): recount, do NOT bump again.
	// (Unconditional deletes report success on absent keys on every
	// backend, so the version is what makes this exactly-once.)
	if derr := s.Store.Delete(ctx, key, ver); derr != nil {
		if store.IsNotFound(derr) || store.IsPreconditionFailed(derr) {
			return s.starCount(ctx, owner, repo), nil
		}
		return 0, derr
	}
	if !s.repoAlive(ctx, owner, repo) {
		return 0, nil
	}
	return s.bumpStars(ctx, owner, repo, -1)
}

// bumpStars CASes ONLY the stars field (watchers/forks/watcher_list pass
// through untouched — the §4.1 canonical loop). Decrements floor at 0.
func (s *Service) bumpStars(ctx context.Context, owner, repo string, delta int) (int, error) {
	var count int
	_, err := s.casUpdate(ctx, SocialKey(owner, repo), 8, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		var d SocialDoc
		if cur != nil {
			var perr error
			d, perr = parseSocialInto(cur)
			if perr != nil {
				return nil, false, perr
			}
		}
		d.Stars += delta
		if d.Stars < 0 {
			d.Stars = 0
		}
		d.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		count = d.Stars
		return encodeSocial(&d), true, nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// parseSocialInto decodes into a value (keeps bumpStars readable).
func parseSocialInto(raw []byte) (SocialDoc, error) {
	d, err := parseSocial(raw)
	if err != nil {
		return SocialDoc{}, err
	}
	return *d, nil
}

// starCount reads the denormalized count (0 when absent).
func (s *Service) starCount(ctx context.Context, owner, repo string) int {
	raw, _, err := s.getJSON(ctx, SocialKey(owner, repo))
	if err != nil || raw == nil {
		return 0
	}
	d, err := parseSocial(raw)
	if err != nil {
		return 0
	}
	return d.Stars
}

// Counts reads the denormalized counters (zeros when absent, §6). Read
// gate applies (counts are repo-visible metadata); a deleted repo is 404
// (the read gate alone would synthesize a public default and serve zeros
// for a ghost).
func (s *Service) Counts(ctx context.Context, p auth.Principal, owner, repo string) (SocialDoc, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return SocialDoc{}, err
	}
	if !s.repoAlive(ctx, owner, repo) {
		return SocialDoc{}, fmt.Errorf("%w: repo %s not found", ErrNotFound, repoName(owner, repo))
	}
	raw, _, err := s.getJSON(ctx, SocialKey(owner, repo))
	if err != nil {
		return SocialDoc{}, err
	}
	if raw == nil {
		return SocialDoc{}, nil
	}
	d, err := parseSocial(raw)
	if err != nil {
		return SocialDoc{}, err
	}
	return *d, nil
}

// ViewerState reports the caller's (starred, watching) flags for GET
// social's viewer object. Anonymous ⇒ both false (no error: the counts
// are still served under the read gate). A deleted repo ⇒ both false
// (miss-tolerant: the stale userspace records must not render).
func (s *Service) ViewerState(ctx context.Context, p auth.Principal, owner, repo string) (starred, watching bool) {
	if p.Anonymous || p.Name == "" {
		return false, false
	}
	if !s.repoAlive(ctx, owner, repo) {
		return false, false
	}
	who := normPrincipal(p.Name)
	if raw, _, err := s.getJSON(ctx, StarKey(who, owner, repo)); err == nil && raw != nil {
		starred = true
	}
	if raw, _, err := s.getJSON(ctx, WatchingKey(who, owner, repo)); err == nil && raw != nil {
		watching = true
	}
	return starred, watching
}

// Starred lists one principal's star records newest-first (n default 50,
// max 100; after is the "<starred_at>|<repo>" cursor). Entries naming a
// deleted repo are SKIPPED (miss-tolerant reads, §7 — the prefix sweep
// cannot enumerate userspace, so readers probe the manifest per entry;
// probe errors keep the entry). Corrupt records are skipped (they render
// nothing).
func (s *Service) Starred(ctx context.Context, principal string, n int, after string) ([]StarEntry, bool, error) {
	who := normPrincipal(principal)
	if who == "" {
		return nil, false, fmt.Errorf("%w: principal must not be empty", ErrInvalid)
	}
	if n <= 0 {
		n = ListDefaultPage
	}
	if n > ListMaxPage {
		n = ListMaxPage
	}
	var afterTime, afterRepo string
	if after != "" {
		ts, rp, ok := splitStarCursor(after)
		if !ok {
			return nil, false, fmt.Errorf("%w: malformed after cursor", ErrInvalid)
		}
		afterTime, afterRepo = ts, rp
	}
	prefix := StarredPrefix(who)
	var keys []string
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		keys = append(keys, m.Key)
		return nil
	}); err != nil {
		return nil, false, err
	}
	out := make([]StarEntry, 0, len(keys))
	for _, k := range keys {
		o, r, ok := splitStarKey(prefix, k)
		if !ok {
			continue
		}
		raw, _, err := s.getJSON(ctx, k)
		if err != nil || raw == nil {
			continue
		}
		rec, perr := parseStarRecord(raw)
		if perr != nil {
			continue
		}
		repo := rec.Repo
		if repo == "" {
			repo = o + "/" + r
		}
		if or, nm, ok := splitStarRepo(repo); ok {
			if !s.repoAlive(ctx, or, nm) {
				continue
			}
		}
		if after != "" && !starCursorAfter(rec.StarredAt, repo, afterTime, afterRepo) {
			continue
		}
		out = append(out, StarEntry{Repo: repo, StarredAt: rec.StarredAt})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StarredAt != out[j].StarredAt {
			return out[i].StarredAt > out[j].StarredAt
		}
		return out[i].Repo < out[j].Repo
	})
	var more bool
	if len(out) > n {
		out = out[:n]
		more = true
	}
	return out, more, nil
}

// splitStarCursor splits a "<starred_at>|<repo>" cursor on the FIRST "|"
// (the timestamp never contains one; repo names may).
func splitStarCursor(after string) (string, string, bool) {
	i := strings.Index(after, "|")
	if i < 0 {
		return "", "", false
	}
	ts, rp := after[:i], after[i+1:]
	if ts == "" || rp == "" {
		return "", "", false
	}
	if _, err := time.Parse(dateTimeFmt, ts); err != nil {
		return "", "", false
	}
	return ts, rp, true
}

// starCursorAfter orders newest-starred_at-first, repo ascending on ties.
func starCursorAfter(ts, repo, afterTime, afterRepo string) bool {
	if ts != afterTime {
		return ts < afterTime
	}
	return repo > afterRepo
}

// IncForks CAS-increments the parent's forks counter (§6: called from 03's
// fork completion step via the pulls.ForksCounter seam). ONLY the forks
// field moves; everything else passes through. The object is created
// lazily (zeros) on first mutation.
func (s *Service) IncForks(ctx context.Context, owner, repo string) error {
	_, err := s.casUpdate(ctx, SocialKey(owner, repo), 8, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		var d SocialDoc
		if cur != nil {
			var perr error
			d, perr = parseSocialInto(cur)
			if perr != nil {
				return nil, false, perr
			}
		}
		d.Forks++
		d.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		return encodeSocial(&d), true, nil
	})
	return err
}
