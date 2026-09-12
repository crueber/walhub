// sync.go — the mirror-sync engine: the Seam 5 task, the bucket lease,
// the followOnce-shaped converge, and the registry-enumeration loop.
//
// ### Concurrency
//
// Hazard: two instances firing the same due mirror would run two full
// clones and race publishes; the (repo,kind) single-flight is
// instance-memory only and does not span instances.
// Avoidance: the sync body takes the bucket lease
// (leases/mirror-<owner>-<name>.pb, CAS+TTL, skew 0 — the bundle-lease
// shape) BEFORE cloning. A held lease is a skip + narrate, never a
// wait, never a retry within the fire. The loop itself is one
// goroutine (RunLoop) exiting via ctx; every fire is a task whose
// goroutines derive from the task ctx (drain cancels everything); no
// lock is held across store/network calls.
package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/repoimport"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// leaseTTL bounds one sync fire's cross-instance exclusion (clone +
// converge of a human-scale mirror; the loop never waits on it).
const leaseTTL = 10 * time.Minute

// kinds registry (Seam 5): one kind, registered once from composition
// — a duplicate panics (the maintain.RegisterKind contract in code
// terms, same as repoimport.RegisterKind).
var (
	kindsMu sync.Mutex
	kinds   = map[string]bool{}
)

// RegisterKind records a task-kind name; a duplicate panics.
func RegisterKind(name string) {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	if kinds[name] {
		panic(fmt.Sprintf("mirror: duplicate task kind %q", name))
	}
	kinds[name] = true
}

// ResetKindsForTest clears the kind registry. Test-only: the registry
// is process-global and write-only (composition registers once per
// process), so without a reset every -count=N re-run of a registering
// test trips the duplicate panic outside its recover (Forgejo #397).
// Production keeps the panic-on-duplicate contract above.
func ResetKindsForTest() {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	kinds = map[string]bool{}
}

// Service owns the mirror-sync surface: the task spawn (SyncNow,
// shared by the loop, the manual endpoint, and the create-from-URL
// first sync) and the scheduled loop (RunLoop).
type Service struct {
	store    store.ObjectStore
	reg      *wal.Registry
	git      *Runner
	hostname string
	now      func() time.Time

	asyncMu sync.Mutex
	async   map[string]*asyncSync // id-keyed async fires (HTTP 202 surface)

	// SSRF gate input, from the [import] section (one gate, one clock
	// for both flows — no new config section).
	allowPrivate bool
	allowlist    []string
	allowFile    bool
	maxBytes     int64

	// probeTimeout bounds one servability probe's Sync+object join loop
	// (issue #320; the probe is patient background work, unlike a
	// request's serve_sync_timeout wait). Zero → the engine's default
	// materialize cap (composition wires server.serve_materialize_timeout).
	probeTimeout time.Duration
	// probe runs the post-publish servability probe (h + head oid). A
	// func field (the `now` clock precedent): tests substitute failure
	// and hang shapes; production always uses probeServe.
	probe func(ctx context.Context, h *wal.RepoHandle, oid string) error
}

// Deps wires a Service. Store/Reg are required; GitBinary falls back
// to "git"; Hostname falls back to "unknown".
type Deps struct {
	Store     store.ObjectStore
	Reg       *wal.Registry
	GitBinary string
	CacheDir  string
	Hostname  string
	Now       func() time.Time

	CloneTimeout time.Duration
	GitTimeout   time.Duration

	// ProbeTimeout bounds one servability probe (issue #320): the
	// post-publish Sync+object join loop. Zero → the engine's default
	// materialize cap (composition wires server.serve_materialize_timeout).
	ProbeTimeout time.Duration

	AllowPrivate bool
	Allowlist    []string
	AllowFile    bool
	MaxBytes     int64
}

// New builds a Service over the shared store/registry.
func New(d Deps) *Service {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	host := d.Hostname
	if host == "" {
		host = "unknown"
	}
	s := &Service{
		store:        d.Store,
		reg:          d.Reg,
		git:          NewRunner(d.GitBinary, d.CacheDir, d.CloneTimeout, d.GitTimeout),
		hostname:     host,
		now:          now,
		allowPrivate: d.AllowPrivate,
		allowlist:    d.Allowlist,
		allowFile:    d.AllowFile,
		maxBytes:     d.MaxBytes,
		probeTimeout: d.ProbeTimeout,
	}
	if s.probeTimeout <= 0 {
		s.probeTimeout = wal.DefaultServeMaterializeTimeout
	}
	s.probe = s.probeServe
	return s
}

// SyncNow spawns (or joins, via the (repo,mirror-sync) single-flight)
// one sync fire and awaits its outcome. Callers with a request ctx
// that must not block (HTTP handlers) use SyncAsync instead — law 7
// (never block a request goroutine on bulk work): a clone is bulk
// work, and the task narrates itself regardless of listeners.
func (s *Service) SyncNow(ctx context.Context, owner, name, token string, force bool) (*wal.TaskRecord, error) {
	if s.reg == nil {
		return nil, fmt.Errorf("mirror: registry not configured")
	}
	doc, _, err := Load(ctx, s.store, owner, name)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("mirror: %s/%s is not a mirror", owner, name)
	}
	target := owner + "/" + name
	params := map[string]string{
		"upstream_url": doc.UpstreamURL,
		"schedule":     doc.Schedule,
	}
	if force {
		params["force"] = "true"
	}
	if token != "" {
		params["secret_set"] = "true" // presence only — import S2 scrub rule
	}
	return s.reg.Tasks().Run(ctx, target, KindMirrorSync, params,
		func(tctx context.Context, task *wal.Task) error {
			return s.runSync(tctx, task, owner, name, token, force)
		})
}

// asyncSync is one service-level async fire (the HTTP shape): the id
// is minted up front so the 202 carries it; the outcome lands on
// done-close. The table's (repo,kind) single-flight still joins
// overlapping fires inside the body — this map never dedups, it only
// names (import B4 id-keyed rings, minimal form).
type asyncSync struct {
	id      string
	target  string
	started string
	done    chan struct{}
	rec     *wal.TaskRecord
	err     error
}

// SyncAsync spawns a fire without blocking (HTTP handlers): the task
// body runs on a detached ctx (client disconnect cannot cancel it —
// the import B6 discipline; drain still cancels via the table), and
// the returned id resolves via SyncStatus. token is memory-only.
func (s *Service) SyncAsync(ctx context.Context, owner, name, token string, force bool) string {
	id := newSyncID()
	a := &asyncSync{id: id, target: owner + "/" + name, started: s.now().UTC().Format(time.RFC3339Nano), done: make(chan struct{})}
	s.asyncMu.Lock()
	if s.async == nil {
		s.async = map[string]*asyncSync{}
	}
	s.async[id] = a
	pruneAsyncLocked(s.async)
	s.asyncMu.Unlock()
	go func() {
		rec, err := s.SyncNow(context.WithoutCancel(ctx), owner, name, token, force)
		a.rec, a.err = rec, err
		close(a.done)
	}()
	return id
}

// SyncStatus resolves an async id (ok=false when unknown) and lists
// recent finished mirror-sync table records for the target (newest
// last — the TaskTable.List order).
func (s *Service) SyncStatus(id string) (a *asyncSync, ok bool) {
	s.asyncMu.Lock()
	defer s.asyncMu.Unlock()
	a, ok = s.async[id]
	return a, ok
}

// RecentSyncs returns the finished mirror-sync task records for a
// target (the table's recent ring, kind-filtered).
func (s *Service) RecentSyncs(target string) []*wal.TaskRecord {
	if s.reg == nil {
		return nil
	}
	var out []*wal.TaskRecord
	for _, rec := range s.reg.Tasks().List(target) {
		if rec != nil && rec.Kind == KindMirrorSync {
			out = append(out, rec)
		}
	}
	return out
}

func newSyncID() string {
	return fmt.Sprintf("msync-%d", time.Now().UTC().UnixNano())
}

// pruneAsyncLocked evicts finished async entries past 100 (bounded
// memory — the ring, not a log).
func pruneAsyncLocked(m map[string]*asyncSync) {
	if len(m) <= 100 {
		return
	}
	type kv struct {
		id      string
		started string
		done    bool
	}
	var all []kv
	for id, a := range m {
		select {
		case <-a.done:
			all = append(all, kv{id, a.started, true})
		default:
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].started < all[j].started })
	for _, e := range all {
		if len(m) <= 100 {
			break
		}
		delete(m, e.id)
	}
}

// runSync is the task body: lease → gates → clone → ingest packs →
// ff-only converge → PublishRefs → record outcome. Skips (held lease,
// removed mirror, live import claim, nothing moved) narrate and
// return nil — a skip is not a failure and never touches the failure
// counter. Failures record consecutive_failures + the scrubbed reason
// and return the error (task terminal).
func (s *Service) runSync(ctx context.Context, task *wal.Task, owner, name, token string, force bool) error {
	target := owner + "/" + name
	now := s.now()
	task.Notice(fmt.Sprintf("mirror sync %s (force=%v)", target, force))

	release, lerr := s.acquireLease(ctx, owner, name)
	if lerr != nil {
		task.Notice(fmt.Sprintf("mirror sync %s skipped: another instance holds the sync lease", target))
		return nil
	}
	defer release()

	doc, _, err := Load(ctx, s.store, owner, name)
	if err != nil {
		return s.fail(ctx, owner, name, now, task, err.Error())
	}
	if doc == nil {
		task.Notice(fmt.Sprintf("mirror sync %s skipped: mirror configuration removed", target))
		return nil
	}
	if !force && BackedOff(doc, now) {
		task.Notice(fmt.Sprintf("mirror sync %s skipped: in failure backoff (%d consecutive failures)", target, doc.ConsecutiveFailures))
		return nil
	}
	if skip, serr := s.importClaimLive(ctx, owner, name); serr != nil {
		return s.fail(ctx, owner, name, now, task, serr.Error())
	} else if skip {
		task.Notice(fmt.Sprintf("mirror sync %s skipped: a repo-import claim is in progress on the target", target))
		return nil
	}

	n, err := repoimport.NormalizeSource(doc.UpstreamURL)
	if err != nil {
		return s.fail(ctx, owner, name, now, task, err.Error())
	}
	if verr := repoimport.ValidateTransport(n, token != ""); verr != nil {
		return s.fail(ctx, owner, name, now, task, verr.Error())
	}
	if serr := repoimport.CheckSSRF(n, repoimport.SSRFConfig{
		AllowPrivate: s.allowPrivate,
		Allowlist:    s.allowlist,
		AllowFile:    s.allowFile,
		Dangerous:    true, // authority check happened at mirror creation (admin-only PUT); the stored URL re-gates host/private only
	}, nil); serr != nil {
		return s.fail(ctx, owner, name, now, task, serr.Error())
	}

	scratch, err := s.git.ScratchDir(owner, name)
	if err != nil {
		return s.fail(ctx, owner, name, now, task, err.Error())
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // task scratch is disposable by law 1

	tokenEnv := ""
	if token != "" {
		tokenEnv = CredentialEnv(target + "/" + now.Format("150405.000000000"))
	}
	task.Notice(fmt.Sprintf("mirror sync %s: cloning %s", target, n.URL))
	if cerr := s.git.CloneMirror(ctx, n.URL, scratch, n.Scheme, n.Host, tokenEnv, token); cerr != nil {
		return s.fail(ctx, owner, name, now, task, classifySyncError(ctx, cerr))
	}

	all, ferr := s.git.ForEachRef(ctx, scratch)
	if ferr != nil {
		return s.fail(ctx, owner, name, now, task, ferr.Error())
	}
	// S4 refmap reuse (R1 (e)): import branches + tags; drop
	// replace/meta/keep-around always, pull/changes/review + notes by
	// default (a mirror is not a forge — upstream PR refs stay out).
	riRefs := make([]repoimport.Ref, 0, len(all))
	for _, r := range all {
		riRefs = append(riRefs, repoimport.Ref{Name: r.Name, Oid: r.Oid, Peeled: r.Peeled})
	}
	kept := repoimport.FilterRefs(riRefs, false, false, nil, false, "")

	h, oerr := s.reg.Open(ctx, target)
	if oerr != nil {
		return s.fail(ctx, owner, name, now, task, fmt.Sprintf("open repo: %v", oerr))
	}
	curTips, curPeeled, curHead, rerr := s.currentRefs(ctx, h)
	if rerr != nil {
		return s.fail(ctx, owner, name, now, task, rerr.Error())
	}

	formatName, ferr := s.git.ShowObjectFormat(ctx, scratch)
	if ferr != nil {
		return s.fail(ctx, owner, name, now, task, ferr.Error())
	}
	meta := map[string]string{"agent": KindMirrorSync, "principal": "mirror", "mirrored_from": n.URL}
	if perr := s.ingestPacks(ctx, task, h, owner, name, scratch, formatName, meta); perr != nil {
		return s.fail(ctx, owner, name, now, task, perr.Error())
	}

	// followOnce-shaped converge (R1 (e)): compare + ff-only + atomic
	// PublishRefs. Deleted upstream refs are left alone — deletion is
	// a human's call (follow §8.3).
	txn := &proto.RefTransaction{Atomic: true}
	refused := []string{}
	moved := 0
	for _, r := range kept {
		old := curTips[r.Name]
		if old == r.Oid && curPeeled[r.Name] == r.Peeled {
			continue
		}
		if old != "" && !isZeroHex(old) && !force {
			anc, aerr := s.git.MergeBaseIsAncestor(ctx, scratch, old, r.Oid)
			if aerr != nil {
				return s.fail(ctx, owner, name, now, task, fmt.Sprintf("ff check %s: %v", r.Name, scrubText(aerr.Error())))
			}
			if !anc {
				refused = append(refused, r.Name)
				// r.Name is upstream-controlled (import S2 scrub
				// discipline — a hostile upstream must not smuggle
				// credential-shaped text into task logs).
				task.Notice(fmt.Sprintf("mirror sync %s: upstream rewound %s %s→%s; refusing (ff-only — use force resync to override)", target, scrubText(r.Name), shortOid(old), shortOid(r.Oid)))
				continue
			}
		}
		u := &proto.RefUpdate{Name: r.Name, OldOid: old, NewOid: r.Oid}
		if old == "" {
			u.OldOid = zeroHex(len(r.Oid))
		}
		if r.Peeled != "" {
			u.NewPeeled = r.Peeled
		}
		txn.Updates = append(txn.Updates, u)
		moved++
	}
	headTarget := HeadTarget(scratch)
	if headTarget != "" && headTarget != curHead {
		keep := false
		for _, r := range kept {
			if r.Name == headTarget {
				keep = true
				break
			}
		}
		if keep || headTarget == "refs/heads/main" {
			txn.Updates = append(txn.Updates, &proto.RefUpdate{Name: "HEAD", NewSymbolicTarget: headTarget})
		}
	}
	if len(txn.Updates) == 0 {
		if len(refused) > 0 {
			s.recordRefused(ctx, owner, name, now, refused)
			task.Notice(fmt.Sprintf("mirror sync %s: %d ref(s) rewound upstream; refused (ff-only)", target, len(refused)))
			return nil
		}
		task.Notice(fmt.Sprintf("mirror sync %s: already in sync", target))
		return s.succeed(ctx, owner, name, now)
	}
	// Server-side publish bypasses the push funnel by construction
	// (R1 (c)): Publish/PublishRefs never enter pushPipeline, so the
	// sync cannot refuse itself.
	if _, perr := h.Publish(ctx, wal.PublishRequest{Txn: txn, Meta: meta}); perr != nil {
		return s.fail(ctx, owner, name, now, task, fmt.Sprintf("publish refs: %v (safe to retry)", scrubText(perr.Error())))
	}
	task.Notice(fmt.Sprintf("mirror sync %s: published %d ref(s)%s", target, moved, refusedSuffix(refused)))
	// Servability probe (issue #320): a sync that publishes refs the
	// instance cannot serve is not a successful sync — last_result must
	// not say "ok". The probe joins the serve materialization with a
	// background budget (probeTimeout, patient — unlike a request's
	// serve_sync_timeout wait) and then proves one object readable at
	// the new head. Failure records the degraded outcome (failure
	// counter + backoff, serve-health marker) and fails the task; the
	// refs stay published and the next fire retries.
	if perr := s.probe(ctx, h, probeOid(kept, headTarget)); perr != nil {
		reason := "servability probe: " + scrubText(wal.ShortErr(perr))
		markServeDegraded(s.store, owner, name, reason)
		task.Notice(fmt.Sprintf("mirror sync %s: published %d ref(s) but %s", target, moved, reason))
		return s.fail(ctx, owner, name, now, task, reason)
	}
	clearServeHealth(s.store, owner, name)
	return s.succeed(ctx, owner, name, now)
}

func refusedSuffix(refused []string) string {
	if len(refused) == 0 {
		return ""
	}
	return fmt.Sprintf("; %d rewound ref(s) refused", len(refused))
}

// --- servability probe (issue #320) -------------------------------------------

// probeOid selects the object the post-publish probe proves readable:
// the HEAD target's tip when published, else the first published oid,
// else "" (nothing published provable — an empty probe passes on the
// Sync alone).
func probeOid(kept []repoimport.Ref, headTarget string) string {
	for _, r := range kept {
		if r.Name == headTarget && r.Oid != "" {
			return r.Oid
		}
	}
	for _, r := range kept {
		if r.Oid != "" {
			return r.Oid
		}
	}
	return ""
}

// isServeTimeout reports a bounded-serve-wait expiry (the engine's
// WalErrTimeout): the materialize body may still be progressing in the
// background, so the probe re-joins while its budget lasts. Any other
// error is a verdict — returned immediately.
func isServeTimeout(err error) bool {
	var we *wal.WalError
	return errors.As(err, &we) && we.Kind == wal.WalErrTimeout
}

// probeServe joins the serve materialization with a background budget
// and proves one object readable. Each h.Sync waits at most
// serve_sync_timeout (the engine bound); timeouts re-join while the
// probe budget (probeTimeout) lasts, so a slow-but-progressing cold
// materialize still passes without flapping. The object check verdict
// is terminal (a missing object will not appear on retry). Never hangs
// past the budget: the last timeout (or verdict) is returned.
func (s *Service) probeServe(ctx context.Context, h *wal.RepoHandle, oid string) error {
	deadline := time.Now().Add(s.probeTimeout)
	for {
		g, err := h.Sync(ctx, wal.LevelServe)
		if err == nil {
			oerr := s.probeObject(ctx, h, oid)
			g.Release()
			return oerr
		}
		if !probeRetry(err, time.Now(), deadline) {
			return err
		}
	}
}

// probeRetry reports whether a failed Sync attempt is worth re-joining:
// only a serve-wait timeout inside the probe budget. Every other error
// (corrupt packs, canceled caller, missing store) is a verdict.
func probeRetry(err error, now, deadline time.Time) bool {
	return isServeTimeout(err) && now.Before(deadline)
}

// probeObject proves one object readable in the serving copy (the
// "one blob fetch at head" shape): "" oid (nothing published) passes
// on the Sync alone.
func (s *Service) probeObject(ctx context.Context, h *wal.RepoHandle, oid string) error {
	if oid == "" {
		return nil
	}
	return s.git.ProbeObject(ctx, h.Dir(), oid)
}

// succeed records the success outcome (clears the failure counter,
// stamps last_synced_at — the only writer of the next-fire anchor).
func (s *Service) succeed(ctx context.Context, owner, name string, now time.Time) error {
	if err := RecordAttempt(ctx, s.store, owner, name, true, "", now); err != nil {
		return fmt.Errorf("record sync outcome: %w", err)
	}
	return nil
}

// fail records consecutive_failures + the scrubbed reason, narrates,
// and returns the terminal task error.
func (s *Service) fail(ctx context.Context, owner, name string, now time.Time, task *wal.Task, reason string) error {
	reason = scrubText(reason)
	if rerr := RecordAttempt(ctx, s.store, owner, name, false, reason, now); rerr != nil {
		task.Notice(fmt.Sprintf("mirror sync %s/%s: outcome record lost: %v", owner, name, rerr))
	}
	task.Notice(fmt.Sprintf("mirror sync %s/%s failed: %s", owner, name, reason))
	return fmt.Errorf("mirror sync: %s", reason)
}

// recordRefused stamps a rewind refusal WITHOUT touching the failure
// counter (a rewound upstream is policy, not an outage — backoff
// would only delay the narration; the refusal repeats every round
// until a human force-resyncs, follow §8.3).
func (s *Service) recordRefused(ctx context.Context, owner, name string, now time.Time, refs []string) {
	for attempt := 0; attempt < 2; attempt++ {
		doc, ver, err := Load(ctx, s.store, owner, name)
		if err != nil || doc == nil {
			return
		}
		doc.LastAttemptAt = now.UTC().Format(time.RFC3339)
		// Ref names are upstream-controlled: scrub before they reach
		// the sidecar (same S2 rule as the task notice above).
		doc.LastResult = "refused: upstream rewound " + scrubText(shortList(refs)) + " (ff-only; force resync to override)"
		if uerr := UpdateCAS(ctx, s.store, owner, name, doc, ver); uerr != nil {
			if store.IsPreconditionFailed(uerr) {
				continue
			}
		}
		return
	}
}

// importClaimLive reports a live repo-import claim on the target
// (import.json present with Complete=false — the fix-#79 claim; a
// landed Complete=true provenance is NOT live). R1 (e): the #79 claim
// protocol otherwise collides with the sync writer.
func (s *Service) importClaimLive(ctx context.Context, owner, name string) (bool, error) {
	raw, _, err := store.GetBytes(ctx, s.store, store.RepoPrefix(owner, name)+repoimport.ImportKey, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("probe import claim: %v", scrubText(err.Error()))
	}
	if raw == nil {
		return false, nil
	}
	var doc struct {
		Complete bool `json:"complete"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return true, nil // unreadable claim: fail closed toward skipping
	}
	return !doc.Complete, nil
}

// currentRefs reads the serving copy's ref tips (+ annotated-tag peel
// and HEAD symref): the handle's catchUp at Open already replayed the
// manifest's refs phase, so for-each-ref sees durable state.
func (s *Service) currentRefs(ctx context.Context, h *wal.RepoHandle) (tips, peeled map[string]string, head string, err error) {
	tips = map[string]string{}
	peeled = map[string]string{}
	all, ferr := s.git.ForEachRef(ctx, h.Dir())
	if ferr != nil {
		return nil, nil, "", fmt.Errorf("read refs: %v (safe to retry)", scrubText(ferr.Error()))
	}
	for _, r := range all {
		tips[r.Name] = r.Oid
		if r.Peeled != "" {
			peeled[r.Name] = r.Peeled
		}
	}
	return tips, peeled, HeadTarget(h.Dir()), nil
}

// ingestPacks uploads the scratch clone's packs as tier-0 entries
// (the import publishPack shape: idx install + AddPack + idx upload;
// resume skips checksums already durable). No tier-2 repack here —
// scheduled deltas stay small; compaction owns the base.
func (s *Service) ingestPacks(ctx context.Context, task *wal.Task, h *wal.RepoHandle, owner, name, scratch, formatName string, meta map[string]string) error {
	packs, _ := filepath.Glob(filepath.Join(scratch, "objects", "pack", "*.pack"))
	sort.Strings(packs)
	for i, pack := range packs {
		if st, serr := os.Stat(pack); serr == nil && s.maxBytes > 0 && st.Size() > s.maxBytes {
			return fmt.Errorf("pack %s exceeds limit (%d bytes)", filepath.Base(pack), s.maxBytes)
		}
		checksum, cerr := packTrailerChecksum(pack, formatName)
		if cerr != nil {
			return fmt.Errorf("pack %s: %v", filepath.Base(pack), scrubText(cerr.Error()))
		}
		task.Progress("ingest", uint64(i+1), uint64(len(packs)), "packs")
		if present, perr := s.packPresent(ctx, owner, name, checksum); perr != nil {
			return perr
		} else if present {
			if err := s.ensureIdxUploaded(ctx, h, owner, name, pack, checksum); err != nil {
				return err
			}
			continue
		}
		if err := s.publishPack(ctx, h, owner, name, pack, checksum, meta); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) packPresent(ctx context.Context, owner, name, checksum string) (bool, error) {
	meta, err := s.store.Head(ctx, store.RepoPrefix(owner, name)+store.PackKey(checksum))
	if err != nil {
		if store.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("probe pack %s: %v (safe to retry)", checksum, scrubText(err.Error()))
	}
	return meta != nil, nil
}

func (s *Service) idxPresent(ctx context.Context, owner, name, checksum string) (bool, error) {
	meta, err := s.store.Head(ctx, store.RepoPrefix(owner, name)+store.IdxKey(checksum))
	if err != nil {
		if store.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("probe idx %s: %v (safe to retry)", checksum, scrubText(err.Error()))
	}
	return meta != nil, nil
}

// publishPack installs the .idx into the serving copy BEFORE AddPack
// (its internal LevelServe Sync needs it locally) and uploads it to
// wal/<checksum>.idx after (a fresh instance materializes from the
// store alone — "warmth", law 4). The .idx upload is create-if-absent;
// a 412 loser is success (content-addressed bytes).
func (s *Service) publishPack(ctx context.Context, h *wal.RepoHandle, owner, name, packPath, checksum string, meta map[string]string) error {
	if _, err := s.installIdx(ctx, h, packPath, checksum); err != nil {
		return err
	}
	if _, err := h.AddPack(ctx, packPath, checksum, 0, meta); err != nil {
		return fmt.Errorf("publish pack %s: %v (safe to retry; orphan packs are inert)", checksum, scrubText(err.Error()))
	}
	return s.uploadIdx(ctx, h, owner, name, checksum)
}

func (s *Service) ensureIdxUploaded(ctx context.Context, h *wal.RepoHandle, owner, name, packPath, checksum string) error {
	present, err := s.idxPresent(ctx, owner, name, checksum)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	if _, err := s.installIdx(ctx, h, packPath, checksum); err != nil {
		return err
	}
	return s.uploadIdx(ctx, h, owner, name, checksum)
}

func (s *Service) installIdx(ctx context.Context, h *wal.RepoHandle, packPath, checksum string) (string, error) {
	idxSrc, err := s.git.EnsurePackIdx(ctx, packPath)
	if err != nil {
		return "", fmt.Errorf("pack %s: %v", filepath.Base(packPath), scrubText(err.Error()))
	}
	servingIdx := filepath.Join(h.Repo().PackDir(), "pack-"+checksum+".idx")
	if idxSrc != servingIdx {
		raw, rerr := os.ReadFile(idxSrc)
		if rerr != nil {
			return "", fmt.Errorf("read idx: %v", scrubText(rerr.Error()))
		}
		tmp := servingIdx + ".tmp"
		if werr := os.WriteFile(tmp, raw, 0o644); werr != nil {
			_ = os.Remove(tmp)
			return "", fmt.Errorf("install idx: %v", scrubText(werr.Error()))
		}
		if rerr := os.Rename(tmp, servingIdx); rerr != nil {
			_ = os.Remove(tmp)
			return "", fmt.Errorf("install idx: %v", scrubText(rerr.Error()))
		}
	}
	return servingIdx, nil
}

func (s *Service) uploadIdx(ctx context.Context, h *wal.RepoHandle, owner, name, checksum string) error {
	servingIdx := filepath.Join(h.Repo().PackDir(), "pack-"+checksum+".idx")
	idxBytes, rerr := os.ReadFile(servingIdx)
	if rerr != nil {
		return fmt.Errorf("read idx: %v", scrubText(rerr.Error()))
	}
	_, uerr := store.PutBytes(ctx, s.store, store.RepoPrefix(owner, name)+store.IdxKey(checksum), idxBytes,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream"})
	if uerr != nil && !store.IsPreconditionFailed(uerr) {
		return fmt.Errorf("upload idx %s: %v", checksum, scrubText(uerr.Error()))
	}
	return nil
}

// --- bucket lease (R1 (d)) ---------------------------------------------------

// acquireLease takes leases/mirror-<owner>-<name>.pb via the CAS
// ladder (absent → Create epoch 0; expired → Update steal with
// epoch+1; otherwise held → skip). Skew is 0 (the bundle-lease
// shape). Never waits, never retries within the fire.
func (s *Service) acquireLease(ctx context.Context, owner, name string) (func(), error) {
	key := store.LeaseKey(store.MirrorLeaseName(owner, name))
	holder := s.hostname
	for range 8 {
		body, meta, err := store.GetBytes(ctx, s.store, key, store.GetOptions{})
		if err != nil && !store.IsNotFound(err) {
			return nil, err
		}
		now := s.now().UTC()
		if body == nil {
			lease := &proto.Lease{Holder: holder, Purpose: KindMirrorSync}
			acq, exp := proto.TimeFromGo(now), proto.TimeFromGo(now.Add(leaseTTL))
			lease.AcquiredAt, lease.ExpiresAt, lease.Epoch = &acq, &exp, 0
			put, err := s.store.Put(ctx, key, store.PutBody{Bytes: lease.Marshal()},
				store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"})
			if err == nil {
				return s.releaseFunc(key, holder, put.Version), nil
			}
			if !store.IsPreconditionFailed(err) {
				return nil, err
			}
			continue
		}
		cur := &proto.Lease{}
		if err := cur.Unmarshal(body); err != nil {
			return nil, err
		}
		var expires time.Time
		if cur.ExpiresAt != nil {
			expires = cur.ExpiresAt.Go()
		}
		if now.Before(expires) {
			return nil, errLeaseHeld
		}
		next := *cur
		next.Holder = holder
		next.Purpose = KindMirrorSync
		next.Epoch = cur.Epoch + 1
		acq, exp := proto.TimeFromGo(now), proto.TimeFromGo(now.Add(leaseTTL))
		next.AcquiredAt, next.ExpiresAt = &acq, &exp
		put, err := s.store.Put(ctx, key, store.PutBody{Bytes: next.Marshal()},
			store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/x-protobuf"})
		if err == nil {
			return s.releaseFunc(key, holder, put.Version), nil
		}
		if !store.IsPreconditionFailed(err) {
			return nil, err
		}
	}
	return nil, errLeaseHeld
}

var errLeaseHeld = fmt.Errorf("mirror: sync lease held")

// releaseFunc deletes the lease only if it still names us (a stolen
// lease is left for its new owner).
func (s *Service) releaseFunc(key, holder string, _ store.Version) func() {
	return func() {
		body, meta, err := store.GetBytes(context.Background(), s.store, key, store.GetOptions{})
		if err != nil || body == nil {
			return
		}
		cur := &proto.Lease{}
		if cur.Unmarshal(body) != nil || cur.Holder != holder {
			return
		}
		_ = s.store.Delete(context.Background(), key, meta.Version)
	}
}

// --- scheduled loop (R1 (d)) -------------------------------------------------

// RunLoop is the mirror goroutine (the follow.go shape: its own
// cadence, never a maintenance unit, never blocking maintenance):
// every interval it enumerates the in-memory repo registry and fires
// due mirrors. Placement-gated by maintain (nil = fire everywhere —
// single-instance default). Overdue-after-restart fires ONCE (the
// fire stamps last_synced_at, moving the computed next fire into the
// future — no N catch-ups). Deleting mirror.json stops the loop for
// that repo (probe-absent → skip — no extra machinery).
func (s *Service) RunLoop(ctx context.Context, interval time.Duration, maintain func(string) bool) {
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.round(ctx, maintain)
		}
	}
}

// Round runs one enumeration pass (exported for tests; RunLoop calls
// it on the ticker).
func (s *Service) Round(ctx context.Context, maintain func(string) bool) {
	s.round(ctx, maintain)
}

func (s *Service) round(ctx context.Context, maintain func(string) bool) {
	defer func() {
		if r := recover(); r != nil {
			// A panicking repo must not kill the loop (law 7: narrate, never silent-stall).
			_ = r
		}
	}()
	if s.reg == nil {
		return
	}
	now := s.now()
	for _, repo := range s.reg.List() {
		if ctx.Err() != nil {
			return
		}
		if maintain != nil && !maintain(repo) {
			continue
		}
		owner, name, ok := splitRepo(repo)
		if !ok {
			continue
		}
		doc, _, err := Load(ctx, s.store, owner, name)
		if err != nil || doc == nil {
			continue // probe-absent or unreadable: skip (no LIST, one cheap probe per repo)
		}
		if !Due(doc, now) {
			// Not due — but a serve-health marker means the mirror is
			// degraded: fire the self-heal when its backoff elapses
			// (heal.go). Due mirrors skip the heal here: their sync
			// carries its own post-publish probe, and a no-op sync
			// leaves the marker for the next round (one minute out).
			s.maybeHeal(ctx, owner, name, now)
			continue
		}
		if _, serr := s.SyncNow(ctx, owner, name, "", false); serr != nil && ctx.Err() == nil {
			// SyncNow errors here are spawn errors (the body narrates
			// itself via the task); keep the loop moving.
			_ = serr
		}
	}
}

// maybeHeal fires one heal task for a marked repo whose backoff
// elapsed (issue #320, heal.go). Fire-and-forget: the heal is long
// patient work and must not stall the enumeration of other mirrors;
// the (repo,mirror-heal) single-flight joins overlapping fires and
// the task narrates itself regardless of listeners. SyncNow is NOT
// reused: a heal is not a sync (no lease, no clone, no mirror-doc
// touch) — only the task table is shared.
func (s *Service) maybeHeal(ctx context.Context, owner, name string, now time.Time) {
	doc, ok := wal.LoadServeHealth(ctx, s.store, owner, name)
	if !ok || doc == nil || !healDue(doc, now) {
		return
	}
	target := owner + "/" + name
	go func() {
		_, _ = s.reg.Tasks().Run(ctx, target, KindMirrorHeal,
			map[string]string{"trigger": "serve-degraded"},
			func(tctx context.Context, task *wal.Task) error {
				return s.runHeal(tctx, task, owner, name)
			})
	}()
}

func splitRepo(repo string) (owner, name string, ok bool) {
	o, n, found := splitOnce(repo, "/")
	if !found || o == "" || n == "" {
		return "", "", false
	}
	return o, n, true
}

func splitOnce(s, sep string) (string, string, bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

// --- small helpers -----------------------------------------------------------

func isZeroHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

// zeroHex renders the all-zero absent marker in the ref's hash length
// (40 sha1 / 64 sha256 — the format travels with the update, never
// assumed).
func zeroHex(n int) string {
	if n <= 0 {
		n = 40
	}
	out := make([]byte, n)
	for i := range out {
		out[i] = '0'
	}
	return string(out)
}

func shortOid(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func shortList(refs []string) string {
	if len(refs) > 5 {
		refs = refs[:5]
	}
	out := ""
	for i, r := range refs {
		if i > 0 {
			out += ", "
		}
		out += r
	}
	return out
}

// classifySyncError maps clone failures onto task errors (upstream
// auth failure is a task error, never walhub-auth 401 — v1 scheduled
// fires are anonymous; a 401/403 here means the upstream went
// private, which backoff + narration cover).
func classifySyncError(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "clone interrupted; safe to retry"
	}
	return scrubText(err.Error())
}

// packTrailerChecksum reads the pack trailer (20 bytes sha1, 32
// sha256 — format-aware): the content address AddPack publishes.
func packTrailerChecksum(path, format string) (string, error) {
	size := 20
	if format == "sha256" {
		size = 32
	}
	f, err := os.Open(path) //nolint:gosec // scratch + serving-copy paths only
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only trailer probe
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() < int64(size) {
		return "", fmt.Errorf("%s: not a pack file", filepath.Base(path))
	}
	if _, err := f.Seek(-int64(size), 2); err != nil {
		return "", err
	}
	trailer := make([]byte, size)
	if _, err := f.Read(trailer); err != nil {
		return "", err
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, size*2)
	for _, b := range trailer {
		out = append(out, hexdigits[b>>4], hexdigits[b&0xf])
	}
	return string(out), nil
}
