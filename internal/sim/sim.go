// sim.go — the sim harness: Cluster (one truth store, N instance links),
// Instance (link + registry + handle with crash-boundary wrappers), the
// retrying pusher, the truth oracle, and the §4.9 lease helper.
package sim

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/fault"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// defaultRepo is the one repo every sim cluster shares.
const defaultRepo = "acme/api"

// defaultSeeds is the seed set when neither WALHUB_SIM_SEED nor
// WALHUB_SIM_SEEDS is set: 22 and neighbors (15_testing.md §4 row 12).
var defaultSeeds = []uint64{22, 21, 23}

// maxPushAttempts bounds the pusher's conflict/transient retry loop; hitting
// it means the core is not converging and the test must fail, not spin.
const maxPushAttempts = 200

// simVerbose enables per-attempt pusher logging (WALHUB_SIM_VERBOSE=1): the
// failure artifact for a starved pusher. Off by default (hot loop).
func simVerbose() bool { return os.Getenv("WALHUB_SIM_VERBOSE") != "" }

// unwrapAll renders the full causal chain of an error (WalError.Error hides
// the wrapped store fault, which is what a starvation diagnosis needs).
func unwrapAll(err error) string {
	var parts []string
	for err != nil {
		parts = append(parts, err.Error())
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return strings.Join(parts, " <- ")
}

// simSeeds resolves the seed list: WALHUB_SIM_SEEDS (comma-separated) wins
// over WALHUB_SIM_SEED (single), which wins over the default set.
func simSeeds() ([]uint64, error) {
	if s := os.Getenv("WALHUB_SIM_SEEDS"); s != "" {
		return parseSeedList(s)
	}
	if s := os.Getenv("WALHUB_SIM_SEED"); s != "" {
		return parseSeedList(s)
	}
	return append([]uint64{}, defaultSeeds...), nil
}

// parseSeedList parses "22" or "21,22,23" into seeds; empty entries and
// non-numbers are errors (a misconfigured harness must fail loudly, never
// silently run the defaults).
func parseSeedList(s string) ([]uint64, error) {
	var out []uint64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("sim: bad seed list %q: empty entry", s)
		}
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("sim: bad seed list %q: %v", s, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// linkSeed derives a link's FaultStore seed deterministically from the run
// seed and the link index, so one seed reproduces the whole cluster.
func linkSeed(seed uint64, idx int) uint64 {
	return seed*0x9E3779B97F4A7C15 + uint64(idx)
}

// repoManifestKey is the truth-store key of the repo manifest.
func repoManifestKey(repo string) string {
	return "repos/" + repo + "/manifest.pb"
}

// refTxn builds a single-update ref transaction.
func refTxn(ref, old, new string) *proto.RefTransaction {
	return &proto.RefTransaction{Updates: []*proto.RefUpdate{{Name: ref, OldOid: old, NewOid: new}}}
}

// oidFor derives a deterministic 40-hex oid from (seed, pusher, push); never
// the zero oid (zero means "absent" in txns).
func oidFor(seed uint64, pusher, push int) string {
	return fmt.Sprintf("%040x", seed*uint64(0x1000003)+uint64(pusher)*0x1000003+uint64(push)+1)
}

// simConfig is a registry config for one instance: isolated cache dir, short
// batch window, no freshness TTL (every sync checks — the sim counts the
// check; the TTL-skip case sets FreshnessTTL explicitly).
func simConfig(cacheDir string) *config.Config {
	cfg := config.Defaults()
	cfg.Cache.Dir = cacheDir
	cfg.WAL.BatchWindow = config.Duration(5 * time.Millisecond)
	cfg.WAL.FreshnessTTL = 0
	return cfg
}

// Cluster is N instances sharing one truth store (memory) and one repo.
// Each instance sees the truth only through its own FaultStore link.
type Cluster struct {
	t         *testing.T
	ctx       context.Context
	cancel    context.CancelFunc
	truth     store.ObjectStore
	repo      string
	root      string // parent dir for per-instance cache dirs
	created   bool
	instances []*Instance
}

// newCluster builds a cluster over a fresh memory truth store. Close (via
// t.Cleanup) shuts every registry down.
func newCluster(t *testing.T, repo string) *Cluster {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c := &Cluster{
		t:      t,
		ctx:    ctx,
		cancel: cancel,
		truth:  store.NewMemory(),
		repo:   repo,
		root:   t.TempDir(),
	}
	t.Cleanup(c.Close)
	return c
}

// Close shuts every instance registry down and cancels the cluster context.
// Idempotent: restarts call it repeatedly.
func (c *Cluster) Close() {
	for _, in := range c.instances {
		if in.reg != nil {
			in.reg.Close()
			in.reg = nil
		}
	}
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
}

// AddInstance boots one instance (fresh cache dir, clean link plan) and
// opens (or, for the first instance, creates) the repo handle. Callers set
// faults via in.link.Set after boot so setup ops stay clean.
func (c *Cluster) AddInstance(name string, seed uint64) *Instance {
	c.t.Helper()
	in := &Instance{name: name, seed: seed, link: fault.New(c.truth, name, linkSeed(seed, len(c.instances)))}
	in.boot(c, false)
	c.instances = append(c.instances, in)
	return in
}

// Instance is one simulated walhub process: a FaultStore link, a Registry
// over it, and the open repo handle.
type Instance struct {
	name     string
	seed     uint64
	link     *fault.FaultStore
	reg      *wal.Registry
	h        *wal.RepoHandle
	cacheDir string
	restarts int
}

// boot (re)builds the registry + handle. keepDisk=false means a fresh cache
// dir (Restart: fresh process state, same link); keepDisk=true reuses the
// instance's dir (RestartKeepDisk: the persistent cache survives).
func (in *Instance) boot(c *Cluster, keepDisk bool) {
	c.t.Helper()
	if !keepDisk || in.cacheDir == "" {
		in.cacheDir = filepath.Join(c.root, fmt.Sprintf("%s-r%d", in.name, in.restarts))
		if err := os.MkdirAll(in.cacheDir, 0o755); err != nil {
			c.t.Fatalf("sim: mkdir %s: %v", in.cacheDir, err)
		}
	}
	in.reg = wal.NewRegistry(c.ctx, in.link, simConfig(in.cacheDir))
	var h *wal.RepoHandle
	var err error
	if !c.created {
		h, err = in.reg.Create(c.ctx, c.repo, git.Sha1)
		if err == nil {
			c.created = true
		}
	} else {
		h, err = in.reg.Open(c.ctx, c.repo)
	}
	if err != nil {
		c.t.Fatalf("sim: %s boot (created=%v): %v", in.name, c.created, err)
	}
	in.h = h
	in.link.SetTrace(true)
}

// Restart abandons this instance's process state (goroutines die with the
// registry ctx; a crashed handle is dropped, never reused) and boots fresh
// on the SAME link (stats and fault plan survive — the partition is real).
// When the link hangs (BlackHole/PHang), Heal (or Set a non-hanging plan)
// BEFORE Restart: the reboot's open has no timeout by design (a hung store
// op answers only to context cancel), so rebooting onto a hanging link hangs
// the test until the go-test timeout.
func (in *Instance) Restart(c *Cluster) {
	c.t.Helper()
	in.restarts++
	if in.reg != nil {
		in.reg.Close()
		in.reg = nil
	}
	in.boot(c, false)
}

// RestartKeepDisk is Restart with the cache dir surviving (the "same disk,
// new process" crash model).
func (in *Instance) RestartKeepDisk(c *Cluster) {
	c.t.Helper()
	in.restarts++
	if in.reg != nil {
		in.reg.Close()
		in.reg = nil
	}
	in.boot(c, true)
}

// Heal clears the link's fault plan (liveness mode for a core link; ops
// already hanging stay hung — that is the point of a crash).
func (in *Instance) Heal() { in.link.Heal() }

// Ops returns the link's total op counter (the budget instrument).
func (in *Instance) Ops() uint64 { return in.link.Stats().Ops.Load() }

// withCrash converts an injected FaultStore panic into a synthetic process
// death: (crashed=true). The caller must Restart the instance; the old handle
// is wedged (locks may be held) and is never reused.
func withCrash(name string, fn func() error) (err error, crashed bool) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("sim: instance %s crashed: %v", name, r)
			crashed = true
		}
	}()
	err = fn()
	return err, false
}

// Publish wraps h.Publish with the instance-boundary crash recovery.
func (in *Instance) Publish(ctx context.Context, req wal.PublishRequest) (res wal.PublishResult, err error, crashed bool) {
	err, crashed = withCrash(in.name, func() error {
		var perr error
		res, perr = in.h.Publish(ctx, req)
		return perr
	})
	return res, err, crashed
}

// Sync wraps h.Sync with the instance-boundary crash recovery. A non-nil
// guard must be Released by the caller.
func (in *Instance) Sync(ctx context.Context, lvl wal.SyncLevel) (g *wal.ReadGuard, err error, crashed bool) {
	err, crashed = withCrash(in.name, func() error {
		var serr error
		g, serr = in.h.Sync(ctx, lvl)
		return serr
	})
	return g, err, crashed
}

// Checkpoint wraps h.WriteCheckpoint with the instance-boundary crash
// recovery.
func (in *Instance) Checkpoint(ctx context.Context, trig wal.CheckpointTrigger) (err error, crashed bool) {
	return withCrash(in.name, func() error { return in.h.WriteCheckpoint(ctx, trig) })
}

// LocalRef reads one ref from the instance's local view (no store ops).
func (in *Instance) LocalRef(ref string) (string, bool) {
	snap, err := in.h.Layer().Snapshot(in.h.Repo())
	if err != nil {
		return "", false
	}
	e, ok := snap.Get(ref)
	if !ok {
		return "", false
	}
	return e.Oid, true
}

// PushRef publishes ref := newOid (old must be the caller's last committed
// oid for that ref) with conflict/transient retry until ctx expires or
// maxPushAttempts is hit. Returns the committed seq. Under faults, transient
// store errors retry with a short backoff; per-ref conflicts re-sync first
// (the caller's old stays authoritative for refs it alone writes).
func (in *Instance) PushRef(ctx context.Context, ref, old, newOid string) (uint64, error) {
	var lastErr error
	for attempt := 0; attempt < maxPushAttempts; attempt++ {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		res, err, crashed := in.Publish(ctx, wal.PublishRequest{Txn: refTxn(ref, old, newOid)})
		if crashed {
			return 0, fmt.Errorf("sim: %s crashed during push of %s (restart first)", in.name, ref)
		}
		if err != nil {
			lastErr = err
			if simVerbose() {
				fmt.Printf("sim: %s push %s attempt %d: transient %v | cause: %v\n",
					in.name, ref, attempt, err, unwrapAll(err))
			}
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}
		conflicted := false
		for _, pr := range res.PerRef {
			if pr.Err != nil {
				conflicted = true
				lastErr = pr.Err
				break
			}
		}
		if !conflicted {
			return res.Seq, nil
		}
		if simVerbose() {
			fmt.Printf("sim: %s push %s attempt %d: conflict %v (re-syncing)\n", in.name, ref, attempt, lastErr)
		}
		if g, serr, scr := in.Sync(ctx, wal.LevelRefs); scr {
			return 0, fmt.Errorf("sim: %s crashed re-syncing %s", in.name, ref)
		} else if serr == nil {
			g.Release()
		} else if simVerbose() {
			fmt.Printf("sim: %s push %s attempt %d: re-sync error %v\n", in.name, ref, attempt, serr)
		}
	}
	return 0, fmt.Errorf("sim: %s push of %s did not converge after %d attempts (last: %v)",
		in.name, ref, maxPushAttempts, lastErr)
}

// TruthManifest reads manifest.pb via the truth store, bypassing every link
// — no instance's stale view can contaminate the oracle.
func (c *Cluster) TruthManifest() (*proto.Manifest, error) {
	body, _, err := store.GetBytes(c.ctx, c.truth, repoManifestKey(c.repo), store.GetOptions{})
	if err != nil {
		return nil, err
	}
	if body == nil {
		return nil, fmt.Errorf("sim: truth manifest absent for %s", c.repo)
	}
	return proto.UnmarshalManifest(body)
}

// CheckTruth is the post-run oracle: the truth manifest decodes, every listed
// log segment is present, segments never overlap and the newest ends at head
// (burn gaps from crashed writers are allowed — orphans are swept, never
// relisted), and every expected ref is reflected in a fresh verifier's view.
func (c *Cluster) CheckTruth(expectedRefs map[string]string) error {
	c.t.Helper()
	m, err := c.TruthManifest()
	if err != nil {
		return err
	}
	var problems []string
	problems = append(problems, verifySegments(m)...)
	// Every listed segment must be present (a manifest reference to a missing
	// pack/segment is corruption, not garbage).
	for _, s := range m.LogSegments {
		segKey := "repos/" + c.repo + "/" + s.Key
		ok, herr := store.Exists(c.ctx, c.truth, segKey)
		if herr != nil {
			problems = append(problems, fmt.Sprintf("segment %s HEAD error: %v", s.Key, herr))
		} else if !ok {
			problems = append(problems, fmt.Sprintf("segment %s listed but absent", s.Key))
		}
	}
	// Fresh verifier over the truth (no link): sync, snapshot, compare refs.
	ver := wal.NewRegistry(c.ctx, c.truth, simConfig(filepath.Join(c.root, "verifier")))
	defer ver.Close()
	h, err := ver.Open(c.ctx, c.repo)
	if err != nil {
		return fmt.Errorf("sim: verifier open: %v (segments: %v)", err, problems)
	}
	g, err := h.Sync(c.ctx, wal.LevelRefs)
	if err != nil {
		return fmt.Errorf("sim: verifier sync: %v (segments: %v)", err, problems)
	}
	defer g.Release()
	snap, err := h.Layer().Snapshot(h.Repo())
	if err != nil {
		return fmt.Errorf("sim: verifier snapshot: %v", err)
	}
	got := map[string]string{}
	for _, r := range snap.Refs {
		got[r.Name] = r.Oid
	}
	problems = append(problems, checkRefs(got, expectedRefs)...)
	if len(problems) > 0 {
		return fmt.Errorf("sim: truth check failed:\n  manifest head=%d min=%d rev=%d segs=%s cpset=%v\n  keys=%s\n  - %s",
			m.HeadSeq, m.MinSeq, m.Revision, segSummary(m), m.Checkpoint != nil,
			c.keySummary(), strings.Join(problems, "\n  - "))
	}
	return nil
}

// segSummary renders first/last per listed segment for failure artifacts.
func segSummary(m *proto.Manifest) string {
	parts := make([]string, 0, len(m.LogSegments))
	for _, s := range m.LogSegments {
		parts = append(parts, fmt.Sprintf("[%d,%d]", s.FirstSeq, s.LastSeq))
	}
	return strings.Join(parts, ",")
}

// keySummary lists every key under the repo prefix (the garbage census:
// unlisted orphans are expected; listed-but-absent is corruption).
func (c *Cluster) keySummary() string {
	keys, err := fault.SnapshotKeys(c.ctx, c.truth, "repos/"+c.repo+"/")
	if err != nil {
		return "list-error: " + err.Error()
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, strings.TrimPrefix(k, "repos/"+c.repo+"/"))
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// verifySegments checks the pure manifest invariant over log segments:
// sorted by FirstSeq, never overlapping, newest ends exactly at head. Gaps
// are allowed (burned orphan seqs are swept, never relisted). Checkpoint
// trimming is honored: seqs below min_seq are folded into the checkpoint, so
// no listed segment may lie entirely below min_seq, and an empty segment
// list is legal exactly when the checkpoint sits at head. Returns one problem
// string per violation.
func verifySegments(m *proto.Manifest) []string {
	if m == nil {
		return []string{"nil manifest"}
	}
	var problems []string
	if m.Checkpoint != nil {
		if m.MinSeq != m.Checkpoint.Seq+1 {
			problems = append(problems, fmt.Sprintf("min_seq %d != checkpoint seq %d + 1",
				m.MinSeq, m.Checkpoint.Seq))
		}
	} else if m.MinSeq != 0 {
		problems = append(problems, fmt.Sprintf("min_seq %d without a checkpoint", m.MinSeq))
	}
	segs := append([]*proto.LogSegmentRef{}, m.LogSegments...)
	sort.Slice(segs, func(i, j int) bool { return segs[i].FirstSeq < segs[j].FirstSeq })
	if len(segs) == 0 {
		if m.Checkpoint != nil {
			if m.HeadSeq != m.Checkpoint.Seq {
				problems = append(problems, fmt.Sprintf("no segments but head %d != checkpoint seq %d",
					m.HeadSeq, m.Checkpoint.Seq))
			}
		} else if m.HeadSeq != 0 {
			problems = append(problems, fmt.Sprintf("no segments but head is %d", m.HeadSeq))
		}
		return problems
	}
	var prevLast uint64
	for i, s := range segs {
		if s.LastSeq < s.FirstSeq {
			problems = append(problems, fmt.Sprintf("segment %s: last %d < first %d", s.Key, s.LastSeq, s.FirstSeq))
		}
		if s.LastSeq < m.MinSeq {
			problems = append(problems, fmt.Sprintf("segment %s entirely below min_seq %d (not trimmed)",
				s.Key, m.MinSeq))
		}
		if i > 0 && s.FirstSeq <= prevLast {
			problems = append(problems, fmt.Sprintf("segment %s overlaps previous (first %d <= prev last %d)",
				s.Key, s.FirstSeq, prevLast))
		}
		prevLast = s.LastSeq
	}
	if prevLast != m.HeadSeq {
		problems = append(problems, fmt.Sprintf("newest segment ends at %d, head is %d", prevLast, m.HeadSeq))
	}
	return problems
}

// checkRefs compares the verifier's ref view against expectations: every
// expected ref present with the right oid; no unexpected refs (a lost commit
// would surface as a missing or reverted ref; a phantom as an extra).
func checkRefs(got, expected map[string]string) []string {
	var problems []string
	for name, want := range expected {
		if gotOid, ok := got[name]; !ok {
			problems = append(problems, fmt.Sprintf("ref %s missing (want %s)", name, want))
		} else if gotOid != want {
			problems = append(problems, fmt.Sprintf("ref %s = %s (want %s)", name, gotOid, want))
		}
	}
	for name := range got {
		if _, ok := expected[name]; !ok {
			problems = append(problems, fmt.Sprintf("unexpected ref %s = %s", name, got[name]))
		}
	}
	return problems
}

// DumpTraces renders the standard failure artifact: per-link stats summary
// plus the last 40 trace lines, newest first (15_testing.md §3.4).
func (c *Cluster) DumpTraces() string {
	var b strings.Builder
	for _, in := range c.instances {
		fmt.Fprintf(&b, "link %s: %s\n", in.name, in.link.Stats().Summary())
		lines := in.link.TakeTrace()
		for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
			lines[i], lines[j] = lines[j], lines[i]
		}
		if len(lines) > 40 {
			lines = lines[:40]
		}
		for _, l := range lines {
			fmt.Fprintf(&b, "    %s\n", l)
		}
	}
	return b.String()
}

// failReport renders the standard failure artifact: the message plus the
// truth manifest summary, the repo key census (orphan backlog size is the
// liveness signal; listed-but-absent is corruption), and the per-link traces.
func (c *Cluster) failReport(format string, args ...any) string {
	diag := ""
	if m, err := c.TruthManifest(); err != nil {
		diag = fmt.Sprintf("truth manifest unreadable: %v; ", err)
	} else {
		diag = fmt.Sprintf("truth head=%d min=%d rev=%d segs=%s; keys: %s; ",
			m.HeadSeq, m.MinSeq, m.Revision, segSummary(m), c.keySummary())
	}
	return fmt.Sprintf("sim: "+format+"\n--- %s--- traces ---\n%s",
		append(args, diag, c.DumpTraces())...)
}

// failf fails the test with the failure artifact. Untestable directly
// (t.Fatalf stops the test); failReport above carries the testable logic.
func (c *Cluster) failf(format string, args ...any) {
	c.t.Helper()
	c.t.Fatalf("%s", c.failReport(format, args...))
}

// ErrLeaseHeld reports a live lease the caller must not steal (§4.9: the
// ladder never waits — a held lease surfaces, it does not block).
var ErrLeaseHeld = fmt.Errorf("sim: lease held")

// acquireLease is the §4.9 acquire ladder over st (the lease-protocol
// conformance itself is contract-tested by TestContract_LeaseSteal; the sim
// uses this to prove a partition never blocks the core): absent → Create
// epoch 0; present and now ≥ expires_at + 2s (the clock-skew tolerance) →
// Update with epoch+1; otherwise ErrLeaseHeld. Retries raced CAS losses.
func acquireLease(ctx context.Context, st store.ObjectStore, key, holder string, ttl time.Duration) (store.Version, error) {
	const skew = 2 * time.Second
	for attempt := 0; attempt < 8; attempt++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		body, meta, err := store.GetBytes(ctx, st, key, store.GetOptions{})
		if err != nil && !store.IsNotFound(err) {
			return "", err
		}
		now := time.Now().UTC()
		if body == nil {
			acq, exp := proto.TimeFromGo(now), proto.TimeFromGo(now.Add(ttl))
			lease := &proto.Lease{Holder: holder, Purpose: "sim", AcquiredAt: &acq, ExpiresAt: &exp, Epoch: 0}
			put, err := st.Put(ctx, key, store.PutBody{Bytes: lease.Marshal()},
				store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"})
			if err == nil {
				return put.Version, nil
			}
			if !store.IsPreconditionFailed(err) {
				return "", err
			}
			continue
		}
		cur := &proto.Lease{}
		if err := cur.Unmarshal(body); err != nil {
			return "", err
		}
		var expires time.Time
		if cur.ExpiresAt != nil {
			expires = cur.ExpiresAt.Go()
		}
		if now.Before(expires.Add(skew)) {
			return "", ErrLeaseHeld
		}
		next := *cur
		next.Holder = holder
		next.Epoch = cur.Epoch + 1
		acq, exp := proto.TimeFromGo(now), proto.TimeFromGo(now.Add(ttl))
		next.AcquiredAt, next.ExpiresAt = &acq, &exp
		put, err := st.Put(ctx, key, store.PutBody{Bytes: next.Marshal()},
			store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/x-protobuf"})
		if err == nil {
			return put.Version, nil
		}
		if !store.IsPreconditionFailed(err) {
			return "", err
		}
	}
	return "", ErrLeaseHeld
}
