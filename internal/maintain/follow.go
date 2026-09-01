// follow.go — the upstream-follow loop (§8): its own goroutine, NEVER a unit,
// every maintenance.follow_interval (30s; 0 = off). Ingress must not wait
// behind a base rebuild: separate cadence, no lease, no unit task slot — its
// only shared surface is the ordinary publish path (push-shaped work). The
// follow loop NEVER writes the maintainer heartbeat (§7).
package maintain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// defaultTokenEnv is the §8.2 credential fallback.
const defaultTokenEnv = "WALGIT_UPSTREAM_TOKEN"

// FollowRound is the per-instance last-round status (§8.5): in-memory, NOT
// the WAL; a host restart clears it and the describe surface omits it until
// the next round completes.
type FollowRound struct {
	At       time.Time         `json:"at"`
	Outcome  string            `json:"outcome"` // in-sync|published|refused|failed
	Detail   string            `json:"detail,omitempty"`
	Upstream map[string]string `json:"upstream,omitempty"`
	Ours     map[string]string `json:"ours,omitempty"`
}

// LastRound surfaces §8.5 status (GET …/settings/describe).
func (m *Maintainer) LastRound(repo string) (FollowRound, bool) {
	m.roundMu.Lock()
	defer m.roundMu.Unlock()
	r, ok := m.rounds[repo]
	if !ok || r == nil {
		return FollowRound{}, false
	}
	return *r, true
}

func (m *Maintainer) setRound(repo string, r FollowRound) {
	m.roundMu.Lock()
	defer m.roundMu.Unlock()
	m.rounds[repo] = &r
	m.metrics.recordFollow(repo, r.Outcome)
}

// RunFollow is the follow goroutine (§8): separate cadence from the unit
// pass; ctx cancel (drain) stops it at the next tick.
func (m *Maintainer) RunFollow(ctx context.Context) {
	if m.followInterval < 0 {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(m.followInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.followRoundAll(ctx)
		}
	}
}

func (m *Maintainer) followRoundAll(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			m.logf("follow round panicked: %v", r)
		}
	}()
	cfg := m.eng.HostConfig()
	if cfg == nil {
		return
	}
	for _, repo := range m.eng.Repos() {
		if ctx.Err() != nil {
			return
		}
		if !assigned(repo, cfg.Placement.Maintain, cfg.Placement.MaintainExclude) {
			continue
		}
		if err := m.followOnce(ctx, repo); err != nil && ctx.Err() == nil {
			m.logf("%s: follow round failed: %v", repo, err)
		}
	}
}

// followOnce is §8.1–§8.4 for one repo.
func (m *Maintainer) followOnce(ctx context.Context, repo string) error {
	round := func(outcome, detail string, tips, ours map[string]string) {
		r := FollowRound{At: m.now(), Outcome: outcome, Detail: detail, Upstream: tips, Ours: ours}
		m.setRound(repo, r)
		m.logf("%s follow outcome=%s (%s)", repo, outcome, detail)
	}
	cfg := m.eng.HostConfig()
	rep, err := m.eng.Open(ctx, repo)
	if err != nil {
		round("failed", err.Error(), nil, nil)
		return err
	}
	snap, _, err := m.loadRepo(ctx, repo, cfg)
	if err != nil {
		round("failed", err.Error(), nil, nil)
		return err
	}
	eff := snap.Eff
	if eff.Upstream.Git == "" || len(eff.Upstream.Follow) == 0 {
		return nil // nothing to follow
	}
	// §8.1 pack-set fit requirement: negotiation needs the object base — do
	// NOT half-negotiate against a partial copy.
	if !fits(eff, snap.Manifest) {
		round("failed", "pack set does not fit", nil, nil)
		return nil
	}
	ours, err := rep.RefValues(ctx)
	if err != nil {
		round("failed", err.Error(), nil, nil)
		return err
	}
	if m.opt.Follow == nil {
		round("failed", "follow fetcher not wired", nil, nil)
		return nil
	}
	tokenEnv := eff.Upstream.TokenEnv
	if tokenEnv == "" {
		tokenEnv = defaultTokenEnv
	}
	tips, err := m.opt.Follow.Fetch(ctx, repo, eff.Upstream.Git, tokenEnv, ours, eff.Upstream.Follow)
	if err != nil {
		round("failed", err.Error(), nil, ours)
		return err
	}

	// §8.3 compare: no ref moved → in-sync (the fetched pack is discarded by
	// the fetcher). Deleted upstream refs are left alone — deletion is a
	// human's call.
	var updates []*proto.RefUpdate
	refused := false
	for _, ref := range eff.Upstream.Follow {
		newOid, ok := tips[ref]
		if !ok || newOid == "" {
			continue
		}
		oldOid := ours[ref]
		if oldOid == newOid {
			continue
		}
		// Fast-forward only: old must be an ancestor of new. A rewound
		// upstream is refused and logged EVERY round until a human decides —
		// the refusal is not sticky-silenced (§8.3).
		if oldOid != "" && !isZeroHex(oldOid) {
			anc, err := m.opt.Follow.AncestorOf(ctx, repo, oldOid, newOid)
			if err != nil {
				round("failed", err.Error(), tips, ours)
				return err
			}
			if !anc {
				refused = true
				m.logf("%s: upstream rewound %s %s→%s; refusing (ff-only)", repo, ref, oldOid, newOid)
				continue
			}
		}
		updates = append(updates, &proto.RefUpdate{Name: ref, OldOid: oldOid, NewOid: newOid})
	}
	if len(updates) == 0 {
		if refused {
			round("refused", "upstream rewound; ff-only refusal", tips, ours)
		} else {
			round("in-sync", "", tips, ours)
		}
		return nil
	}
	// Follow publish: an ordinary PUSH entry — policy is NOT evaluated (§8.4:
	// follow is configuration, not a principal; the only way to stop
	// following a ref is to remove it from upstream.follow).
	txn := &proto.RefTransaction{Updates: updates, Atomic: true}
	meta := map[string]string{
		"principal": "upstream",
		"upstream":  eff.Upstream.Git,
		"agent":     "walgit follow",
	}
	if _, err := rep.PublishRefs(ctx, txn, meta); err != nil {
		round("failed", err.Error(), tips, ours)
		return err
	}
	detail := movedDetail(updates)
	if refused {
		round("refused", detail+"; rewound refs refused", tips, ours)
		return nil
	}
	round("published", detail, tips, ours)
	return nil
}

func movedDetail(updates []*proto.RefUpdate) string {
	parts := make([]string, 0, len(updates))
	for _, u := range updates {
		short := func(s string) string {
			if len(s) > 7 {
				return s[:7]
			}
			return s
		}
		parts = append(parts, fmt.Sprintf("%s %s→%s", u.Name, short(u.OldOid), short(u.NewOid)))
	}
	return strings.Join(parts, ", ")
}

func isZeroHex(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

// ---- the real fetcher (exec) -------------------------------------------------

// execFollow is the §8.2 persistent scratch implementation:
// <cache.dir>/follow/<owner>/<name>.git, bare, with objects/info/alternates →
// the serving objects dir so the delta fetch reuses local objects.
type execFollow struct {
	CacheDir string
	Binary   string
}

// Fetch stages ours into refs/follow (the delta-request base — this is what
// makes the fetch a delta request rather than a blind one), fetches with the
// exact §8.2 argv, and reads the tips back via for-each-ref.
func (f *execFollow) Fetch(ctx context.Context, repo, url, tokenEnv string, ours map[string]string, refs []string) (map[string]string, error) {
	id, err := git.ParseRepoId(repo)
	if err != nil {
		return nil, err
	}
	followDir := filepath.Join(f.CacheDir, "follow", id.Owner, id.Name+".git")
	if _, statErr := os.Stat(followDir); os.IsNotExist(statErr) {
		if _, err := git.InitLocalRepo(filepath.Join(f.CacheDir, "follow"), id, git.Sha1); err != nil {
			return nil, fmt.Errorf("follow scratch init: %w", err)
		}
		if err := os.MkdirAll(filepath.Join(followDir, "objects", "info"), 0o755); err != nil {
			return nil, err
		}
	}
	serving, err := git.OpenLocalRepo(f.CacheDir, id)
	if err != nil {
		return nil, err
	}
	if serving == nil {
		return nil, fmt.Errorf("follow: serving repo %s absent", id)
	}
	if err := os.WriteFile(filepath.Join(followDir, "objects", "info", "alternates"),
		[]byte(serving.ObjectsDir()+"\n"), 0o644); err != nil {
		return nil, err
	}

	// Stage WAL ref values (§8.2 step 2): git update-ref --stdin setting
	// refs/follow/<ref> to the WAL's current oids.
	var stdin bytes.Buffer
	stdin.WriteString("start\n")
	for _, r := range refs {
		if oid := ours[r]; oid != "" && !isZeroHex(oid) {
			fmt.Fprintf(&stdin, "update refs/follow/%s %s\n", r, oid)
		} else {
			fmt.Fprintf(&stdin, "delete refs/follow/%s\n", r)
		}
	}
	stdin.WriteString("prepare\ncommit\n")
	if _, err := f.git(ctx, followDir, nil, []string{"update-ref", "--stdin"}, stdin.Bytes()); err != nil {
		return nil, fmt.Errorf("stage refs/follow: %w", err)
	}

	// Fetch (§8.2 exact argv): unpackLimit=1 keeps the delta pack small;
	// always GIT_TERMINAL_PROMPT=0; token via config-pair credential helper
	// reading $<tokenEnv> (never on argv).
	if tokenEnv == "" {
		tokenEnv = defaultTokenEnv
	}
	env := []string{tokenEnv + "=" + os.Getenv(tokenEnv)}
	argv := []string{
		"-c", "fetch.unpackLimit=1", "-c", "transfer.unpackLimit=1",
		"-c", "fetch.writeCommitGraph=false", "-c", "gc.auto=0",
		"-c", "protocol.version=2",
		"-c", "credential.helper=",
		"-c", "credential.helper=!f(){ echo username=x-access-token; echo password=$WALGIT_UPSTREAM_TOKEN; };f",
		"fetch", "--no-tags", url,
	}
	for _, r := range refs {
		argv = append(argv, "+"+r+":refs/follow/"+r)
	}
	if _, err := f.git(ctx, followDir, env, argv, nil); err != nil {
		return nil, fmt.Errorf("follow fetch: %w", err)
	}

	out, err := f.git(ctx, followDir, nil, []string{"for-each-ref", "refs/follow/", "--format=%(objectname) %(refname)"}, nil)
	if err != nil {
		return nil, err
	}
	tips := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		oid, name, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if ref, ok := strings.CutPrefix(name, "refs/follow/"); ok {
			tips[ref] = oid
		}
	}
	// The fetched pack is trash (alternates make objects resolve anyway);
	// discard it so the next round re-fetches only what moved (§8.2).
	matches, _ := filepath.Glob(filepath.Join(followDir, "objects", "pack", "pack-*"))
	for _, match := range matches {
		_ = os.Remove(match)
	}
	return tips, nil
}

// AncestorOf runs `git merge-base --is-ancestor old new` in the follow
// scratch: exit 0 = ancestor (ff-ok), exit 1 = rewound.
func (f *execFollow) AncestorOf(ctx context.Context, repo, old, new string) (bool, error) {
	id, err := git.ParseRepoId(repo)
	if err != nil {
		return false, err
	}
	followDir := filepath.Join(f.CacheDir, "follow", id.Owner, id.Name+".git")
	_, err = f.git(ctx, followDir, nil, []string{"merge-base", "--is-ancestor", old, new}, nil)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return false, nil // exit 1: not an ancestor
	}
	return false, err
}

// git runs one command in the follow scratch with GIT_TERMINAL_PROMPT=0;
// ctx cancellation (drain) kills the subprocess.
func (f *execFollow) git(ctx context.Context, dir string, env []string, argv []string, stdin []byte) ([]byte, error) {
	binary := f.Binary
	if binary == "" {
		binary = "git"
	}
	cmd := exec.CommandContext(ctx, binary, argv...)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0", "GIT_DIR=" + dir}, env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return stdout.Bytes(), fmt.Errorf("git %s: %v: %s", strings.Join(argv, " "), err, tailLines(stderr.String(), 4))
	}
	return stdout.Bytes(), nil
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}
