// activity_test.go — Forgejo #247 commit-date derivation (04_git.md §9.10).
// Happy path runs the real git binary (pinned identity via TestMain;
// explicit dates via env); failure arms prove R1 B3 (never fail the push).
package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func activityGit(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=T", "-c", "user.email=t@t"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
	), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestCommitDatesHappyPath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	activityGit(t, dir, nil, "init", "-b", "main", "-q")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	activityGit(t, dir, nil, "add", "f.txt")
	// Distinct author/committer dates pin the #142 preference order.
	env := []string{"GIT_AUTHOR_DATE=2026-01-02T03:04:05+00:00", "GIT_COMMITTER_DATE=2026-03-04T05:06:07+00:00"}
	activityGit(t, dir, env, "commit", "-qm", "dated")
	sha := activityGit(t, dir, nil, "rev-parse", "HEAD")

	l := NewLayer()
	repo := &LocalRepo{Path: dir}
	committer, author, err := l.CommitDates(ctx, repo, sha)
	if err != nil {
		t.Fatalf("CommitDates: %v", err)
	}
	wantC := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	wantA := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if !committer.Equal(wantC) {
		t.Fatalf("committer = %v, want %v", committer, wantC)
	}
	if !author.Equal(wantA) {
		t.Fatalf("author = %v, want %v", author, wantA)
	}
	if got, ok := PickCommitTime(committer, author); !ok || !got.Equal(wantC) {
		t.Fatalf("pick = %v %v, want commit_date %v", got, ok, wantC)
	}
}

func TestPickCommitTimeFallback(t *testing.T) {
	author := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if got, ok := PickCommitTime(time.Time{}, author); !ok || !got.Equal(author) {
		t.Fatalf("zero commit_date must fall back to author_date: %v %v", got, ok)
	}
	if _, ok := PickCommitTime(time.Time{}, time.Time{}); ok {
		t.Fatal("two zero dates must report !ok (record nulls)")
	}
	// Non-UTC input normalizes to UTC (sidecar/catalog are UTC RFC 3339).
	eastern := time.Date(2026, 5, 6, 12, 0, 0, 0, time.FixedZone("E", -4*3600))
	if got, ok := PickCommitTime(eastern, time.Time{}); !ok || got.Hour() != 16 {
		t.Fatalf("must normalize to UTC: %v %v", got, ok)
	}
}

func TestCommitDatesNeverFailsLoud(t *testing.T) {
	ctx := context.Background()
	l := NewLayer()
	dir := t.TempDir()
	activityGit(t, dir, nil, "init", "-b", "main", "-q")
	repo := &LocalRepo{Path: dir}
	for name, sha := range map[string]string{
		"garbage":   "not-a-sha",
		"zero sha1": strings.Repeat("0", 40),
		"absent":    strings.Repeat("a", 40),
	} {
		if _, _, err := l.CommitDates(ctx, repo, sha); err == nil {
			t.Fatalf("%s sha must error (caller degrades to nulls)", name)
		}
	}
	if _, _, err := l.CommitDates(ctx, nil, strings.Repeat("a", 40)); err == nil {
		t.Fatal("nil repo must error")
	}
	if _, _, err := l.CommitDates(ctx, &LocalRepo{}, strings.Repeat("a", 40)); err == nil {
		t.Fatal("empty repo path must error")
	}
}
