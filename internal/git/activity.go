// activity.go — Forgejo #247 commit-date derivation (04_git.md §9.10).
//
// The publish path (via the server's WalEngine) and the maintainer sweep
// (via its Activity hook) both need one fact git knows and the bucket does
// not: a commit's dates. The recipe is a single light `log -1` in the repo
// that already holds the objects; derivation failure NEVER fails the
// caller (R1 B3) — it degrades to null activity, which the sweep heals.
package git

import (
	"context"
	"strings"
	"time"
)

// commitDatesFormat is the exact --format for the activity derivation
// (04_git.md §9.10, normative argv): committer date, NUL, author date.
// %x00 is ASCII text in argv — argv can never contain a NUL byte; git
// expands it (the same discipline as the §9.6 log recipe in internal/api).
const commitDatesFormat = "%cI%x00%aI"

// commitDatesTimeout bounds one derivation (a single local object read;
// pool-accounted like every other Layer exec).
const commitDatesTimeout = 30 * time.Second

// CommitDates runs `git log -1 --format=%cI%x00%aI <sha>` in repo and
// returns (committer date, author date). Either date may be zero when git
// emitted an unparseable value — use PickCommitTime for the #142 fallback.
// Errors (invalid sha, missing object, subprocess failure) are hard: the
// caller degrades to null activity and NEVER fails its own operation.
func (l *Layer) CommitDates(ctx context.Context, repo *LocalRepo, sha string) (time.Time, time.Time, error) {
	if repo == nil || repo.Path == "" {
		return time.Time{}, time.Time{}, errInvalidInput("commit dates: nil repo")
	}
	if !ValidOid(sha) || isZeroOid(sha) {
		return time.Time{}, time.Time{}, errInvalidInput("commit dates: bad sha %q", sha)
	}
	out, _, err := l.runCollect(ctx, execSpec{
		argv:    []string{"log", "-1", "--format=" + commitDatesFormat, sha},
		dir:     repo.Path,
		timeout: commitDatesTimeout,
	})
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	fields := strings.Split(strings.TrimRight(string(out), "\n"), "\x00")
	if len(fields) != 2 {
		return time.Time{}, time.Time{}, errInvalidInput("commit dates: %d fields for %s", len(fields), sha)
	}
	return parseGitDate(fields[0]), parseGitDate(fields[1]), nil
}

// parseGitDate parses one git %cI/%aI value (strict RFC 3339); garbage →
// zero (the fallback, not an error — a present-but-unparseable date must
// not fail the push).
func parseGitDate(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// PickCommitTime applies the #142 date semantic: commit_date first (when
// the commit landed in this repo's history — rebases/cherry-picks move
// it), author_date as fallback (original authorship). ok=false when both
// are zero (no usable date — record nulls).
func PickCommitTime(committer, author time.Time) (t time.Time, ok bool) {
	if !committer.IsZero() {
		return committer.UTC(), true
	}
	if !author.IsZero() {
		return author.UTC(), true
	}
	return time.Time{}, false
}
