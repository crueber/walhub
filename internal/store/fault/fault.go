// Package fault provides the fault-injecting store wrapper the simulation tier
// runs on (15_testing.md §3, from MASTER_RUST_SPEC §17): TigerBeetle-style
// "safety mode" + "liveness mode" around one store.ObjectStore.
//
// One FaultStore is one *instance's link* to the bucket: every simulated
// instance wraps the same inner store (normally the memory backend — the
// "truth store") in its own FaultStore, so faults are per instance — an
// asymmetric partition is "this instance's link black-holes GETs", a stale
// replica is "this link answers every conditional GET with 304", and so on.
//
// Two modes, switched at run time with Set / Heal:
//
//   - safety: a Plan with non-zero probabilities; every op rolls the dice
//     (seeded, deterministic per link given the same op sequence),
//   - liveness: the harness heals a *core* of links (Heal) and freezes the
//     rest (Set a permanent plan: BlackHole, StaleForever, …). The core must
//     then converge; the frozen links may never interfere with it.
//
// Every fault taken is counted in Stats and, when tracing is on, logged with
// the key so a failing seed can be read back.
package fault

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// Plan holds the probabilities (0..=1 floats) and switches. Zero value = no
// faults at all.
type Plan struct {
	// Uniform latency added to every op, before it is applied.
	// Zero value (both 0) = none.
	Delay [2]time.Duration // low, high
	// Latency added to reads (get) AFTER the inner op: the answer was taken
	// at an earlier instant and arrives late — a conditional GET racing a
	// local publish. Honors OnlyKeys. nil = none.
	DelayAfter *time.Duration
	// Retryable before the op is applied (get/head/put/delete/list/compose).
	PErrBefore float64
	// Mutation applied, then Retryable returned (put/delete/compose only) —
	// the class that breaks PUT-then-CAS protocols.
	PErrAfter float64
	// Conditional PUT/DELETE answers PreconditionFailed without applying.
	PCASFail float64
	// get with if-none-match answers NotModified regardless of the real
	// version (a replica that never sees anyone else's writes).
	PStale304 float64
	// get body streams end early with Retryable after some bytes.
	PTruncate float64
	// The op's context never completes (the wrapper waits on ctx.Done).
	PHang float64
	// Every op hangs forever (hard partition). Pending ops keep hanging.
	BlackHole bool
	// Keys containing any of these substrings answer NotFound on get/head
	// (object lost / not yet visible). Mutations still go through.
	DenyKeys []string
	// Keys containing any of these substrings panic on first touch, once per
	// pattern (a crash mid-protocol). A pattern may be scoped to one op as
	// "put:manifest.pb" (ops: get/head/put/delete/compose).
	PanicOnceKeys []string
	// Restrict every probabilistic fault to keys containing one of these
	// substrings (nil = all keys). BlackHole/Deny/Panic are unaffected.
	OnlyKeys []string
}

// Chaos is moderate, uniform chaos: the "safety mode" dice.
func Chaos(rate float64) Plan {
	return Plan{
		Delay:      [2]time.Duration{0, 5 * time.Millisecond},
		PErrBefore: rate,
		PErrAfter:  rate / 2,
		PCASFail:   rate / 2,
		PStale304:  rate / 2,
		PTruncate:  rate / 2,
		PHang:      0,
	}
}

// BlackHole is a hard partition: nothing ever returns.
func BlackHole() Plan { return Plan{BlackHole: true} }

// StaleForever is an asymmetric partition of the replica kind: writes go
// through, but the instance never learns anything new (every conditional GET
// is a 304).
func StaleForever() Plan { return Plan{PStale304: 1.0} }

// WithOnly restricts every probabilistic fault to keys containing one of the
// given substrings. BlackHole/Deny/Panic are unaffected.
func (p Plan) WithOnly(keys ...string) Plan {
	p.OnlyKeys = append([]string{}, keys...)
	return p
}

// Stats are the exact per-op counters, one per fault class plus total ops.
type Stats struct {
	Ops       atomic.Uint64
	ErrBefore atomic.Uint64
	ErrAfter  atomic.Uint64
	CASFail   atomic.Uint64
	Stale304  atomic.Uint64
	Truncate  atomic.Uint64
	Hang      atomic.Uint64
	Denied    atomic.Uint64
	Panics    atomic.Uint64
}

// Faults sums every fault-class counter (ops excluded).
func (s *Stats) Faults() uint64 {
	return s.ErrBefore.Load() + s.ErrAfter.Load() + s.CASFail.Load() +
		s.Stale304.Load() + s.Truncate.Load() + s.Hang.Load() +
		s.Denied.Load() + s.Panics.Load()
}

// Summary renders the k=v line the sim dumps on failure.
func (s *Stats) Summary() string {
	return fmt.Sprintf(
		"ops=%d err_before=%d err_after=%d cas_fail=%d stale_304=%d truncate=%d hang=%d denied=%d panics=%d",
		s.Ops.Load(), s.ErrBefore.Load(), s.ErrAfter.Load(), s.CASFail.Load(),
		s.Stale304.Load(), s.Truncate.Load(), s.Hang.Load(), s.Denied.Load(),
		s.Panics.Load())
}

// rng is xorshift64*: tiny, seedable, good enough for dice. Ported
// byte-for-byte from the Rust wrapper so seeds are reproducible across ports.
type rng struct{ s uint64 }

func newRng(seed uint64) rng {
	if seed < 1 {
		seed = 1
	}
	return rng{seed ^ 0x9E3779B97F4A7C15}
}

func (r *rng) nextU64() uint64 {
	x := r.s
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.s = x
	return x * 0x2545F4914F6CDD1D
}

func (r *rng) f64() float64 {
	return float64(r.nextU64()>>11) / float64(uint64(1)<<53)
}

func (r *rng) below(n uint64) uint64 {
	if n == 0 {
		return 0
	}
	return r.nextU64() % n
}

// FaultStore wraps one inner store per instance link and injects faults
// according to its current Plan. It implements store.ObjectStore fully,
// delegating to inner after decide says proceed.
type FaultStore struct {
	inner store.ObjectStore
	name  string

	// Lock order (13_concurrency.md): mu → firedMu → rngMu. No op ever holds
	// two of these while calling into inner.
	mu    sync.Mutex // guards plan + trace
	plan  Plan
	trace []string

	firedMu sync.Mutex // guards the panic-once fired set
	fired   []string

	rngMu sync.Mutex // guards rng only; never shares the plan mutex
	rng   rng

	stats Stats
}

// New wraps inner under the given link name, with dice seeded from seed
// (WALHUB_SIM_SEED; deterministic per link given the same op sequence).
func New(inner store.ObjectStore, name string, seed uint64) *FaultStore {
	return &FaultStore{
		inner: inner,
		name:  name,
		rng:   newRng(seed),
	}
}

// Name is the link name (used by the sim's dumpTraces artifact).
func (f *FaultStore) Name() string { return f.name }

// Set replaces the plan; it takes effect for every op issued from now on.
func (f *FaultStore) Set(plan Plan) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plan = plan
}

// Plan returns a copy of the current plan (string slices deep-copied, so the
// caller can never mutate the live plan through it).
func (f *FaultStore) Plan() Plan {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.plan
	p.DenyKeys = append([]string{}, f.plan.DenyKeys...)
	p.PanicOnceKeys = append([]string{}, f.plan.PanicOnceKeys...)
	if f.plan.OnlyKeys == nil {
		p.OnlyKeys = nil
	} else {
		p.OnlyKeys = append([]string{}, f.plan.OnlyKeys...)
	}
	if f.plan.DelayAfter != nil {
		d := *f.plan.DelayAfter
		p.DelayAfter = &d
	}
	return p
}

// Heal switches to liveness mode for a core link: no faults from now on. Ops
// that are already hanging stay hung (that is the point of a crash).
func (f *FaultStore) Heal() { f.Set(Plan{}) }

// Stats returns the exact per-op counters.
func (f *FaultStore) Stats() *Stats { return &f.stats }

// SetTrace turns the decision trace ring on or off.
func (f *FaultStore) SetTrace(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if on {
		f.trace = []string{}
	} else {
		f.trace = nil
	}
}

// TakeTrace swaps out and returns the accumulated trace lines (empty when
// tracing is off); tracing stays on.
func (f *FaultStore) TakeTrace() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.trace == nil {
		return nil
	}
	t := f.trace
	f.trace = []string{}
	return t
}

func (f *FaultStore) logf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.trace != nil {
		f.trace = append(f.trace, fmt.Sprintf(format, args...))
	}
}

func inScope(plan Plan, key string) bool {
	if plan.OnlyKeys == nil {
		return true
	}
	return slices.ContainsFunc(plan.OnlyKeys, func(k string) bool { return strings.Contains(key, k) })
}

var faultOps = []string{"get", "head", "put", "delete", "compose"}

// panicMatch reports whether a PanicOnceKeys pattern applies to this op, and
// the effective key substring ("put:manifest.pb" is op-scoped, plain strings
// are not).
func panicMatch(pattern, op string) (string, bool) {
	if o, k, ok := strings.Cut(pattern, ":"); ok && slices.Contains(faultOps, o) {
		if o == op {
			return k, true
		}
		return "", false
	}
	return pattern, true
}

type dkind int

const (
	dProceed dkind = iota
	dErrBefore
	dErrAfter
	dCasFail
	dStale
	dTruncate
	dHang
	dDenied
)

type decision struct {
	kind dkind
	cut  int // truncate: cut point drawn from the rng
}

func (f *FaultStore) rngF64() float64 {
	f.rngMu.Lock()
	defer f.rngMu.Unlock()
	return f.rng.f64()
}

func (f *FaultStore) rngBelow(n uint64) uint64 {
	f.rngMu.Lock()
	defer f.rngMu.Unlock()
	return f.rng.below(n)
}

// dice draws the six per-op rolls and the truncation cut point under one rng
// lock (same consumption order as the Rust wrapper, so seeds reproduce).
func (f *FaultStore) dice() (roll [6]float64, cut int) {
	f.rngMu.Lock()
	defer f.rngMu.Unlock()
	for i := range roll {
		roll[i] = f.rng.f64()
	}
	return roll, int(f.rng.below(1 << 20))
}

// decide rolls the dice for one op. mutation: put/delete/compose;
// conditional: CAS put/delete or if-none-match get; readBody: get (for
// truncation). First match wins, in this normative order:
//
//  1. ops counter (done by every caller path before/inside decide)
//  2. panic-once            3. black_hole        4. deny (non-mutations)
//  5. delay                 6. scope check       7. dice
func (f *FaultStore) decide(op, key string, mutation, conditional, readBody bool) decision {
	f.stats.Ops.Add(1)
	plan := f.Plan()

	// 2. panic-once: a crash in the middle of a protocol step. Recovered at
	// the instance boundary, never here.
	var hit string
	found := false
	for _, pattern := range plan.PanicOnceKeys {
		if m, ok := panicMatch(pattern, op); ok && strings.Contains(key, m) {
			hit, found = pattern, true
			break
		}
	}
	if found {
		f.firedMu.Lock()
		already := slices.Contains(f.fired, hit)
		if !already {
			f.fired = append(f.fired, hit)
		}
		f.firedMu.Unlock()
		if !already {
			f.stats.Panics.Add(1)
			f.logf("%s %s %s: PANIC", f.name, op, key)
			panic(fmt.Sprintf("fault-store[%s]: injected crash during %s %s", f.name, op, key))
		}
	}

	// 3. black hole: pending ops keep hanging even after heal.
	if plan.BlackHole {
		f.stats.Hang.Add(1)
		f.logf("%s %s %s: black-hole", f.name, op, key)
		return decision{kind: dHang}
	}

	// 4. deny keys on non-mutations: object lost / not yet visible.
	if !mutation {
		for _, p := range plan.DenyKeys {
			if strings.Contains(key, p) {
				f.stats.Denied.Add(1)
				f.logf("%s %s %s: denied", f.name, op, key)
				return decision{kind: dDenied}
			}
		}
	}

	// 5. uniform delay in [Delay[0], Delay[1]].
	if lo, hi := plan.Delay[0], plan.Delay[1]; lo > 0 || hi > 0 {
		var span uint64
		if hi > lo {
			span = uint64((hi - lo) / time.Microsecond)
		}
		extra := f.rngBelow(span + 1)
		time.Sleep(lo + time.Duration(extra)*time.Microsecond)
	}

	// 6. scope check: probabilistic faults only apply to in-scope keys.
	if !inScope(plan, key) {
		return decision{kind: dProceed}
	}

	// 7. dice, in order; first match wins.
	roll, cut := f.dice()
	var d decision
	switch {
	case roll[0] < plan.PHang:
		f.stats.Hang.Add(1)
		d = decision{kind: dHang}
	case roll[1] < plan.PErrBefore:
		f.stats.ErrBefore.Add(1)
		d = decision{kind: dErrBefore}
	case mutation && conditional && roll[2] < plan.PCASFail:
		f.stats.CASFail.Add(1)
		d = decision{kind: dCasFail}
	case mutation && roll[3] < plan.PErrAfter:
		f.stats.ErrAfter.Add(1)
		d = decision{kind: dErrAfter}
	case !mutation && conditional && roll[4] < plan.PStale304:
		f.stats.Stale304.Add(1)
		d = decision{kind: dStale}
	case readBody && roll[5] < plan.PTruncate:
		f.stats.Truncate.Add(1)
		d = decision{kind: dTruncate, cut: cut}
	default:
		d = decision{kind: dProceed}
	}
	if d.kind != dProceed {
		what := "?"
		switch d.kind {
		case dHang:
			what = "hang"
		case dErrBefore:
			what = "err-before"
		case dCasFail:
			what = "cas-fail"
		case dErrAfter:
			what = "err-after"
		case dStale:
			what = "stale-304"
		case dTruncate:
			what = "truncate"
		}
		f.logf("%s %s %s: %s", f.name, op, key, what)
	}
	return d
}

func (f *FaultStore) retryable(op, key, when string) *store.StoreError {
	return store.NewRetryable(key, fmt.Errorf(
		"fault-store[%s]: injected transient error %s %s %s", f.name, when, op, key))
}

// hangErr is the ctx-cancellable hang: it never completes on its own — the
// caller's timeout/cancel is the only way out (no timeout of the wrapper's
// own). A hung op surfaces as the context error, which the sim treats as a
// crash.
func hangErr(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// ---- store.ObjectStore ----

func (f *FaultStore) Backend() string { return f.inner.Backend() }

func (f *FaultStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	conditional := opts.IfNoneMatch != ""
	var late *time.Duration
	if plan := f.Plan(); plan.DelayAfter != nil && inScope(plan, key) {
		d := *plan.DelayAfter
		late = &d
	}
	res, err := f.getInner(ctx, key, opts, conditional)
	if late != nil {
		f.logf("%s get %s: answer delivered %v late (conditional=%v)", f.name, key, *late, conditional)
		time.Sleep(*late)
	}
	return res, err
}

func (f *FaultStore) getInner(ctx context.Context, key string, opts store.GetOptions, conditional bool) (store.GetResult, error) {
	switch d := f.decide("get", key, false, conditional, true); d.kind {
	case dHang:
		return nil, hangErr(ctx)
	case dErrBefore:
		return nil, f.retryable("get", key, "before")
	case dDenied:
		return nil, store.NewNotFound(key)
	case dStale:
		// A replica that never sees anyone else's writes: 304 regardless of
		// the true version.
		return store.NotModified{Version: opts.IfNoneMatch}, nil
	case dTruncate:
		res, err := f.inner.Get(ctx, key, opts)
		if err != nil {
			return nil, err
		}
		obj, ok := res.(store.Object)
		if !ok {
			return res, nil
		}
		size := int(obj.Meta.Size)
		at := d.cut
		if size == 0 {
			at = 0
		} else {
			at %= size
		}
		msg := fmt.Sprintf("fault-store[%s]: injected truncation of %s at %d/%d", f.name, key, at, size)
		return store.Object{Meta: obj.Meta, Body: newTruncateReader(key, obj.Body, at, msg)}, nil
	default: // proceed, errAfter, casFail — no get-side behavior
		return f.inner.Get(ctx, key, opts)
	}
}

func (f *FaultStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	switch f.decide("head", key, false, false, false).kind {
	case dHang:
		return nil, hangErr(ctx)
	case dErrBefore:
		return nil, f.retryable("head", key, "before")
	case dDenied:
		return nil, nil
	default:
		return f.inner.Head(ctx, key)
	}
}

func (f *FaultStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	conditional := opts.Mode != store.PutOverwrite
	switch f.decide("put", key, true, conditional, false).kind {
	case dHang:
		return store.ObjectMeta{}, hangErr(ctx)
	case dErrBefore:
		return store.ObjectMeta{}, f.retryable("put", key, "before")
	case dCasFail:
		return store.ObjectMeta{}, store.NewPrecondition(key, "")
	case dErrAfter:
		// The ambiguous-write class: applied, then the response is lost.
		if _, err := f.inner.Put(ctx, key, body, opts); err != nil {
			return store.ObjectMeta{}, err
		}
		return store.ObjectMeta{}, f.retryable("put", key, "after (applied)")
	default:
		return f.inner.Put(ctx, key, body, opts)
	}
}

func (f *FaultStore) Delete(ctx context.Context, key string, ifVersion store.Version) error {
	conditional := ifVersion != ""
	switch f.decide("delete", key, true, conditional, false).kind {
	case dHang:
		return hangErr(ctx)
	case dErrBefore:
		return f.retryable("delete", key, "before")
	case dCasFail:
		return store.NewPrecondition(key, "")
	case dErrAfter:
		if err := f.inner.Delete(ctx, key, ifVersion); err != nil {
			return err
		}
		return f.retryable("delete", key, "after (applied)")
	default:
		return f.inner.Delete(ctx, key, ifVersion)
	}
}

func (f *FaultStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	// Listing is never on a hot path; only the black hole and err_before apply.
	plan := f.Plan()
	f.stats.Ops.Add(1)
	if plan.BlackHole {
		f.stats.Hang.Add(1)
		return hangErr(ctx)
	}
	if inScope(plan, prefix) && f.rngF64() < plan.PErrBefore {
		f.stats.ErrBefore.Add(1)
		return f.retryable("list", prefix, "before")
	}
	return f.inner.List(ctx, prefix, startAfter, fn)
}

func (f *FaultStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	plan := f.Plan()
	f.stats.Ops.Add(1)
	if plan.BlackHole {
		f.stats.Hang.Add(1)
		return hangErr(ctx)
	}
	if inScope(plan, prefix) && f.rngF64() < plan.PErrBefore {
		f.stats.ErrBefore.Add(1)
		return f.retryable("list_prefixes", prefix, "before")
	}
	return f.inner.ListPrefixes(ctx, prefix, fn)
}

func (f *FaultStore) SignedGetURL(ctx context.Context, key string, ttl time.Duration) (*string, error) {
	return f.inner.SignedGetURL(ctx, key, ttl)
}

func (f *FaultStore) AccelTarget(ctx context.Context, key string) (*store.AccelTarget, error) {
	return f.inner.AccelTarget(ctx, key)
}

func (f *FaultStore) SupportsCompose() bool { return f.inner.SupportsCompose() }
func (f *FaultStore) ComposeIsNative() bool { return f.inner.ComposeIsNative() }

func (f *FaultStore) Compose(ctx context.Context, dst string, sources []string, opts store.PutOptions) (store.ObjectMeta, error) {
	conditional := opts.Mode != store.PutOverwrite
	switch f.decide("compose", dst, true, conditional, false).kind {
	case dHang:
		return store.ObjectMeta{}, hangErr(ctx)
	case dErrBefore:
		return store.ObjectMeta{}, f.retryable("compose", dst, "before")
	case dCasFail:
		return store.ObjectMeta{}, store.NewPrecondition(dst, "")
	case dErrAfter:
		if _, err := f.inner.Compose(ctx, dst, sources, opts); err != nil {
			return store.ObjectMeta{}, err
		}
		return store.ObjectMeta{}, f.retryable("compose", dst, "after (applied)")
	default:
		return f.inner.Compose(ctx, dst, sources, opts)
	}
}

// ---- truth oracle helpers (15_testing.md §3.4) ----

// SnapshotKeys snapshots an inner store's keys → sizes (for oracles that read
// the truth behind every link). Works for any store; intended for the memory
// truth store.
func SnapshotKeys(ctx context.Context, s store.ObjectStore, prefix string) (map[string]int64, error) {
	out := map[string]int64{}
	err := s.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		out[m.Key] = m.Size
		return nil
	})
	return out, err
}

// TruthBytes reads a whole object from the truth store, bypassing every link
// (no instance's stale view can contaminate the oracle). Returns nil bytes
// when the key is absent.
func TruthBytes(ctx context.Context, s store.ObjectStore, key string) ([]byte, error) {
	b, _, err := store.GetBytes(ctx, s, key, store.GetOptions{})
	if err != nil && store.IsNotFound(err) {
		return nil, nil // Rust oracle: Ok(None) for a missing object
	}
	return b, err
}

// truncateReader wraps a get body so it ends early with Retryable after at
// bytes (the truncated prefix is delivered first).
type truncateReader struct {
	rc   io.ReadCloser
	key  string
	at   int
	sent int
	msg  string
	done bool
}

func newTruncateReader(key string, rc io.ReadCloser, at int, msg string) *truncateReader {
	return &truncateReader{rc: rc, key: key, at: at, msg: msg}
}

func (t *truncateReader) faultErr() error {
	return store.NewRetryable(t.key, errors.New(t.msg))
}

func (t *truncateReader) Read(p []byte) (int, error) {
	if t.done {
		return 0, t.faultErr()
	}
	n, err := t.rc.Read(p)
	if n > 0 {
		if room := t.at - t.sent; n > room {
			t.done = true
			n = room
		}
		t.sent += n
		if t.done && err == nil {
			err = t.faultErr()
		}
	}
	return n, err
}

func (t *truncateReader) Close() error { return t.rc.Close() }
