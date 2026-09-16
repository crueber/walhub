// service.go — the push-mirror sync engine: the Seam 5 task, the bucket
// lease, the on-push enqueue, and the scheduled loop.
//
// ### Concurrency
//
// Hazard: two instances firing the same repo would run two full pushes
// and race outcomes; the (repo,kind) single-flight is instance-memory
// only and does not span instances.
// Avoidance: the sync body takes the bucket lease
// (leases/pushmirror-<owner>-<name>.pb, CAS+TTL, skew 0 — the
// pull-mirror lease shape) BEFORE pushing. A held lease is a skip +
// narrate, never a wait, never a retry within the fire. The scheduled
// loop is one goroutine (RunLoop) exiting via ctx; every fire is a task
// whose goroutines derive from the task ctx (drain cancels everything);
// no lock is held across store/network calls. The push itself holds a
// read guard across object access (the upload-pack reader shape);
// removals TryLock-or-defer around it.
package pushmirror

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/repoimport"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// leaseTTL bounds one sync fire's cross-instance exclusion (push of a
// human-scale repo; the loop never waits on it).
const leaseTTL = 10 * time.Minute

// kinds registry (Seam 5): one kind, registered once from composition —
// a duplicate panics (the maintain.RegisterKind contract in code terms).
var (
	kindsMu sync.Mutex
	kinds   = map[string]bool{}
)

// RegisterKind records a task-kind name; a duplicate panics.
func RegisterKind(name string) {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	if kinds[name] {
		panic(fmt.Sprintf("pushmirror: duplicate task kind %q", name))
	}
	kinds[name] = true
}

// ResetKindsForTest clears the kind registry. Test-only: the registry is
// process-global and write-only (composition registers once per
// process), so without a reset every -count=N re-run of a registering
// test trips the duplicate panic.
func ResetKindsForTest() {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	kinds = map[string]bool{}
}

// Service owns the push-mirror surface: the task spawn (SyncNow,
// shared by on-push, the loop, and the manual endpoint), the on-push
// enqueue (EnqueueOnPush), and the scheduled loop (RunLoop).
type Service struct {
	store    store.ObjectStore
	reg      *wal.Registry
	git      *Runner
	hostname string
	now      func() time.Time

	asyncMu sync.Mutex
	async   map[string]*asyncSync // id-keyed async fires (HTTP 202 surface)

	// SSRF gate input, from the [import] section (one gate, one clock
	// for both mirror directions — no new config section).
	allowPrivate bool
	allowlist    []string
	allowFile    bool
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

	PushTimeout time.Duration
	GitTimeout  time.Duration

	AllowPrivate bool
	Allowlist    []string
	AllowFile    bool
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
	return &Service{
		store:        d.Store,
		reg:          d.Reg,
		git:          NewRunner(d.GitBinary, d.CacheDir, d.PushTimeout, d.GitTimeout),
		hostname:     host,
		now:          now,
		allowPrivate: d.AllowPrivate,
		allowlist:    d.Allowlist,
		allowFile:    d.AllowFile,
	}
}

// ValidateTarget checks an upstream URL against the chosen auth kind:
//
//	file:// → auth none only (e2e + fixture path; gated by AllowFile at the HTTP layer).
//	https:// → none|password|token (token over plaintext http refused).
//	http:// → none only (never send secrets over plaintext).
//	ssh:// + scp-like (git@host:path) → ssh only (no key agent ambiguity: the key file is the agent).
//	git:// → always refused (no auth possible, plaintext by construction).
func ValidateTarget(n repoimport.Normalized, authKind string, hasSecret bool) error {
	switch n.Scheme {
	case "file":
		if authKind != AuthNone {
			return fmt.Errorf("pushmirror: file:// upstreams take no auth (auth_kind must be %q)", AuthNone)
		}
		return nil
	case "https":
		switch authKind {
		case AuthNone, AuthPassword, AuthToken:
			return nil
		default:
			return fmt.Errorf("pushmirror: https upstreams take auth_kind none|password|token")
		}
	case "http":
		if authKind != AuthNone {
			return fmt.Errorf("pushmirror: token requires https (never send credentials over plaintext http)")
		}
		return nil
	case "ssh", "scp":
		if authKind != AuthSSH {
			return fmt.Errorf("pushmirror: ssh upstreams take auth_kind ssh (a deploy key)")
		}
		return nil
	default:
		return fmt.Errorf("pushmirror: unsupported upstream scheme %q", n.Scheme)
	}
}

// SyncNow spawns (or joins, via the (repo,mirror-push-sync)
// single-flight) one sync fire and awaits its outcome. Callers with a
// request ctx that must not block (HTTP handlers, the on-push hook) use
// SyncAsync instead — law 7 (a push is bulk work, and the task narrates
// itself regardless of listeners).
func (s *Service) SyncNow(ctx context.Context, owner, name string, force bool) (*wal.TaskRecord, error) {
	if s.reg == nil {
		return nil, fmt.Errorf("pushmirror: registry not configured")
	}
	doc, _, err := Load(ctx, s.store, owner, name)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("pushmirror: %s/%s has no push mirror", owner, name)
	}
	target := owner + "/" + name
	params := map[string]string{
		"upstream_url": doc.UpstreamURL,
		"auth_kind":    doc.AuthKind,
	}
	if force {
		params["force"] = "true"
	}
	return s.reg.Tasks().Run(ctx, target, KindPushMirrorSync, params,
		func(tctx context.Context, task *wal.Task) error {
			return s.runPush(tctx, task, owner, name, force)
		})
}

// asyncSync is one service-level async fire (the HTTP shape): the id is
// minted up front so the 202 carries it; the outcome lands on
// done-close. The table's (repo,kind) single-flight still joins
// overlapping fires inside the body — this map never dedups, it only
// names (the pull-mirror B4 id-keyed rings, minimal form).
type asyncSync struct {
	id      string
	target  string
	started string
	done    chan struct{}
	rec     *wal.TaskRecord
	err     error
}

// SyncAsync spawns a fire without blocking (HTTP handlers, on-push):
// the task body runs on a detached ctx (client disconnect cannot cancel
// it; drain still cancels via the table), and the returned id resolves
// via SyncStatus.
func (s *Service) SyncAsync(ctx context.Context, owner, name string, force bool) string {
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
		rec, err := s.SyncNow(context.WithoutCancel(ctx), owner, name, force)
		a.rec, a.err = rec, err
		close(a.done)
	}()
	return id
}

// SyncStatus resolves an async id (ok=false when unknown).
func (s *Service) SyncStatus(id string) (a *asyncSync, ok bool) {
	s.asyncMu.Lock()
	defer s.asyncMu.Unlock()
	a, ok = s.async[id]
	return a, ok
}

// RecentSyncs returns the finished push-mirror task records for a
// target (the table's recent ring, kind-filtered).
func (s *Service) RecentSyncs(target string) []*wal.TaskRecord {
	if s.reg == nil {
		return nil
	}
	var out []*wal.TaskRecord
	for _, rec := range s.reg.Tasks().List(target) {
		if rec != nil && rec.Kind == KindPushMirrorSync {
			out = append(out, rec)
		}
	}
	return out
}

func newSyncID() string {
	return fmt.Sprintf("psync-%d", time.Now().UTC().UnixNano())
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
	}
	var all []kv
	for id, a := range m {
		select {
		case <-a.done:
			all = append(all, kv{id, a.started})
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

// EnqueueOnPush is the server OnPush hook body (Forgejo #623): after a
// successful client push lands, probe the config sidecar (one exact-key
// GET, 404s free — the ONLY store trip on this path, off the push
// response) and fire an async sync when configured. It returns
// immediately — the fire runs on its own goroutine — so the push
// response is never gated (law 6: on-push enqueue is cheap).
//
// Server-side publishes (pull-mirror sync, merge tasks) never enter
// pushPipeline, so they never reach this hook (sync.go:434 bypass) —
// the exclusion is structural, pinned by test.
func (s *Service) EnqueueOnPush(owner, name string) {
	if s == nil || s.store == nil || s.reg == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if !HasConfig(ctx, s.store, owner, name) {
			return
		}
		s.SyncAsync(ctx, owner, name, false)
	}()
}

// runPush is the task body: lease → load config+secret → validate →
// Sync(LevelServe) materialize → namespace-refspec push → record outcome.
// Skips (held lease, removed config) narrate and return nil — a skip is
// not a failure and never touches the failure counter. Failures record
// consecutive_failures + the scrubbed reason and return the error
// (task terminal). Secrets are memory-only and scrubbed at every
// narration point.
func (s *Service) runPush(ctx context.Context, task *wal.Task, owner, name string, force bool) error {
	target := owner + "/" + name
	now := s.now()
	task.Notice(fmt.Sprintf("push-mirror sync %s (force=%v)", target, force))

	release, lerr := s.acquireLease(ctx, owner, name)
	if lerr != nil {
		task.Notice(fmt.Sprintf("push-mirror sync %s skipped: another instance holds the sync lease", target))
		return nil
	}
	defer release()

	doc, _, err := Load(ctx, s.store, owner, name)
	if err != nil {
		return s.fail(ctx, owner, name, now, task, err.Error())
	}
	if doc == nil {
		task.Notice(fmt.Sprintf("push-mirror sync %s skipped: configuration removed", target))
		return nil
	}
	if !force && BackedOff(doc, now) {
		task.Notice(fmt.Sprintf("push-mirror sync %s skipped: in failure backoff (%d consecutive failures)", target, doc.ConsecutiveFailures))
		return nil
	}

	n, err := repoimport.NormalizeSource(doc.UpstreamURL)
	if err != nil {
		return s.fail(ctx, owner, name, now, task, err.Error())
	}
	sec, _, err := LoadSecret(ctx, s.store, owner, name)
	if err != nil {
		return s.fail(ctx, owner, name, now, task, err.Error())
	}
	if verr := ValidateTarget(n, doc.AuthKind, sec != nil && sec.HasMaterial()); verr != nil {
		return s.fail(ctx, owner, name, now, task, verr.Error())
	}
	if serr := repoimport.CheckSSRF(n, repoimport.SSRFConfig{
		AllowPrivate: s.allowPrivate,
		Allowlist:    s.allowlist,
		AllowFile:    s.allowFile,
		Dangerous:    true, // authority checked at admin-only config time; stored URL re-gates host/private only
	}, nil); serr != nil {
		return s.fail(ctx, owner, name, now, task, serr.Error())
	}
	auth, aerr := s.resolveAuth(doc, sec, n)
	if aerr != nil {
		return s.fail(ctx, owner, name, now, task, aerr.Error())
	}

	h, oerr := s.reg.Open(ctx, target)
	if oerr != nil {
		return s.fail(ctx, owner, name, now, task, fmt.Sprintf("open repo: %v", oerr))
	}
	// Materialize what the store publishes (the pull direction's ref
	// reconstruction, reversed): Sync applies the manifest's refs into
	// the serving copy, and the push ships that copy. The read guard is
	// held across the transfer (the upload-pack reader shape).
	g, serr := h.Sync(ctx, wal.LevelServe)
	if serr != nil {
		return s.fail(ctx, owner, name, now, task, fmt.Sprintf("materialize: %v (safe to retry)", scrubText(serr.Error())))
	}
	defer g.Release()

	task.Notice(fmt.Sprintf("push-mirror sync %s: pushing to %s", target, n.URL))
	learned, perr := s.git.Push(ctx, h.Dir(), n.URL, auth)
	if perr != nil {
		return s.fail(ctx, owner, name, now, task, classifyPushError(ctx, perr))
	}
	task.Notice(fmt.Sprintf("push-mirror sync %s: pushed", target))
	if auth.Kind == AuthSSH {
		s.harvestHostKey(ctx, task, target, owner, name, learned, now)
	}
	return s.succeed(ctx, owner, name, now)
}

// harvestHostKey is the single post-push harvest point (Forgejo #625):
// the scheduled loop, on-push fan-out, and sync-now all funnel through
// runPush, so all three harvest identically here — no per-trigger
// forks. After a successful SSH push the learned accept-new lines merge
// into the secret sidecar (operator pins stay authoritative) and the
// fingerprint narrates (presence-style, never key material).
//
// Failure semantics: a harvest miss never fails the sync — the outcome
// stays ok, the miss narrates, and the next accept-new fire re-learns
// and retries.
//
// ### Concurrency
//
// Hazard: the serving-copy read guard (g, released by defer) is still
// held here, and harvest takes store round trips.
// Avoidance: the guard only fences pack removal (TryLock-or-defer
// around it) — the secret-sidecar CAS never contends with it, and the
// push itself already held the guard across network I/O (the
// upload-pack reader shape). No new lock, no new ordering.
func (s *Service) harvestHostKey(ctx context.Context, task *wal.Task, target, owner, name, learned string, now time.Time) {
	if strings.TrimSpace(learned) == "" {
		return
	}
	fp, added, herr := RecordHostKeyTrust(ctx, s.store, owner, name, learned, now)
	if herr != nil {
		task.Notice(fmt.Sprintf("push-mirror sync %s: pushed, but host-key trust was not recorded (%s; will retry next sync)", target, scrubText(herr.Error())))
		return
	}
	if added && fp != "" {
		task.Notice(fmt.Sprintf("push-mirror sync %s: learned host key %s (pinned for future syncs)", target, fp))
	}
}

// resolveAuth builds the memory-only push credential from the config +
// secret sidecars. Missing material for a material-needing kind is a
// verdict (failed outcome), never a silent anonymous push — silently
// downgrading to anonymous would push (or fail to push) under the wrong
// identity.
func (s *Service) resolveAuth(doc *Doc, sec *Secret, n repoimport.Normalized) (PushAuth, error) {
	auth := PushAuth{Kind: doc.AuthKind, Scheme: n.Scheme, Host: n.Host}
	switch doc.AuthKind {
	case AuthNone:
		return auth, nil
	case AuthPassword:
		if sec == nil || sec.Password == "" {
			return auth, fmt.Errorf("pushmirror: password auth configured but no password stored")
		}
		auth.Username = doc.Username
		if auth.Username == "" {
			auth.Username = sec.Username
		}
		auth.Password = sec.Password
		return auth, nil
	case AuthToken:
		if sec == nil || sec.Token == "" {
			return auth, fmt.Errorf("pushmirror: token auth configured but no token stored")
		}
		auth.Username = doc.Username
		if auth.Username == "" {
			auth.Username = sec.Username
		}
		auth.Token = sec.Token
		return auth, nil
	case AuthSSH:
		if sec == nil || sec.SSHPrivateKey == "" {
			return auth, fmt.Errorf("pushmirror: ssh auth configured but no private key stored")
		}
		auth.PrivateKey = sec.SSHPrivateKey
		auth.KnownHosts = sec.SSHKnownHosts
		auth.Fingerprint = doc.KeyFingerprint
		return auth, nil
	}
	return auth, fmt.Errorf("pushmirror: unknown auth kind %q", doc.AuthKind)
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
		task.Notice(fmt.Sprintf("push-mirror sync %s/%s: outcome record lost: %v", owner, name, rerr))
	}
	task.Notice(fmt.Sprintf("push-mirror sync %s/%s failed: %s", owner, name, reason))
	return fmt.Errorf("push-mirror sync: %s", reason)
}

// --- bucket lease ---------------------------------------------------

// acquireLease takes leases/pushmirror-<owner>-<name>.pb via the CAS
// ladder (absent → Create epoch 0; expired → Update steal with
// epoch+1; otherwise held → skip). Never waits, never retries within
// the fire.
func (s *Service) acquireLease(ctx context.Context, owner, name string) (func(), error) {
	key := store.LeaseKey(store.PushMirrorLeaseName(owner, name))
	holder := s.hostname
	for range 8 {
		body, meta, err := store.GetBytes(ctx, s.store, key, store.GetOptions{})
		if err != nil && !store.IsNotFound(err) {
			return nil, err
		}
		now := s.now().UTC()
		if body == nil {
			lease := &proto.Lease{Holder: holder, Purpose: KindPushMirrorSync}
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
		next.Purpose = KindPushMirrorSync
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

var errLeaseHeld = fmt.Errorf("pushmirror: sync lease held")

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

// --- scheduled loop -------------------------------------------------

// RunLoop is the push-mirror scheduled goroutine (the follow.go shape:
// its own cadence, never a maintenance unit, never blocking
// maintenance): every interval it enumerates the in-memory repo
// registry and fires due mirrors. Placement-gated by maintain (nil =
// fire everywhere — single-instance default). Overdue-after-restart
// fires ONCE (the fire stamps last_synced_at). Deleting the config
// stops the loop for that repo (probe-absent → skip). Repos with
// scheduling OFF ("") never fire here — on-push is their only trigger.
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
			// A panicking repo must not kill the loop (narrate, never silent-stall).
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
			continue
		}
		if _, serr := s.SyncNow(ctx, owner, name, false); serr != nil && ctx.Err() == nil {
			// Spawn errors narrate via the task; keep the loop moving.
			_ = serr
		}
	}
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

// classifyPushError maps push failures onto task errors (an upstream
// 401/403 here means the upstream went private or the credential died —
// backoff + narration cover it; it is never walhub-auth 401).
func classifyPushError(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "push interrupted; safe to retry"
	}
	return scrubText(err.Error())
}
