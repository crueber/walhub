// publish.go — the publish path (doc 05 §5.3): one single-flight publisher
// goroutine per repo with 5ms/64 group commit, the per-attempt CAS ladder
// (verify → upload ∥ slot claim → manifest CAS → local commit), the orphan
// burn protocol (§5.4), and publishCompact/publishSettings/annotatePack.
package wal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"github.com/BurntSushi/toml"
)

// PreparedPack is a pack ready to publish (receive-pack output, add-pack file).
type PreparedPack struct {
	Checksum    string // pack trailing SHA, hex
	PackPath    string // local file path ("" for pack-less publishes)
	IdxPath     string
	PackSize    uint64
	IdxSize     uint64
	ObjectCount uint64
	Tier        uint32
}

// PublishRequest is one push waiting in the group-commit window (§5.3).
// The Settings/Compact/Supersedes fields route publishSettings/publishCompact/
// addPack through the same funnel (one publisher per repo).
type PublishRequest struct {
	Pack      *PreparedPack // nil for ref-only pushes
	Txn       *proto.RefTransaction
	Meta      map[string]string // principal, request_id, agent, push-options…
	Synced    bool              // receive-pack reuses its own freshness check
	CreatedAt *time.Time        // explicit entry time (monotonic guard applies)

	Compact    bool                // COMPACT entry instead of PUSH/REF_UPDATE
	Supersedes []string            // COMPACT: checksums this pack replaces
	Settings   *proto.RepoSettings // SETTINGS entry (publishSettings)
	Tier       uint32              // add-pack tier
}

// publishJob carries a request through the publisher with its own reply.
type publishJob struct {
	req   PublishRequest
	reply chan publishResult // owned by the caller; publisher sends, then closes
}

type publishResult struct {
	res PublishResult
	err error
}

// Publisher is the single-flight publisher goroutine per repo (§5.3).
// The channel is owned by the HANDLE and never closed on respawn — jobs
// enqueued during a respawn gap are retained.
type Publisher struct {
	h      *RepoHandle
	ch     chan *publishJob // cap wal.max_batch
	wg     sync.WaitGroup   // loop exit tracking for Close
	closed chan struct{}    // handle teardown signal
}

const (
	stripedUploadMin = 256 << 20 // ≥ 256 MiB → striped upload when compose is native
	orphanProbes     = 3
	orphanProbeWait  = 100 * time.Millisecond
	maxBurns         = 8

	errRestartLadder = walErrRestart("restart")
)

type walErrRestart string

func (e walErrRestart) Error() string { return string(e) }

// Publish enqueues req and awaits the result (exactly one reply per job).
func (h *RepoHandle) Publish(ctx context.Context, req PublishRequest) (PublishResult, error) {
	h.ensurePublisher()
	job := &publishJob{req: req, reply: make(chan publishResult, 1)}
	select {
	case h.pub.ch <- job:
	case <-ctx.Done():
		return PublishResult{}, ctx.Err()
	case <-h.pub.closed:
		return PublishResult{}, &WalError{Kind: WalErrRetry, Detail: "publisher shut down"}
	}
	select {
	case r := <-job.reply:
		return r.res, r.err
	case <-ctx.Done():
		return PublishResult{}, ctx.Err()
	}
}

func (h *RepoHandle) ensurePublisher() {
	h.pubOnce.Do(func() {
		h.pub = &Publisher{
			h:      h,
			ch:     make(chan *publishJob, h.reg.cfg.WAL.MaxBatch),
			closed: make(chan struct{}),
		}
		h.pub.wg.Add(1)
		go h.pub.loop() // 13 §1 row 3: publisher goroutine, exits on teardown
	})
}

// Close tears the publisher down. The handle closes ch exactly once at
// teardown — actually it signals `closed`; ch is never closed (respawn keeps
// the mailbox), and the loop drains before exiting.
func (p *Publisher) Close() {
	select {
	case <-p.closed:
		return
	default:
	}
	close(p.closed)
	p.wg.Wait()
}

// loop is the group-commit loop (§5.3.1 with the deviation-mandated variant):
// recv → try-drain ready jobs; the 5ms window starts only when a lone job
// arrived with nothing else immediately receivable. A lone push on an idle
// repo commits without waiting the window.
func (p *Publisher) loop() {
	defer p.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			logWarnf("publisher panic %s: %v — respawning", p.h.ID, r)
			go p.loop() // respawn replaces the loop, not the mailbox
		}
	}()
	batchWindow := time.Duration(p.h.reg.cfg.WAL.BatchWindow)
	if batchWindow <= 0 {
		batchWindow = 5 * time.Millisecond
	}
	maxBatch := p.h.reg.cfg.WAL.MaxBatch
	if maxBatch <= 0 {
		maxBatch = 64
	}
	for {
		var job *publishJob
		select {
		case job = <-p.ch:
		case <-p.closed:
			return
		}
		batch := []*publishJob{job}
		// try-drain: everything already receivable right now.
	drain:
		for len(batch) < maxBatch {
			select {
			case j := <-p.ch:
				batch = append(batch, j)
			default:
				break drain
			}
		}
		if len(batch) == 1 {
			// Nothing was receivable at drain time; give the window one shot.
			timer := time.NewTimer(batchWindow)
		collect:
			for len(batch) < maxBatch {
				select {
				case j := <-p.ch:
					batch = append(batch, j)
				case <-timer.C:
					break collect
				case <-p.closed:
					timer.Stop()
					p.runBatch(context.Background(), batch)
					return
				}
			}
			timer.Stop()
		}
		ctx, cancel := context.WithCancel(p.h.reg.ctx)
		p.runBatch(ctx, batch)
		cancel()
	}
}

// runBatch executes the per-attempt ladder (§5.3.2) for one merged batch.
// Guarantees: exactly one reply per job — sent exactly once, then closed.
func (p *Publisher) runBatch(ctx context.Context, batch []*publishJob) {
	defer func() {
		if r := recover(); r != nil {
			logWarnf("runBatch panic %s: %v", p.h.ID, r)
			p.replyAll(batch, PublishResult{}, &WalError{Kind: WalErrStore, Detail: fmt.Sprintf("internal panic: %v", r)})
		}
	}()

	// 1. Sync (refs+serve) unless every job is already synced.
	needSync := false
	for _, j := range batch {
		if !j.req.Synced {
			needSync = true
			break
		}
	}
	if needSync {
		g, err := p.h.Sync(ctx, LevelServe)
		if err != nil {
			p.replyAll(batch, PublishResult{}, err)
			return
		}
		g.Release()
	}

	// 2. Snapshot manifest + version; build the ref view overlay.
	p.h.syncMu.Lock()
	base, version := p.h.manifest, p.h.version
	lastEntry := p.h.lastEntryTime
	p.h.syncMu.Unlock()

	var overlay *refOverlay
	maxRetries := int(p.h.reg.cfg.WAL.CASMaxRetries)
	if maxRetries <= 0 {
		maxRetries = 16
	}
	burned := map[uint64]string{} // seq → segment key (batch-local burned list)

	for attempt := 1; ; attempt++ {
		if attempt > maxRetries {
			p.replyAll(batch, PublishResult{}, &WalError{Kind: WalErrRetry, Detail: fmt.Sprintf("%d attempts", maxRetries)})
			return
		}
		if attempt > 1 {
			publishCASRetries.Add(1)
		}

		// 3. Verify each txn against the CURRENT refs — the overlay is
		// rebuilt every attempt so a post-412 re-sync's ref view is honored.
		overlay = newRefOverlay(p.h)
		view := overlay.clone()
		valid := make([]*publishJob, 0, len(batch))
		perJob := make([]PublishResult, len(batch))
		anyValid := false
		prevTime := lastEntry
		for i, j := range batch {
			txErrs := verifyTxn(j.req.Txn, view)
			// Monotonic created_at (§5.3.2 step 3): an explicit time must be
			// ≥ the WAL head's last entry time, and within the batch ≥ the
			// previous job's explicit time.
			if j.req.CreatedAt != nil && len(txErrs) == 0 {
				if !prevTime.IsZero() && j.req.CreatedAt.Before(prevTime) {
					txErrs = append(txErrs, &RefError{Kind: RefErrRejected, Ref: "*",
						Detail: fmt.Sprintf("created_at %s precedes %s (monotonic guard)",
							j.req.CreatedAt.Format(time.RFC3339Nano), prevTime.Format(time.RFC3339Nano))})
				} else {
					prevTime = *j.req.CreatedAt
				}
			}
			if len(txErrs) > 0 {
				for _, e := range txErrs {
					perJob[i].PerRef = append(perJob[i].PerRef, RefResult{Name: e.Ref, Err: e})
				}
				continue
			}
			anyValid = true
			valid = append(valid, j)
			applyTxnToView(view, j.req.Txn)
		}
		if !anyValid {
			// Rejections are transport-successes: per-ref errors, Seq 0.
			for i, j := range batch {
				p.reply(j, perJob[i], nil)
			}
			return
		}

		// 4. Build entries: seq = firstSeq + position.
		firstSeq := base.HeadSeq + 1
		now := time.Now().UTC()
		entries := make([]*proto.LogEntry, 0, len(valid))
		entryJob := map[*proto.LogEntry]*publishJob{} // survives burn renumbering
		seqOf := map[*publishJob]uint64{}
		for _, j := range valid {
			e := &proto.LogEntry{
				Seq:    firstSeq + uint64(len(entries)),
				Writer: p.h.reg.instance,
				Meta:   j.req.Meta,
				Txn:    j.req.Txn,
			}
			if j.req.CreatedAt != nil {
				e.CreatedAt = TsPtr(j.req.CreatedAt.UTC())
			} else {
				e.CreatedAt = TsPtr(now)
			}
			switch {
			case j.req.Settings != nil:
				e.Kind = proto.EntryKindSettings
				e.Settings = j.req.Settings
			case j.req.Compact:
				e.Kind = proto.EntryKindCompact
				if j.req.Pack != nil {
					e.Pack = packRefOf(j.req.Pack, e.Seq, j.req.Tier)
				}
				e.Supersedes = j.req.Supersedes
			case j.req.Pack != nil:
				e.Kind = proto.EntryKindPush
				e.Pack = packRefOf(j.req.Pack, e.Seq, 0)
			default:
				e.Kind = proto.EntryKindRefUpdate
			}
			entryJob[e] = j
			entries = append(entries, e)
		}

		// 5. Concurrently: pack uploads ∥ log slot claim (joined below).
		upErr := make(chan error, 1)
		upsDone := make(chan struct{})
		go func() {
			defer close(upsDone)
			upErr <- p.uploadPacks(ctx, valid)
		}()
		segFirst, segKey, segVersion, segBody, claimErr := p.claimSlot(ctx, base, entries, burned)
		<-upsDone
		if err := <-upErr; err != nil {
			// Upload failure fails the whole batch (all jobs get the error;
			// packs may have landed as orphans — harmless, §5.0.3).
			p.replyAll(batch, PublishResult{}, err)
			return
		}
		if claimErr != nil {
			if errors.Is(claimErr, errRestartLadder) {
				if g, serr := p.h.Sync(ctx, LevelRefs); serr == nil {
					g.Release()
				}
				p.h.syncMu.Lock()
				base, version = p.h.manifest, p.h.version
				p.h.syncMu.Unlock()
				continue
			}
			p.replyAll(batch, PublishResult{}, claimErr)
			return
		}

		// claimSlot renumbered the entries (burns advance segFirst): remap.
		for idx, e := range entries {
			_ = idx
			seqOf[entryJob[e]] = e.Seq
		}

		// 6+7. Build the manifest update and CAS it.
		next := buildNextManifest(base, entries, firstSeq, segFirst, segBody, p.h.reg.instance)
		newVersion, committed, casErr := p.casManifest(ctx, version, segKey, segVersion, next)
		if casErr != nil {
			if errors.Is(casErr, errRestartLadder) {
				p.h.syncMu.Lock()
				base, version = p.h.manifest, p.h.version
				p.h.syncMu.Unlock()
				continue
			}
			p.replyAll(batch, PublishResult{}, casErr)
			return
		}
		if committed {
			p.commitLocal(ctx, next, newVersion, entries, burned)
		}
		// 8. Answer every waiter: valid → {Seq, per-ref ok}; invalid → errors.
		for i, j := range batch {
			res := perJob[i]
			if s, ok := seqOf[j]; ok && committed {
				res.Seq = s
			}
			if res.PerRef == nil {
				for _, u := range txnUpdates(j.req.Txn) {
					res.PerRef = append(res.PerRef, RefResult{Name: u.Name})
				}
			}
			p.reply(j, res, nil)
		}
		return
	}
}

func packRefOf(p *PreparedPack, seq uint64, tier uint32) *proto.PackRef {
	return &proto.PackRef{
		Checksum:    p.Checksum,
		PackSize:    p.PackSize,
		IdxSize:     p.IdxSize,
		ObjectCount: p.ObjectCount,
		Tier:        tier,
		Seq:         seq,
		Kind:        proto.PackKindObjects,
	}
}

// reply sends exactly one result and closes the reply channel (double-reply
// would panic on send-to-closed — a test-detectable defect).
func (p *Publisher) reply(j *publishJob, res PublishResult, err error) {
	j.reply <- publishResult{res: res, err: err}
	close(j.reply)
}

func (p *Publisher) replyAll(batch []*publishJob, res PublishResult, err error) {
	for _, j := range batch {
		p.reply(j, res, err)
	}
}

// ---- ref view overlay (§5.3.2 step 2: O(log n) lookups, never a full scan) --

// refOverlay is a sorted base snapshot + delta map; verification and batch
// application fold into the delta, so later jobs in the batch see earlier writes.
type refOverlay struct {
	base  *git.RefSnapshot
	delta map[string]git.RefEntry // present = known (oid "" = tombstone)
}

func newRefOverlay(h *RepoHandle) *refOverlay {
	snap, err := h.Layer().Snapshot(h.repo)
	if err != nil {
		snap = &git.RefSnapshot{}
	}
	return &refOverlay{base: snap, delta: map[string]git.RefEntry{}}
}

func (o *refOverlay) clone() *refOverlay {
	c := &refOverlay{base: o.base, delta: make(map[string]git.RefEntry, len(o.delta))}
	for k, v := range o.delta {
		c.delta[k] = v
	}
	return c
}

func (o *refOverlay) get(name string) (git.RefEntry, bool) {
	if e, ok := o.delta[name]; ok {
		if e.Oid == "" {
			return git.RefEntry{}, false
		}
		return e, true
	}
	return o.base.Get(name)
}

func (o *refOverlay) set(e git.RefEntry) { o.delta[e.Name] = e }

func (o *refOverlay) del(name string) { o.delta[name] = git.RefEntry{Name: name} }

// verifyTxn checks one txn against the view; per-update rules §5.3.2 step 3.
func verifyTxn(txn *proto.RefTransaction, view *refOverlay) []*RefError {
	if txn == nil {
		return []*RefError{{Kind: RefErrRejected, Ref: "*", Detail: "nil transaction"}}
	}
	var errs []*RefError
	for _, u := range txn.Updates {
		if err := git.ValidateRefUpdate(u); err != nil {
			errs = append(errs, &RefError{Kind: RefErrRejected, Ref: u.Name, Detail: err.Error()})
			continue
		}
		if u.NewSymbolicTarget != "" {
			continue // always ok
		}
		cur, exists := view.get(u.Name)
		if isZeroOid(u.OldOid) {
			if exists {
				errs = append(errs, &RefError{Kind: RefErrConflict, Ref: u.Name,
					Detail: fmt.Sprintf("expected absent, got %s", cur.Oid)})
			}
			continue
		}
		if !exists {
			errs = append(errs, &RefError{Kind: RefErrConflict, Ref: u.Name,
				Detail: fmt.Sprintf("expected %s, got absent", u.OldOid)})
			continue
		}
		if cur.Oid != u.OldOid {
			errs = append(errs, &RefError{Kind: RefErrConflict, Ref: u.Name,
				Detail: fmt.Sprintf("expected %s, got %s", u.OldOid, cur.Oid)})
		}
	}
	return errs
}

// applyTxnToView folds a verified txn into the working view (no re-checks).
func applyTxnToView(view *refOverlay, txn *proto.RefTransaction) {
	for _, u := range txn.Updates {
		if u.NewSymbolicTarget != "" {
			continue // HEAD moves are applied at commit time
		}
		if isZeroOid(u.NewOid) {
			view.del(u.Name)
			continue
		}
		view.set(git.RefEntry{Name: u.Name, Oid: u.NewOid, Peeled: u.NewPeeled})
	}
}

func isZeroOid(s string) bool {
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return len(s) == 40 || len(s) == 64
}

// ---- step 5a: pack uploads (create-if-absent, §5.3.2 step 5a) ----------------

// uploadPacks PUTs wal/<checksum>.pack and .idx create-if-absent (duplicate
// creates are success). ≥ 256 MiB and native compose → striped upload.
func (p *Publisher) uploadPacks(ctx context.Context, jobs []*publishJob) error {
	seen := map[string]bool{}
	for _, j := range jobs {
		if j.req.Pack == nil || seen[j.req.Pack.Checksum] {
			continue
		}
		seen[j.req.Pack.Checksum] = true
		if err := p.uploadPack(ctx, j.req.Pack); err != nil {
			return err
		}
	}
	return nil
}

func (p *Publisher) uploadPack(ctx context.Context, pack *PreparedPack) error {
	opts := store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream"}
	if pack.PackPath != "" && pack.PackSize >= stripedUploadMin && p.h.reg.st.ComposeIsNative() {
		f, err := os.Open(pack.PackPath)
		if err != nil {
			return &WalError{Kind: WalErrIo, Detail: pack.PackPath, Wrapped: err}
		}
		defer f.Close()
		if _, err := store.PutFileParallel(ctx, p.h.reg.st, store.PackKey(pack.Checksum), f, int64(pack.PackSize), opts); err != nil {
			if store.IsPreconditionFailed(err) {
				return nil // duplicate create is success
			}
			return &WalError{Kind: WalErrStore, Detail: store.PackKey(pack.Checksum), Wrapped: err}
		}
	} else if pack.PackPath != "" {
		if _, err := store.PutBytes(ctx, p.h.reg.st, store.PackKey(pack.Checksum), mustRead(pack.PackPath), opts); err != nil {
			if store.IsPreconditionFailed(err) {
				return nil
			}
			return &WalError{Kind: WalErrStore, Detail: store.PackKey(pack.Checksum), Wrapped: err}
		}
	}
	if pack.IdxPath != "" {
		if _, err := store.PutBytes(ctx, p.h.reg.st, store.IdxKey(pack.Checksum), mustRead(pack.IdxPath), opts); err != nil {
			if store.IsPreconditionFailed(err) {
				return nil
			}
			return &WalError{Kind: WalErrStore, Detail: store.IdxKey(pack.Checksum), Wrapped: err}
		}
	}
	return nil
}

// ---- step 5b: the log slot claim (§5.3.2 step 5b, §5.4 burn protocol) -------

// claimSlot PutCreates log/<seq:016x>.pb for the batch's entries. On a 412 the
// §5.4 sequence runs: fresh manifest → head_seq ≥ seq means the commit landed;
// else HEAD the slot: absent → retry create; present after 3 probes × 100 ms
// → burn the seq (cap 8 consecutive) and start a fresh segment.
// Returns the segment's first seq, key, CAS version and framed body.
func (p *Publisher) claimSlot(ctx context.Context, base *pbManifest, entries []*proto.LogEntry, burned map[uint64]string) (segFirst uint64, key string, version string, body []byte, err error) {
	segFirst = base.HeadSeq + 1
	burnStreak := 0
	for {
		if ctx.Err() != nil {
			return 0, "", "", nil, ctx.Err()
		}
		for i, e := range entries {
			e.Seq = segFirst + uint64(i) // renumber on every attempt/burn
		}
		body = proto.EncodeSegment(entries)
		key = store.LogSegmentKey(segFirst)
		meta, cerr := p.h.reg.st.Put(ctx, p.h.repoKey(key), store.PutBody{Bytes: body},
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"})
		if cerr == nil {
			return segFirst, key, string(meta.Version), body, nil
		}
		// Someone wrote that slot. Fresh manifest read (§5.4 step 1).
		fresh, ferr := p.freshHead(ctx)
		if ferr != nil && !store.IsNotFound(ferr) && ctx.Err() == nil {
			return 0, "", "", nil, &WalError{Kind: WalErrStore, Detail: "casLanded re-read", Wrapped: ferr}
		}
		if fresh != nil && fresh.HeadSeq >= segFirst {
			// The commit landed (possibly ours, lost response): re-sync,
			// restart the ladder.
			return 0, "", "", nil, errRestartLadder
		}

		// HEAD the slot (§5.4 step 2): absent → retry the Create.
		for probe := 0; probe < orphanProbes; probe++ {
			time.Sleep(orphanProbeWait)
			exists, herr := store.Exists(ctx, p.h.reg.st, p.h.repoKey(key))
			if herr != nil {
				return 0, "", "", nil, &WalError{Kind: WalErrStore, Detail: key, Wrapped: herr}
			}
			if !exists {
				break // slot freed: retry the create
			}
			if probe == orphanProbes-1 {
				// Orphan (a crashed writer): burn the seq (§5.4 step 3).
				if burnStreak++; burnStreak > maxBurns {
					return 0, "", "", nil, &WalError{Kind: WalErrCorrupt,
						Detail: fmt.Sprintf("%s: %d consecutive orphan burns at seq %d", p.h.ID, burnStreak-1, segFirst)}
				}
				burned[segFirst] = key
				orphansBurned.Add(1)
				segFirst++
				burnStreakReset(&burnStreak)
				break
			}
		}
	}
}

// burnStreakReset is a no-op seam: the consecutive-burn cap counts burns
// within one claim loop (§5.4 step 3, cap 8 → ErrCorrupt).
func burnStreakReset(n *int) {}

// freshHead re-reads the manifest straight from the store (bypasses the guard).
func (p *Publisher) freshHead(ctx context.Context) (*pbManifest, error) {
	body, _, err := store.GetBytes(ctx, p.h.reg.st, manifestKey(p.h.ID), store.GetOptions{})
	if err != nil {
		return nil, err
	}
	if body == nil {
		return nil, nil
	}
	return proto.UnmarshalManifest(body)
}

// ---- steps 6+7: manifest build and CAS --------------------------------------

// buildNextManifest applies the batch to a copy of base (§5.3.2 step 6).
func buildNextManifest(base *pbManifest, entries []*proto.LogEntry, firstSeq, segFirst uint64, segBody []byte, writer string) *pbManifest {
	next := *base // shallow copy; we rebuild the mutated slices
	lastSeq := firstSeq - 1 + uint64(len(entries))
	if len(entries) > 0 {
		lastSeq = entries[len(entries)-1].Seq
	}
	next.HeadSeq = lastSeq

	packs := make([]*proto.PackRef, 0, len(base.Packs)+len(entries))
	keep := map[string]bool{}
	for _, p := range base.Packs {
		keep[p.Checksum] = true
		packs = append(packs, p)
	}
	for _, e := range entries {
		switch e.Kind {
		case proto.EntryKindPush:
			if e.Pack != nil {
				packs = append(packs, e.Pack)
			}
		case proto.EntryKindCompact:
			for _, dead := range e.Supersedes {
				delete(keep, dead)
			}
			if e.Pack != nil {
				packs = append(packs, e.Pack)
			}
		case proto.EntryKindSettings:
			next.Settings = e.Settings
		}
	}
	// Superseded removal only applies to packs still referenced.
	live := packs[:0]
	seen := map[string]bool{}
	for _, p := range packs {
		if !keep[p.Checksum] || seen[p.Checksum] {
			continue
		}
		seen[p.Checksum] = true
		live = append(live, p)
	}
	sortPackRefs(live)
	next.Packs = live

	segs := make([]*proto.LogSegmentRef, 0, len(base.LogSegments)+1)
	segs = append(segs, base.LogSegments...)
	segs = append(segs, &proto.LogSegmentRef{
		Key:      store.LogSegmentKey(segFirst),
		FirstSeq: segFirst,
		LastSeq:  lastSeq,
		Size:     uint64(len(segBody)),
		Sealed:   true,
	})
	sort.Slice(segs, func(i, j int) bool { return segs[i].FirstSeq < segs[j].FirstSeq })
	next.LogSegments = segs

	next.UpdatedAt = TsPtr(time.Now().UTC())
	next.Writer = writer
	next.Revision = base.Revision + 1
	return &next
}

func sortPackRefs(ps []*proto.PackRef) {
	sort.Slice(ps, func(i, j int) bool { return ps[i].Seq < ps[j].Seq })
}

// casManifest runs step 7: PutUpdate(version) (or PutCreate when no version
// is held — a repo created before its first publish). Ok → committed. 412 →
// delete our own segment (CAS delete of exactly our version, so a racing
// writer reading it is never hurt) and restart the ladder. Any other error →
// casLanded: fresh manifest re-read; if it lists our segment we are committed
// (recover the version via HEAD); otherwise NOT committed — the segment stays
// (a lost-response commit the re-read failed to observe must not be destroyed;
// a later writer burns past it).
func (p *Publisher) casManifest(ctx context.Context, version, segKey, segVersion string, next *pbManifest) (string, bool, error) {
	key := manifestKey(p.h.ID)
	opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: store.Version(version), ContentType: "application/x-protobuf"}
	if version == "" {
		opts.Mode = store.PutCreate
	}
	meta, perr := p.h.reg.st.Put(ctx, key, store.PutBody{Bytes: next.Marshal()}, opts)
	if perr == nil {
		return string(meta.Version), true, nil
	}
	if store.IsPreconditionFailed(perr) {
		// Delete exactly our own log segment (CAS delete of our version).
		if segVersion != "" {
			_ = p.h.reg.st.Delete(ctx, p.h.repoKey(segKey), store.Version(segVersion))
		}
		return "", false, errRestartLadder
	}
	// Ambiguous: the write may have landed and the response was lost.
	fresh, ferr := p.freshHead(ctx)
	if ferr != nil {
		return "", false, &WalError{Kind: WalErrStore, Detail: "casLanded re-read", Wrapped: ferr}
	}
	if fresh != nil {
		for _, s := range fresh.LogSegments {
			if s.Key == segKey {
				// Committed; recover the version via HEAD.
				if m, herr := p.h.reg.st.Head(ctx, key); herr == nil && m != nil {
					return string(m.Version), true, nil
				}
				return "", true, nil
			}
		}
	}
	// NOT committed: leave the segment in place; a later writer burns past it.
	return "", false, errRestartLadder
}

// commitLocal is step 8 (§5.3.2 + §3.2 ordering rules): local refs FIRST
// under syncMu, then advertise the new manifest version. On local apply
// failure: WARN + withdraw (manifest_version = ""), push still answered ok.
// Store work (orphan sweep) happens OUTSIDE syncMu (13 §2.2 rule 4).
func (p *Publisher) commitLocal(ctx context.Context, next *pbManifest, version string, entries []*proto.LogEntry, burned map[uint64]string) {
	p.h.syncMu.Lock()
	applied := true
	var txns []*proto.RefTransaction
	for _, e := range entries {
		if e.Txn != nil && (e.Kind == proto.EntryKindPush || e.Kind == proto.EntryKindRefUpdate) {
			txns = append(txns, e.Txn)
		}
	}
	if len(txns) > 0 {
		if err := p.h.applyTxnsOffline(txns); err != nil {
			publishLocalApplyFailed.Add(1) // walgit_publish_local_apply_failed_total
			logWarnf("publish %s: local ref apply failed: %v (withdrawing version)", p.h.ID, err)
			version = "" // withdraw: next sync replays; the bucket CAS is the truth
			applied = false
		}
	}
	if applied {
		for _, e := range entries {
			if e.CreatedAt != nil {
				t := e.CreatedAt.Go()
				if p.h.firstEntryTime.IsZero() || t.Before(p.h.firstEntryTime) {
					p.h.firstEntryTime = t
				}
				if t.After(p.h.lastEntryTime) {
					p.h.lastEntryTime = t
				}
			}
		}
		// Advertise: the new version is only published AFTER local refs are
		// written (refs-first rule, §3.2).
		p.h.manifest = next
		p.h.version = version
		p.h.heldRev = next.Revision
		p.h.freshAt = time.Now()
	}
	oldRev := p.h.state.Revision
	wasReady := p.h.state.PacksReady()
	set := func(st *RepoState) {
		st.ManifestVersion = version
		if applied {
			st.AppliedSeq = next.HeadSeq
			st.Revision = next.Revision
			if wasReady && st.PacksRevision == oldRev {
				st.PacksRevision = next.Revision // keep packs_ready
			}
		}
	}
	if err := p.h.updateState(set); err != nil {
		logWarnf("publish %s: state persist failed: %v", p.h.ID, err)
	}
	p.h.syncMu.Unlock()

	// Off the critical path, outside every lock: sweep burned orphans (§5.4
	// step 4) and the opportunistic checkpoint trigger (§5.5).
	p.sweepBurned(burned)
	for _, e := range entries {
		if e.Kind == proto.EntryKindCompact && len(e.Supersedes) > 0 {
			p.h.updateState(func(st *RepoState) {
				st.PendingPackRemovals = append(st.PendingPackRemovals, e.Supersedes...)
			})
		}
		if e.Kind == proto.EntryKindSettings {
			p.h.invalidateSettings()
		}
	}
	p.foldCommitGraphs(entries)
	p.maybeCheckpoint(entries)
}

// sweepBurned CAS-deletes the burned segments we recorded (best effort: a
// crash between burn and sweep leaves an unlisted segment that the next
// burn pass handles).
func (p *Publisher) sweepBurned(burned map[uint64]string) {
	if len(burned) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(p.h.reg.ctx, 30*time.Second)
	defer cancel()
	for _, key := range burned {
		meta, err := p.h.reg.st.Head(ctx, p.h.repoKey(key))
		if err != nil || meta == nil {
			continue
		}
		if err := p.h.reg.st.Delete(ctx, p.h.repoKey(key), meta.Version); err == nil {
			orphansSwept.Add(1)
		}
	}
}

// foldCommitGraphs folds commit-graphs for pushed packs off the critical path
// (13 §1: a short-lived background goroutine bounded by the registry ctx).
func (p *Publisher) foldCommitGraphs(entries []*proto.LogEntry) {
	var idxNames []string
	for _, e := range entries {
		if e.Kind == proto.EntryKindPush && e.Pack != nil && !e.Pack.HasCommitGraph {
			idxNames = append(idxNames, e.Pack.Checksum+".idx")
		}
	}
	if len(idxNames) == 0 {
		return
	}
	repo, layer := p.h.repo, p.h.Layer()
	go func() { // 13 §1: short-lived, exits when done or ctx canceled
		cctx, cancel := context.WithTimeout(p.h.reg.ctx, 10*time.Minute)
		defer cancel()
		if err := layer.FoldCommitGraphs(cctx, repo, idxNames); err != nil {
			logWarnf("%s: commit-graph fold: %v", p.h.ID, err)
		}
	}()
}

// maybeCheckpoint evaluates the §5.5 triggers and runs an opportunistic
// background checkpoint off the reply path.
func (p *Publisher) maybeCheckpoint(entries []*proto.LogEntry) {
	p.h.syncMu.Lock()
	m := p.h.manifest
	p.h.syncMu.Unlock()
	if m == nil {
		return
	}
	trig, ok := checkpointTrigger(p.h.reg.vals, m, p.h.firstEntryTime, p.h.lastEntryTime)
	if !ok {
		return
	}
	h := p.h
	_, err := h.reg.tasks.Run(h.reg.ctx, h.ID, "checkpoint", map[string]string{"trigger": string(trig)},
		func(ctx context.Context, t *Task) error {
			// 13 §1 row: background task goroutine; registry ctx, not the reply.
			return h.writeCheckpoint(ctx, t, trig)
		})
	if err != nil && !errors.Is(err, context.Canceled) {
		logWarnf("%s: opportunistic checkpoint: %v", h.ID, err)
	}
}

// applyTxnsOffline folds txns into packed-refs in one atomic rewrite.
// Caller holds syncMu.
func (h *RepoHandle) applyTxnsOffline(txns []*proto.RefTransaction) error {
	snap, err := h.Layer().Snapshot(h.repo)
	if err != nil {
		return &WalError{Kind: WalErrGit, Detail: "snapshot", Wrapped: err}
	}
	refs := make([]git.RefEntry, len(snap.Refs))
	copy(refs, snap.Refs)
	headT := snap.HeadTarget
	for _, txn := range txns {
		refs = git.ApplyRefTxnsOffline(refs, gitUpdates(txn.Updates))
		for _, u := range txn.Updates {
			if u.Name == "HEAD" && u.NewSymbolicTarget != "" {
				headT = u.NewSymbolicTarget
			}
		}
	}
	sortRefs(refs)
	if err := h.Layer().LoadSnapshot(h.repo, refs, headT, ""); err != nil {
		return &WalError{Kind: WalErrGit, Detail: "packed-refs rewrite", Wrapped: err}
	}
	return nil
}

// ---- §5.3.3: publishCompact / addPack / annotatePack / publishSettings ------

// PublishCompact publishes a repack/base/add-pack result as a COMPACT entry.
func (h *RepoHandle) PublishCompact(ctx context.Context, pack *PreparedPack, supersedes []string, meta map[string]string) (PublishResult, error) {
	return h.Publish(ctx, PublishRequest{Pack: pack, Supersedes: supersedes, Compact: true, Meta: meta})
}

// AddPack installs a pack file into the local copy (filename must be
// pack-<checksum>.pack) and publishes it as a COMPACT entry superseding
// nothing, with the caller's tier.
func (h *RepoHandle) AddPack(ctx context.Context, path, checksum string, tier uint32, meta map[string]string) (PublishResult, error) {
	if err := installPackFile(h.repo, path, checksum); err != nil {
		return PublishResult{}, err
	}
	pack := &PreparedPack{Checksum: checksum, PackPath: path, Tier: tier}
	if st, err := os.Stat(path); err == nil {
		pack.PackSize = uint64(st.Size())
	}
	return h.Publish(ctx, PublishRequest{Pack: pack, Compact: true, Tier: tier, Meta: meta})
}

// AnnotatePack retrofits .rev/.bitmap/.commit-graph flags onto a live PackRef —
// manifest-only CAS, no log entry, head_seq unchanged.
func (h *RepoHandle) AnnotatePack(ctx context.Context, checksum string, hasRev, hasBitmap, hasCommitGraph bool) error {
	h.syncMu.Lock()
	base, version := h.manifest, h.version
	h.syncMu.Unlock()
	for attempt := 0; attempt < 16; attempt++ {
		if attempt > 0 {
			publishCASRetries.Add(1)
		}
		next := *base
		next.Packs = make([]*proto.PackRef, len(base.Packs))
		for i, p := range base.Packs {
			cp := *p
			if p.Checksum == checksum {
				p2 := *p
				p2.HasRev, p2.HasBitmap, p2.HasCommitGraph = hasRev, hasBitmap, hasCommitGraph
				next.Packs[i] = &p2
			} else {
				next.Packs[i] = p
			}
			_ = cp
		}
		next.Revision = base.Revision + 1
		next.UpdatedAt = TsPtr(time.Now().UTC())
		next.Writer = h.reg.instance
		key := manifestKey(h.ID)
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: store.Version(version), ContentType: "application/x-protobuf"}
		if version == "" {
			opts.Mode = store.PutCreate
		}
		meta, err := h.reg.st.Put(ctx, key, store.PutBody{Bytes: next.Marshal()}, opts)
		if err == nil {
			h.syncMu.Lock()
			h.manifest = &next
			h.version = string(meta.Version)
			h.heldRev = next.Revision
			h.freshAt = time.Now()
			h.syncMu.Unlock()
			return nil
		}
		if !store.IsPreconditionFailed(err) {
			return &WalError{Kind: WalErrStore, Detail: key, Wrapped: err}
		}
		// 412: normal contention — refresh and retry.
		if err := h.freshenManifest(ctx); err != nil {
			return err
		}
		h.syncMu.Lock()
		base, version = h.manifest, h.version
		h.syncMu.Unlock()
	}
	return &WalError{Kind: WalErrRetry, Detail: "annotate_pack CAS retries exhausted"}
}

// PublishSettings validates and publishes a new settings.toml (§5.3.3):
// invalid TOML → error, nothing published. Happy path: log slot + manifest
// CAS (2 rounds). Invalidates the effective-config cache on commit.
func (h *RepoHandle) PublishSettings(ctx context.Context, settingsToml, author, message string, meta map[string]string) error {
	var doc map[string]any
	if md, err := toml.Decode(settingsToml, &doc); err != nil {
		return &WalError{Kind: WalErrInvalid, Detail: "settings.toml: " + err.Error()}
	} else if len(md.Keys()) > 0 && doc == nil {
		return &WalError{Kind: WalErrInvalid, Detail: "settings.toml: empty document"}
	}
	h.syncMu.Lock()
	prevRev := uint64(0)
	if h.manifest != nil && h.manifest.Settings != nil {
		prevRev = h.manifest.Settings.Revision
	}
	h.syncMu.Unlock()
	rs := &proto.RepoSettings{
		Toml:     settingsToml,
		Revision: prevRev + 1,
		Author:   author,
		Message:  message,
	}
	meta2 := map[string]string{"author": author, "message": message}
	for k, v := range meta {
		meta2[k] = v
	}
	_, err := h.Publish(ctx, PublishRequest{Settings: rs, Meta: meta2})
	if err == nil {
		h.invalidateSettings()
	}
	return err
}

// installPackFile copies an add-pack upload into objects/pack with the
// canonical pack-<checksum>.pack name (§5.3.3 add_pack).
func installPackFile(repo *git.LocalRepo, path, checksum string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return &WalError{Kind: WalErrIo, Detail: path, Wrapped: err}
	}
	dst := filepath.Join(repo.PackDir(), "pack-"+checksum+".pack")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return &WalError{Kind: WalErrIo, Detail: dst, Wrapped: err}
	}
	return nil
}

// mustRead reads a whole file, wrapping errors as engine IO errors.
func mustRead(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return data
}

// txnUpdates returns the ref updates carried by a transaction (nil-safe).
func txnUpdates(e *proto.RefTransaction) []*proto.RefUpdate {
	if e == nil {
		return nil
	}
	return e.Updates
}
