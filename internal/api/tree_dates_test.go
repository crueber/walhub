// tree_dates_test.go pins issue #301: per-entry last-commit dates from ONE
// batched `git log` walk. Pure table tests cover the walk-output → per-entry
// map (assignment, renames-as-add/delete, cap/exhaustion, submodules,
// empty); the integration test runs the real argv against a real repo with
// distinct commit dates.
package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/wal"
)

// logTouch is one status→path touch inside a synthetic walk commit.
type logTouch struct {
	status string // single letter, as --no-renames emits
	path   string
}

// encodeTreeLog renders synthetic treeLogArgv -z stdout: per commit,
// "MARK <sha> <date>" NUL, "\n", then alternating status/path records.
func encodeTreeLog(t *testing.T, commits []struct {
	sha, date string
	touches   []logTouch
}) []byte {
	t.Helper()
	var b strings.Builder
	for _, c := range commits {
		b.WriteString(treeLogMarker + " " + c.sha + " " + c.date + "\x00\n")
		for _, tc := range c.touches {
			b.WriteString(tc.status + "\x00" + tc.path + "\x00")
		}
	}
	return []byte(b.String())
}

func TestTreeLogArgv(t *testing.T) {
	sha := strings.Repeat("a", 40)
	root := treeLogArgv(sha, "", 200)
	wantRoot := []string{"log", "--format=" + treeLogMarker + " %H %cI",
		"--name-status", "--no-renames", "--no-color", "--first-parent",
		"-z", "--max-count=200", sha}
	if strings.Join(root, "\x00") != strings.Join(wantRoot, "\x00") {
		t.Fatalf("root argv = %q, want %q", root, wantRoot)
	}
	sub := treeLogArgv(sha, "docs", 50)
	if sub[len(sub)-2] != "--" || sub[len(sub)-1] != "docs" {
		t.Fatalf("subdir argv must end in -- docs: %q", sub)
	}
	joined := strings.Join(sub, " ")
	for _, f := range []string{"--no-renames", "--first-parent", "-z", "--max-count=50", "--no-color"} {
		if !strings.Contains(joined, f) {
			t.Fatalf("argv must pin %q: %q", f, sub)
		}
	}
}

func TestTreeLogCap(t *testing.T) {
	v := &walView{}
	if v.treeLogCap() != defaultMaxTreeLog {
		t.Fatalf("zero walView cap = %d, want default %d", v.treeLogCap(), defaultMaxTreeLog)
	}
	v.maxTreeLog = -3
	if v.treeLogCap() != defaultMaxTreeLog {
		t.Fatalf("negative cap = %d, want default %d", v.treeLogCap(), defaultMaxTreeLog)
	}
	v.maxTreeLog = 7
	if v.treeLogCap() != 7 {
		t.Fatalf("explicit cap = %d, want 7", v.treeLogCap())
	}
}

func TestAssignTreeCommitMeta(t *testing.T) {
	sha1, sha2, sha3 := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40)
	d1, d2, d3 := "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z"
	entries := func() []TreeEntry {
		return []TreeEntry{
			{Name: "a.txt", Type: "blob", Mode: "100644", Size: 3, SHA: "x"},
			{Name: "sub", Type: "tree", Mode: "040000", Size: -1, SHA: "y"},
			{Name: "vendor", Type: "commit", Mode: "160000", Size: -1, SHA: "z"},
		}
	}
	t.Run("newest wins, dirs via nested paths, submodules dateless", func(t *testing.T) {
		es := entries()
		out := encodeTreeLog(t, []struct {
			sha, date string
			touches   []logTouch
		}{
			{sha3, d3, []logTouch{{"M", "a.txt"}}},
			{sha2, d2, []logTouch{{"M", "sub/nested/deep.txt"}}},
			{sha1, d1, []logTouch{{"A", "a.txt"}, {"A", "vendor"}}},
		})
		assignTreeCommitMeta(es, "", out)
		if es[0].CommitSHA != sha3 || es[0].CommitTime != d3 {
			t.Fatalf("a.txt = %q %q, want newest %q %q", es[0].CommitSHA, es[0].CommitTime, sha3, d3)
		}
		if es[1].CommitSHA != sha2 || es[1].CommitTime != d2 {
			t.Fatalf("sub = %q %q, want %q %q", es[1].CommitSHA, es[1].CommitTime, sha2, d2)
		}
		if es[2].CommitSHA != "" || es[2].CommitTime != "" {
			t.Fatalf("vendor submodule must stay dateless: %+v", es[2])
		}
	})
	t.Run("subdir listing maps the prefix", func(t *testing.T) {
		es := []TreeEntry{{Name: "b.txt", Type: "blob"}, {Name: "c.txt", Type: "blob"}}
		out := encodeTreeLog(t, []struct {
			sha, date string
			touches   []logTouch
		}{
			{sha2, d2, []logTouch{{"M", "docs/b.txt"}, {"M", "other/b.txt"}}},
		})
		assignTreeCommitMeta(es, "docs", out)
		if es[0].CommitSHA != sha2 {
			t.Fatalf("docs/b.txt must attribute b.txt, got %+v", es[0])
		}
		if es[1].CommitSHA != "" {
			t.Fatalf("c.txt untouched must stay undated: %+v", es[1])
		}
	})
	t.Run("unassigned entries stay undated past the walk", func(t *testing.T) {
		es := entries()
		out := encodeTreeLog(t, []struct {
			sha, date string
			touches   []logTouch
		}{
			{sha1, d1, []logTouch{{"M", "elsewhere.txt"}}},
		})
		assignTreeCommitMeta(es, "", out)
		for _, e := range es {
			if e.CommitSHA != "" || e.CommitTime != "" {
				t.Fatalf("untouched entry must stay undated: %+v", e)
			}
		}
	})
	t.Run("empty and malformed input degrades to undated", func(t *testing.T) {
		for name, out := range map[string][]byte{
			"empty":    {},
			"garbage":  []byte("not a log walk at all\n"),
			"bad date": []byte(treeLogMarker + " " + sha1 + " not-a-date\x00\nM\x00a.txt\x00"),
			"bad sha":  []byte(treeLogMarker + "  " + d1 + "\x00\nM\x00a.txt\x00"),
		} {
			es := entries()
			assignTreeCommitMeta(es, "", out)
			for _, e := range es {
				if e.CommitSHA != "" || e.CommitTime != "" {
					t.Fatalf("%s: must degrade to undated: %+v", name, e)
				}
			}
		}
	})
	t.Run("all-submodule and empty listings skip the walk", func(t *testing.T) {
		es := []TreeEntry{{Name: "vendor", Type: "commit"}}
		out := encodeTreeLog(t, []struct {
			sha, date string
			touches   []logTouch
		}{
			{sha1, d1, []logTouch{{"M", "vendor"}}},
		})
		assignTreeCommitMeta(es, "", out)
		if es[0].CommitSHA != "" {
			t.Fatalf("submodule-only listing must stay dateless: %+v", es[0])
		}
		assignTreeCommitMeta(nil, "", out) // must not panic
	})
	t.Run("single-char paths stay unambiguous", func(t *testing.T) {
		es := []TreeEntry{{Name: "M", Type: "blob"}}
		out := encodeTreeLog(t, []struct {
			sha, date string
			touches   []logTouch
		}{
			{sha1, d1, []logTouch{{"M", "M"}}},
		})
		assignTreeCommitMeta(es, "", out)
		if es[0].CommitSHA != sha1 || es[0].CommitTime != d1 {
			t.Fatalf("1-char path misattributed: %+v", es[0])
		}
	})
	t.Run("status letters all attribute", func(t *testing.T) {
		es := []TreeEntry{
			{Name: "a", Type: "blob"}, {Name: "b", Type: "blob"},
			{Name: "c", Type: "blob"}, {Name: "d", Type: "tree"},
		}
		out := encodeTreeLog(t, []struct {
			sha, date string
			touches   []logTouch
		}{
			{sha1, d1, []logTouch{{"A", "a"}, {"D", "b"}, {"T", "c"}, {"M", "d/f"}}},
		})
		assignTreeCommitMeta(es, "", out)
		for _, e := range es {
			if e.CommitSHA != sha1 {
				t.Fatalf("status-letter touch must attribute: %+v", e)
			}
		}
	})
}

// commitFileAt writes name and commits it with an explicit author/committer
// date (runGit pins one date for the whole fixture; per-entry dates need
// distinct stamps).
func commitFileAt(t *testing.T, dir, name, body, date string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date,
	)
	add := exec.Command("git", "add", name)
	add.Dir, add.Env = dir, env
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("git add %s: %v %s", name, err, out)
	}
	com := exec.Command("git", "commit", "-m", name)
	com.Dir, com.Env = dir, env
	if out, err := com.CombinedOutput(); err != nil {
		t.Fatalf("git commit %s: %v %s", name, err, out)
	}
	rev := exec.Command("git", "rev-parse", "HEAD")
	rev.Dir, rev.Env = dir, env
	out, err := rev.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// TestTreeDatesIntegration runs the real pinned argv over a real repo:
// distinct per-commit dates land on the right entries, the gitlink stays
// dateless, subdir listings map, and a cap of 1 leaves cold entries
// undated instead of stalling.
func TestTreeDatesIntegration(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	barePath := filepath.Join(dir, "walgit.git")
	runGit(t, dir, "init", "-b", "main", work)
	runGit(t, dir, "init", "--bare", "-b", "main", barePath)
	d1, d2, d3 := "2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z", "2026-04-01T00:00:00Z"
	commitFileAt(t, work, "a.txt", "one\n", d1)
	commitFileAt(t, work, "sub/b.txt", "b\n", d1)
	c2 := commitFileAt(t, work, "a.txt", "two\n", d2)
	// submodule entry: a gitlink the walk touches but the assigner must skip.
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE="+d3, "GIT_COMMITTER_DATE="+d3,
	)
	addSub := exec.Command("git", "update-index", "--add", "--cacheinfo", "160000,"+c2+",vendor")
	addSub.Dir, addSub.Env = work, env
	if out, err := addSub.CombinedOutput(); err != nil {
		t.Fatalf("update-index gitlink: %v %s", err, out)
	}
	comSub := exec.Command("git", "commit", "-m", "vendor")
	comSub.Dir, comSub.Env = work, env
	if out, err := comSub.CombinedOutput(); err != nil {
		t.Fatalf("commit gitlink: %v %s", err, out)
	}
	head := runGit(t, work, "rev-parse", "HEAD")
	runGit(t, work, "push", barePath, "main")
	bare := &git.LocalRepo{Root: dir, ID: git.RepoId{Owner: "demo", Name: "walgit"}, Path: barePath}

	eng := &fakeEngine{obj: wal.ObjectAccess{Local: bare}, rev: 1}
	f := newEngineFixture(t, eng)
	v := f.view()
	ctx := context.Background()
	id := repoID()

	tr, err := v.Tree(ctx, id, head, "")
	if err != nil {
		t.Fatalf("root tree: %v", err)
	}
	byName := map[string]TreeEntry{}
	for _, e := range tr.Entries {
		byName[e.Name] = e
	}
	if got := byName["a.txt"]; got.CommitSHA != c2 || got.CommitTime != d2 {
		t.Fatalf("a.txt = %q %q, want %q %q", got.CommitSHA, got.CommitTime, c2, d2)
	}
	if got := byName["sub"]; got.CommitTime != d1 {
		t.Fatalf("sub = %q %q, want date %q", got.CommitSHA, got.CommitTime, d1)
	}
	if got := byName["vendor"]; got.Type != "commit" || got.CommitSHA != "" || got.CommitTime != "" {
		t.Fatalf("vendor gitlink must stay dateless: %+v", got)
	}

	sub, err := v.Tree(ctx, id, head, "sub")
	if err != nil {
		t.Fatalf("sub tree: %v", err)
	}
	if len(sub.Entries) != 1 || sub.Entries[0].CommitTime != d1 {
		t.Fatalf("sub/b.txt = %+v, want date %q", sub.Entries, d1)
	}

	// Cap of 1: only the vendor commit is walked — the cold entries stay
	// undated instead of stalling the response.
	capped := &walView{engine: eng, layer: git.NewLayer(), binary: "git", maxTreeLog: 1}
	ctr, err := capped.Tree(ctx, id, head, "")
	if err != nil {
		t.Fatalf("capped tree: %v", err)
	}
	for _, e := range ctr.Entries {
		if e.CommitSHA != "" || e.CommitTime != "" {
			t.Fatalf("cap=1 must leave %q undated: %+v", e.Name, e)
		}
	}
}

// TestStampTreeDatesDegraded pins the fail-open contract: a failed walk
// leaves entries undated and never errors (the tree already rendered).
func TestStampTreeDatesDegraded(t *testing.T) {
	fix := newGitFix(t)
	eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 1}
	v := newEngineFixture(t, eng).view()
	entries := []TreeEntry{{Name: "hello.txt", Type: "blob"}}
	v.stampTreeDates(context.Background(), fix.bare, strings.Repeat("0", 40), "", entries)
	if entries[0].CommitSHA != "" || entries[0].CommitTime != "" {
		t.Fatalf("failed walk must degrade to undated: %+v", entries[0])
	}
}
