// bridge.go — the catch-up loop (09 §5): exactly one goroutine owns catchUp;
// wake-ups funnel through a bounded (64) coalescing work channel; the notify
// handler never blocks (non-blocking send + dropped report + sweep backstop);
// the cursor CAS is the cross-instance lock (lost CAS = success).
package events

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// workCap bounds the coalescing work channel (09 §5.2, normative).
const workCap = 64

// RepoView is the bridge's narrow per-repo engine view (bound to the concrete
// internal/wal surface in bind_wal.go).
type RepoView interface {
	// SyncRefs revalidates the manifest FRESH (wal.freshness_ttl does not apply
	// — the bridge always revalidates) and reports head_seq, min_seq (entries
	// below are folded into the checkpoint), and the object format.
	SyncRefs(ctx context.Context) (RepoState, error)
	// ReadLog reads the framed log entries with seq in [from, to].
	ReadLog(ctx context.Context, from, to uint64) ([]*proto.LogEntry, error)
}

// RepoState is what a fresh refs sync yields.
type RepoState struct {
	HeadSeq uint64
	MinSeq  uint64
	Sha256  bool // object format: false = sha1 (40-hex OIDs), true = sha256 (64-hex)
}

// WalSource is the bridge's narrow engine view (bound in bind_wal.go).
type WalSource interface {
	// Repos lists every repo this instance knows ("owner/name").
	Repos(ctx context.Context) ([]string, error)
	// Handle opens the repo view; unknown repo → error.
	Handle(ctx context.Context, repo string) (RepoView, error)
}

// Deps wires the bridge. Sinks are called sequentially, one POST per sink for
// the whole batch; any sink failure aborts the catch-up with the cursor
// untouched (09 §4.3).
type Deps struct {
	Source        WalSource
	Store         store.ObjectStore
	Sinks         []Sink
	Metrics       Metrics       // nil → NoopMetrics
	Logger        *slog.Logger  // nil → discard
	SweepInterval time.Duration // "0s" disables the sweep (09 §6.2)
}

// Bridge is the events service: one goroutine (Run) plus the notify HTTP
// handler (HandleNotify). Instantiate only when the instance's server.roles
// includes events AND events.webhook_url is set — otherwise there is no bridge
// and POST /_events/notify answers 404 (09 §1).
type Bridge struct {
	src   WalSource
	st    store.ObjectStore
	sinks []Sink
	met   Metrics
	log   *slog.Logger

	sweepEvery time.Duration

	work    chan string
	pending map[string]bool // coalescing set: repos queued in the work channel
	mu      sync.Mutex

	sinkFails map[string]error // last sink error per repo (notify 503 path)
	failMu    sync.Mutex

	runOnce sync.Once
}

// New builds a bridge. Run must be started separately (composition decides
// when, per 09 §5.2 startup).
func New(d Deps) *Bridge {
	met := d.Metrics
	if met == nil {
		met = NoopMetrics{}
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Bridge{
		src:        d.Source,
		st:         d.Store,
		sinks:      d.Sinks,
		met:        met,
		log:        log,
		sweepEvery: d.SweepInterval,

		work:      make(chan string, workCap),
		pending:   make(map[string]bool),
		sinkFails: make(map[string]error),
	}
}

// Wake coalesces a wake-up and performs a NON-BLOCKING send on the work
// channel. It never blocks: when the channel is full or the repo is already
// queued, the wake is a no-op and the sweep is the backstop. Returns
// "queued" or "dropped" (09 §6.3).
func (b *Bridge) Wake(repo string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending[repo] {
		return "dropped" // coalesced
	}
	select {
	case b.work <- repo:
		b.pending[repo] = true
		return "queued"
	default:
		return "dropped" // channel full — sweep is the backstop
	}
}

// Run is the bridge goroutine: drains work, runs the sweep ticker, exits on
// ctx cancel after finishing the in-flight catch-up (the cursor is always
// consistent).
func (b *Bridge) Run(ctx context.Context) {
	b.runOnce.Do(func() {
		var ticker *time.Ticker
		tickC := make(<-chan time.Time)
		if b.sweepEvery > 0 {
			ticker = time.NewTicker(b.sweepEvery)
			defer ticker.Stop()
			tickC = ticker.C
		}
		for {
			select {
			case <-ctx.Done():
				return
			case repo := <-b.work:
				b.take(repo)
				b.catchUp(ctx, repo)
			case <-tickC:
				b.sweep(ctx)
			}
		}
	})
}

// take moves a repo out of the queued set before its catch-up runs: a wake-up
// arriving during the run re-queues it, so a repo woken during its own
// catch-up runs again once (at-least-once, 09 §5.2).
func (b *Bridge) take(repo string) {
	b.mu.Lock()
	delete(b.pending, repo)
	b.mu.Unlock()
}

// sweep runs catchUp for every repo in the registry (09 §6.2): backstop and
// health check — anything a sweep publishes means notifications are not
// flowing.
func (b *Bridge) sweep(ctx context.Context) {
	repos, err := b.src.Repos(ctx)
	if err != nil {
		b.log.WarnContext(ctx, "events: sweep listing failed", "err", err)
		return
	}
	for _, repo := range repos {
		n, err := b.catchUp(ctx, repo)
		if err != nil {
			b.log.WarnContext(ctx, "events: sweep catch-up failed", "repo", repo, "err", err)
			continue
		}
		if n > 0 {
			b.met.Add(MetricSweepFound, int64(n))
			b.log.Warn("events: sweep published events; notifications are not flowing",
				"repo", repo, "published", n)
		}
	}
}

// catchUp implements 09 §5.1 in normative order.
// Returns the number of events published to sinks.
func (b *Bridge) catchUp(ctx context.Context, repo string) (int, error) {
	key, err := cursorKey(repo)
	if err != nil {
		return 0, fmt.Errorf("events: bad repo id %q: %w", repo, err)
	}
	view, err := b.src.Handle(ctx, repo)
	if err != nil {
		return 0, fmt.Errorf("events: open %s: %w", repo, err)
	}
	state, err := view.SyncRefs(ctx)
	if err != nil {
		return 0, fmt.Errorf("events: sync refs %s: %w", repo, err)
	}
	head, minSeq := state.HeadSeq, state.MinSeq

	// 2. readable_from = min_seq − 1 (entries below are folded into the
	//    checkpoint); clamp at 0 for an empty log window.
	readableFrom := uint64(0)
	if minSeq > 0 {
		readableFrom = minSeq - 1
	}

	cursor, ver, found, err := loadCursor(ctx, b.st, key)
	if err != nil {
		return 0, err
	}
	from := cursor.PublishedSeq
	if !found {
		// Cold cursor: everything still in the log window is published once;
		// operators pre-seed the cursor to skip history (09 §5.1 step 2).
		from = readableFrom
	}
	// A cursor below readable_from is a GAP: counted + warned, never silently
	// repaired; folded entries are unreadable so the read starts at min_seq.
	if found && cursor.PublishedSeq < readableFrom {
		b.met.Inc(MetricGap, repo)
		b.log.Warn("events: cursor gap; entries below min_seq are folded and unreadable",
			"repo", repo, "cursor", cursor.PublishedSeq, "readable_from", readableFrom)
	}
	readStart := from + 1
	if readStart < minSeq {
		readStart = minSeq
	}

	// 3. Lag gauge — recorded even when the subsequent publish fails.
	b.met.Set(MetricLag, int64(head-from), repo)

	if head <= from {
		b.clearFail(repo)
		return 0, nil
	}

	// 4. read_log → ref events → publish to every sink; a sink failure aborts
	//    here, cursor untouched.
	entries, err := view.ReadLog(ctx, readStart, head)
	if err != nil {
		return 0, fmt.Errorf("events: read log %s [%d,%d]: %w", repo, readStart, head, err)
	}
	batch := make([]RefEvent, 0)
	for _, e := range entries {
		batch = append(batch, eventsFromEntry(repo, e, formatOf(state))...)
	}
	if err := b.publish(ctx, repo, batch); err != nil {
		b.recordFail(repo, err)
		return 0, err
	}
	b.clearFail(repo)

	// 5. CAS the cursor to head. Lost CAS (another bridge advanced it) is
	//    treated as success: our emission was a duplicate.
	if err := casCursor(ctx, b.st, key, ver, found, head); err != nil {
		return 0, err
	}
	return len(batch), nil
}

// publish sends the whole batch to every sink sequentially, one POST per sink.
func (b *Bridge) publish(ctx context.Context, repo string, batch []RefEvent) error {
	if len(batch) == 0 {
		return nil
	}
	for _, s := range b.sinks {
		if err := s.Deliver(ctx, repo, batch); err != nil {
			b.log.Warn("events: sink delivery failed; cursor untouched",
				"sink", s.Name(), "repo", repo, "events", len(batch), "err", err)
			return fmt.Errorf("events: sink %s: %w", s.Name(), err)
		}
		b.met.Add(MetricPublished, int64(len(batch)), s.Name())
	}
	return nil
}

func (b *Bridge) recordFail(repo string, err error) {
	b.failMu.Lock()
	b.sinkFails[repo] = err
	b.failMu.Unlock()
}

func (b *Bridge) clearFail(repo string) {
	b.failMu.Lock()
	delete(b.sinkFails, repo)
	b.failMu.Unlock()
}

func (b *Bridge) lastFail(repo string) error {
	b.failMu.Lock()
	defer b.failMu.Unlock()
	return b.sinkFails[repo]
}

func formatOf(s RepoState) git.ObjectFormat {
	if s.Sha256 {
		return git.Sha256
	}
	return git.Sha1
}
