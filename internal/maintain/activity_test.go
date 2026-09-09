// activity_test.go — Forgejo #247 cold-derivation resolver (R1 B5).
// resolveActivity over a fake engine: happy path, every failure arm
// (unknown repo, unborn HEAD, non-HEAD refs only, zero tip, git failure
// with and without serve-sync rescue, unusable dates), and the #142
// commit-first/author-fallback preference.
package maintain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

var actTip = strings.Repeat("a", 40)

var errActivityBoom = errors.New("boom")

func activityEngine(t *testing.T, tip string, committer, author time.Time) (*fakeEngine, *fakeRepo, *fakeGit) {
	t.Helper()
	g := &fakeGit{committerAt: committer, authorAt: author}
	r := &fakeRepo{
		id:  "o/a",
		dir: t.TempDir(),
		m:   &proto.Manifest{HeadSeq: 9},
		git: g,
		refsView: &RefsView{
			Seq:        9,
			HeadTarget: "refs/heads/main",
			Refs:       []git.RefEntry{{Name: "refs/heads/main", Oid: tip}},
		},
	}
	return newFakeEngine(nil, r), r, g
}

func TestResolveActivityHappyPath(t *testing.T) {
	ctx := context.Background()
	committer := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	author := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	eng, r, g := activityEngine(t, actTip, committer, author)
	sha, ct, found := resolveActivity(ctx, eng, "o/a")
	if !found || sha != actTip {
		t.Fatalf("resolve = %q %v %v", sha, ct, found)
	}
	if !ct.Equal(committer) {
		t.Fatalf("commit-first preference: %v", ct)
	}
	if g.dateCalls != 1 {
		t.Fatalf("date calls = %d, want exactly 1 (no serve-sync when local)", g.dateCalls)
	}
	if r.serveSyncs != 0 {
		t.Fatalf("serve syncs = %d, want 0", r.serveSyncs)
	}
}

func TestResolveActivityAuthorFallback(t *testing.T) {
	ctx := context.Background()
	author := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	eng, _, _ := activityEngine(t, actTip, time.Time{}, author)
	_, ct, found := resolveActivity(ctx, eng, "o/a")
	if !found || !ct.Equal(author) {
		t.Fatalf("zero commit_date must fall back: %v %v", ct, found)
	}
}

func TestResolveActivityFailureArms(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	names := []string{
		"unknown repo", "unborn HEAD", "non-HEAD refs only", "zero tip",
		"git failure, serve-sync fails", "git failure persists after serve-sync",
		"no usable dates",
	}
	for _, name := range names {
		eng, r, g := activityEngine(t, actTip, now, now)
		switch name {
		case "unborn HEAD":
			r.refsView = &RefsView{Seq: 9}
		case "non-HEAD refs only":
			r.refsView = &RefsView{Seq: 9, HeadTarget: "refs/heads/main",
				Refs: []git.RefEntry{{Name: "refs/tags/v1", Oid: actTip}}}
		case "zero tip":
			r.refsView = &RefsView{Seq: 9, HeadTarget: "refs/heads/main",
				Refs: []git.RefEntry{{Name: "refs/heads/main", Oid: strings.Repeat("0", 40)}}}
		case "git failure, serve-sync fails":
			g.dateErr = errActivityBoom
			r.syncServeErr = errActivityBoom
		case "git failure persists after serve-sync":
			g.dateErr = errActivityBoom
		case "no usable dates":
			g.committerAt, g.authorAt = time.Time{}, time.Time{}
		}
		id := "o/a"
		if name == "unknown repo" {
			id = "o/ghost"
		}
		if _, _, found := resolveActivity(ctx, eng, id); found {
			t.Fatalf("%s must resolve !found", name)
		}
	}
}

func TestResolveActivityHeadTargetFallsBackToLocalHEAD(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	// No checkpoint yet and no HEAD move in the log: the WAL view carries
	// refs but no HeadTarget (the production shape that hid the tip).
	eng, r, _ := activityEngine(t, actTip, now, now)
	r.refsView = &RefsView{
		Seq:  9,
		Refs: []git.RefEntry{{Name: "refs/heads/main", Oid: actTip}},
	}
	if err := os.WriteFile(filepath.Join(r.dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, ct, found := resolveActivity(ctx, eng, "o/a")
	if !found || sha != actTip || !ct.Equal(now) {
		t.Fatalf("local HEAD fallback = %q %v %v", sha, ct, found)
	}
	// Detached HEAD locally and none in the view → unknown, never an error.
	if err := os.WriteFile(filepath.Join(r.dir, "HEAD"), []byte(actTip+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, found := resolveActivity(ctx, eng, "o/a"); found {
		t.Fatal("detached HEAD must resolve !found")
	}
}

func TestReadHeadSymrefShapes(t *testing.T) {
	dir := t.TempDir()
	if got := readHeadSymref(dir); got != "" {
		t.Fatalf("missing HEAD = %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readHeadSymref(dir); got != "refs/heads/main" {
		t.Fatalf("symref = %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte(strings.Repeat("a", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readHeadSymref(dir); got != "" {
		t.Fatalf("detached = %q", got)
	}
}

func TestResolveActivityServeSyncRescue(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	eng, r, g := activityEngine(t, actTip, now, now)
	g.failDatesN = 1 // first read misses (objects not local), retry hits
	sha, ct, found := resolveActivity(ctx, eng, "o/a")
	if !found || sha != actTip || !ct.Equal(now) {
		t.Fatalf("rescue = %q %v %v", sha, ct, found)
	}
	if g.dateCalls != 2 {
		t.Fatalf("date calls = %d, want 2 (try + retry)", g.dateCalls)
	}
	if r.serveSyncs != 1 {
		t.Fatalf("serve syncs = %d, want exactly 1", r.serveSyncs)
	}
}
