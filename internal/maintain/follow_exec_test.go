// follow_exec_test.go — the real execFollow fetcher against a local upstream
// work repo (§8.2): stage-refs → delta fetch → tips, ff ancestry, and the
// exec error paths.
package maintain

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// upstreamFixture builds a local work repo with two commits and returns its
// path plus the two tip oids.
func upstreamFixture(t *testing.T) (string, string, string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "upstream")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(argv ...string) string {
		cmd := exec.Command("git", argv...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(argv, " "), err, out)
		}
		return string(out)
	}
	run("init", "-q", "-b", "main", ".")
	run("commit", "-q", "--allow-empty", "-m", "one")
	out1 := strings.TrimSpace(run("rev-parse", "HEAD"))
	run("commit", "-q", "--allow-empty", "-m", "two")
	out2 := strings.TrimSpace(run("rev-parse", "HEAD"))
	return work, out1, out2
}

// syncServing fetches the upstream tips into the serving repo's main ref so
// the follow scratch's alternates resolve the staged "ours" objects.
func syncServing(t *testing.T, serving, upstream string) {
	t.Helper()
	cmd := exec.Command("git", "-C", serving, "fetch", "-q", upstream, "refs/heads/main:refs/heads/main")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("serving sync: %v: %s", err, out)
	}
}

func TestExecFollow_FetchAndAncestry(t *testing.T) {
	upstream, oid1, oid2 := upstreamFixture(t)
	cache := t.TempDir()
	id, err := git.ParseRepoId("acme/widget")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := git.InitLocalRepo(cache, id, git.Sha1); err != nil {
		t.Fatalf("serving init: %v", err)
	}
	f := &execFollow{CacheDir: cache, Binary: "git"}
	ctx := context.Background()

	// Round 1: no local state (zero oid) → deletion staged, plain fetch.
	tips, err := f.Fetch(ctx, "acme/widget", upstream, "WALGIT_UPSTREAM_TOKEN",
		map[string]string{"refs/heads/main": git.Sha1.ZeroHex()}, []string{"refs/heads/main"})
	if err != nil {
		t.Fatalf("fetch 1: %v", err)
	}
	if tips["refs/heads/main"] != oid2 {
		t.Fatalf("tips = %v, want main=%s", tips, oid2)
	}
	// The fetched pack is trash and must be discarded (§8.2).
	matches, _ := filepath.Glob(filepath.Join(cache, "follow", "acme", "widget.git", "objects", "pack", "pack-*"))
	if len(matches) != 0 {
		t.Fatalf("pack files kept: %v", matches)
	}

	// The serving copy holds oid1, so the follow scratch's alternates
	// resolve the staged base object (§8.2 alternates rule).
	syncServing(t, id.LocalDir(cache), upstream)
	// Round 2: ours staged as the delta base (refs/follow holds oid1).
	if _, err := f.Fetch(ctx, "acme/widget", upstream, "", map[string]string{"refs/heads/main": oid1},
		[]string{"refs/heads/main"}); err != nil {
		t.Fatalf("fetch 2: %v", err)
	}
	staged, err := os.ReadFile(filepath.Join(cache, "follow", "acme", "widget.git", "objects", "info", "alternates"))
	if err != nil || !strings.Contains(string(staged), "objects") {
		t.Fatalf("alternates = %q err=%v", staged, err)
	}

	// Ancestry: oid1 is an ancestor of oid2 (ff-ok); reversed is rewound.
	if ff, err := f.AncestorOf(ctx, "acme/widget", oid1, oid2); err != nil || !ff {
		t.Fatalf("ancestor old→new: %v %v", ff, err)
	}
	if ff, err := f.AncestorOf(ctx, "acme/widget", oid2, oid1); err != nil || ff {
		t.Fatalf("ancestor new→old: %v %v", ff, err)
	}
}

func TestExecFollow_Errors(t *testing.T) {
	upstream, _, _ := upstreamFixture(t)
	cache := t.TempDir()
	f := &execFollow{CacheDir: cache, Binary: "git"}
	ctx := context.Background()

	// Malformed repo id.
	if _, err := f.Fetch(ctx, "notanid", upstream, "", nil, nil); err == nil {
		t.Fatal("bad repo id must fail")
	}
	// Serving repo absent.
	if _, err := f.Fetch(ctx, "acme/widget", upstream, "", nil, []string{"refs/heads/main"}); err == nil {
		t.Fatal("absent serving repo must fail")
	}
	// Invalid oid in ours fails the update-ref staging.
	id, _ := git.ParseRepoId("acme/widget")
	if _, err := git.InitLocalRepo(cache, id, git.Sha1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Fetch(ctx, "acme/widget", upstream, "", map[string]string{"refs/heads/main": "zz"},
		[]string{"refs/heads/main"}); err == nil {
		t.Fatal("invalid staged oid must fail")
	}
	// Unreachable upstream fails the fetch itself.
	if _, err := f.Fetch(ctx, "acme/widget", filepath.Join(cache, "nowhere"), "", nil,
		[]string{"refs/heads/main"}); err == nil {
		t.Fatal("unreachable upstream must fail")
	}
	// A canceled context surfaces as ctx.Err, not a git error.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.git(cctx, cache, nil, []string{"version"}, nil); err == nil {
		t.Fatal("canceled ctx must fail")
	}
	// AncestorOf with a malformed id errors too.
	if _, err := f.AncestorOf(ctx, "bad", "a", "b"); err == nil {
		t.Fatal("bad repo id must fail")
	}
}

func TestTailLines(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"a\nb\nc\nd\ne\n", 2, "d; e"},
		{"a\nb", 5, "a; b"},
		{"", 3, ""},
		{"x", 1, "x"},
	}
	for _, tt := range tests {
		if got := tailLines(tt.in, tt.n); got != tt.want {
			t.Fatalf("tailLines(%q,%d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
}

func TestExecFollow_FetchRealRound(t *testing.T) {
	// A full follow round with the real fetcher: the fake-repo engine drives
	// followOnce; the fetcher talks to a local upstream over real git.
	upstream, oid1, oid2 := upstreamFixture(t)
	cache := t.TempDir()
	id, _ := git.ParseRepoId("acme/widget")
	if _, err := git.InitLocalRepo(cache, id, git.Sha1); err != nil {
		t.Fatal(err)
	}
	f := &execFollow{CacheDir: cache, Binary: "git"}
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	syncServing(t, id.LocalDir(cache), upstream)
	eff.Compaction.Enabled = false
	eff.Maintenance.FollowInterval = -1
	eff.Cache.Dir = cache
	eff.Upstream.Git = upstream
	eff.Upstream.Follow = []string{"refs/heads/main"}

	repo := &fakeRepo{
		id:   "acme/widget",
		dir:  filepath.Join(cache, "acme", "widget.git"),
		m:    &proto.Manifest{Repo: "acme/widget", HeadSeq: 5},
		refs: map[string]string{"refs/heads/main": oid1},
	}
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}, Follow: f})
	m.followOnce(context.Background(), "acme/widget")

	round, ok := m.LastRound("acme/widget")
	if !ok {
		t.Fatal("no round recorded")
	}
	if round.Outcome != "published" {
		t.Fatalf("outcome = %q (detail %q)", round.Outcome, round.Detail)
	}
	if len(repo.published) != 1 {
		t.Fatalf("publishes = %+v", repo.published)
	}
	// The upstream tip was published as the new ref value.
	if repo.refs["refs/heads/main"] != oid2 {
		t.Fatalf("published refs = %v, want main→%s", repo.refs, oid2)
	}
}
