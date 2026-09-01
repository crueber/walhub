package fault

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// ---- fake inner store (stands in for the memory truth backend) ----

type fakeStore struct {
	mu   sync.Mutex
	objs map[string][]byte
	vers map[string]store.Version
	seq  uint64

	gets, puts, deletes, composes, lists, listPrefixes int

	failMutation error // when set, Put/Delete/Compose fail with this
	failGet      error // when set, Get fails with this
}

func newFakeStore() *fakeStore {
	return &fakeStore{objs: map[string][]byte{}, vers: map[string]store.Version{}}
}

func (s *fakeStore) Backend() string { return "memory" }

func (s *fakeStore) Get(_ context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.failGet != nil {
		return nil, s.failGet
	}
	v, ok := s.vers[key]
	if opts.IfMatch != "" && (!ok || v != opts.IfMatch) {
		return nil, store.NewPrecondition(key, v)
	}
	if opts.IfNoneMatch != "" && ok && v == opts.IfNoneMatch {
		return store.NotModified{Version: v}, nil
	}
	if !ok {
		return nil, store.NewNotFound(key)
	}
	b := s.objs[key]
	return store.Object{
		Meta: store.ObjectMeta{Key: key, Size: int64(len(b)), Version: v},
		Body: io.NopCloser(bytes.NewReader(b)),
	}, nil
}

func (s *fakeStore) Head(_ context.Context, key string) (*store.ObjectMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vers[key]
	if !ok {
		return nil, nil
	}
	return &store.ObjectMeta{Key: key, Size: int64(len(s.objs[key])), Version: v}, nil
}

func (s *fakeStore) put(key string, b []byte, opts store.PutOptions) (store.ObjectMeta, error) {
	v, ok := s.vers[key]
	switch opts.Mode {
	case store.PutCreate:
		if ok {
			return store.ObjectMeta{}, store.NewPrecondition(key, v)
		}
	case store.PutUpdate:
		if !ok || v != opts.IfVersion {
			return store.ObjectMeta{}, store.NewPrecondition(key, v)
		}
	}
	s.seq++
	s.objs[key] = b
	s.vers[key] = store.Version(strconv.FormatUint(s.seq, 10))
	return store.ObjectMeta{Key: key, Size: int64(len(b)), Version: s.vers[key]}, nil
}

func (s *fakeStore) Put(_ context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	if s.failMutation != nil {
		return store.ObjectMeta{}, s.failMutation
	}
	return s.put(key, body.Bytes, opts)
}
func (s *fakeStore) Delete(_ context.Context, key string, ifVersion store.Version) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	v, ok := s.vers[key]
	if ifVersion != "" && (!ok || v != ifVersion) {
		return store.NewPrecondition(key, v)
	}
	if s.failMutation != nil {
		return s.failMutation
	}
	delete(s.objs, key)
	delete(s.vers, key)
	return nil
}

func (s *fakeStore) List(_ context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	s.mu.Lock()
	s.lists++
	keys := make([]string, 0, len(s.vers))
	for k := range s.vers {
		if strings.HasPrefix(k, prefix) && k > startAfter {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	items := make([]store.ObjectMeta, 0, len(keys))
	for _, k := range keys {
		items = append(items, store.ObjectMeta{Key: k, Size: int64(len(s.objs[k])), Version: s.vers[k]})
	}
	s.mu.Unlock()
	for _, m := range items {
		if err := fn(m); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeStore) ListPrefixes(_ context.Context, prefix string, fn func(string) error) error {
	s.mu.Lock()
	s.listPrefixes++
	seen := map[string]bool{}
	var out []string
	for k := range s.vers {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			p := prefix + rest[:i+1]
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	s.mu.Unlock()
	for _, p := range out {
		if err := fn(p); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeStore) SignedGetURL(_ context.Context, key string, _ time.Duration) (*string, error) {
	u := "https://signed/" + key
	return &u, nil
}

func (s *fakeStore) AccelTarget(_ context.Context, key string) (*store.AccelTarget, error) {
	return &store.AccelTarget{URL: "https://accel/" + key}, nil
}

func (s *fakeStore) SupportsCompose() bool { return true }
func (s *fakeStore) ComposeIsNative() bool { return true }

func (s *fakeStore) Compose(_ context.Context, dst string, sources []string, opts store.PutOptions) (store.ObjectMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.composes++
	if s.failMutation != nil {
		return store.ObjectMeta{}, s.failMutation
	}
	var b []byte
	for _, src := range sources {
		b = append(b, s.objs[src]...)
	}
	return s.put(dst, b, opts)
}

func (s *fakeStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objs[key]
	return ok
}

func (s *fakeStore) bytes(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objs[key]
}

// ---- test helpers ----

func newLink(t *testing.T, seed uint64) (*FaultStore, *fakeStore) {
	t.Helper()
	truth := newFakeStore()
	return New(truth, "inst-a", seed), truth
}

// runWithRecover runs fn and reports the recovered panic value (nil if none).
func runWithRecover(fn func()) (rec any) {
	defer func() { rec = recover() }()
	fn()
	return nil
}

func wantRetryable(t *testing.T, err error, what string) {
	t.Helper()
	if !store.IsRetryable(err) {
		t.Fatalf("%s: want retryable error, got %v", what, err)
	}
	if !strings.Contains(err.Error(), "fault-store[inst-a]") {
		t.Fatalf("%s: retryable error should name the link: %v", what, err)
	}
}

func wantPrecondition(t *testing.T, err error, what string) {
	t.Helper()
	if !store.IsPreconditionFailed(err) {
		t.Fatalf("%s: want precondition failed, got %v", what, err)
	}
}

// ---- presets ----

func TestPlanPresets(t *testing.T) {
	t.Run("default_is_fault_free", func(t *testing.T) {
		var p Plan
		if p.Delay != [2]time.Duration{} || p.DelayAfter != nil ||
			p.PErrBefore != 0 || p.PErrAfter != 0 || p.PCASFail != 0 ||
			p.PStale304 != 0 || p.PTruncate != 0 || p.PHang != 0 ||
			p.BlackHole || len(p.DenyKeys) != 0 || len(p.PanicOnceKeys) != 0 ||
			p.OnlyKeys != nil {
			t.Fatalf("zero Plan must be fault-free: %+v", p)
		}
	})
	t.Run("chaos", func(t *testing.T) {
		p := Chaos(0.6)
		if p.Delay != [2]time.Duration{0, 5 * time.Millisecond} {
			t.Fatalf("chaos delay = %v", p.Delay)
		}
		if p.PErrBefore != 0.6 || p.PErrAfter != 0.3 || p.PCASFail != 0.3 ||
			p.PStale304 != 0.3 || p.PTruncate != 0.3 || p.PHang != 0 {
			t.Fatalf("chaos probabilities wrong: %+v", p)
		}
	})
	t.Run("black_hole", func(t *testing.T) {
		if p := BlackHole(); !p.BlackHole {
			t.Fatal("black hole preset must set BlackHole")
		}
	})
	t.Run("stale_forever", func(t *testing.T) {
		if p := StaleForever(); p.PStale304 != 1.0 {
			t.Fatalf("stale forever PStale304 = %v", p.PStale304)
		}
	})
	t.Run("with_only", func(t *testing.T) {
		p := StaleForever().WithOnly("manifest")
		if len(p.OnlyKeys) != 1 || p.OnlyKeys[0] != "manifest" {
			t.Fatalf("WithOnly = %v", p.OnlyKeys)
		}
		if p.PStale304 != 1.0 {
			t.Fatal("WithOnly must not disturb other fields")
		}
	})
	t.Run("plan_returns_a_copy", func(t *testing.T) {
		link, _ := newLink(t, 1)
		link.Set(StaleForever().WithOnly("manifest"))
		got := link.Plan()
		got.OnlyKeys[0] = "mutated"
		if link.Plan().OnlyKeys[0] == "mutated" {
			t.Fatal("Plan() must return a copy")
		}
	})
}

// ---- err_before: retryable before the op is applied ----

func TestFaultErrBefore(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   func(ctx context.Context, l *FaultStore) error
	}{
		{"get", func(ctx context.Context, l *FaultStore) error {
			_, err := l.Get(ctx, "k", store.GetOptions{})
			return err
		}},
		{"head", func(ctx context.Context, l *FaultStore) error {
			_, err := l.Head(ctx, "k")
			return err
		}},
		{"put", func(ctx context.Context, l *FaultStore) error {
			_, err := l.Put(ctx, "k", store.PutBody{Bytes: []byte("v")}, store.PutOptions{Mode: store.PutOverwrite})
			return err
		}},
		{"delete", func(ctx context.Context, l *FaultStore) error {
			return l.Delete(ctx, "k", "")
		}},
		{"compose", func(ctx context.Context, l *FaultStore) error {
			_, err := l.Compose(ctx, "dst", []string{"src"}, store.PutOptions{Mode: store.PutOverwrite})
			return err
		}},
		{"list", func(ctx context.Context, l *FaultStore) error {
			return l.List(ctx, "p/", "", func(store.ObjectMeta) error { return nil })
		}},
		{"list_prefixes", func(ctx context.Context, l *FaultStore) error {
			return l.ListPrefixes(ctx, "p/", func(string) error { return nil })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link, truth := newLink(t, 1)
			link.Set(Plan{PErrBefore: 1.0})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := tc.op(ctx, link)
			wantRetryable(t, err, tc.name)
			if got := link.Stats().ErrBefore.Load(); got != 1 {
				t.Fatalf("err_before counter = %d, want 1", got)
			}
			if truth.puts != 0 && tc.name != "put" && tc.name != "compose" {
				t.Fatal("err_before must not reach inner on reads")
			}
		})
	}

	t.Run("err_before_applies_nothing", func(t *testing.T) {
		link, truth := newLink(t, 1)
		link.Set(Plan{PErrBefore: 1.0})
		ctx := context.Background()
		if _, err := link.Put(ctx, "k", store.PutBody{Bytes: []byte("v")}, store.PutOptions{Mode: store.PutOverwrite}); err == nil {
			t.Fatal("expected fault")
		}
		if truth.has("k") {
			t.Fatal("err_before must not apply the mutation")
		}
	})
}

// ---- err_after: mutation applied, then the response is lost ----

func TestFaultErrAfter(t *testing.T) {
	for _, tc := range []struct {
		name    string
		op      func(ctx context.Context, l *FaultStore, tr *fakeStore) error
		applied func(tr *fakeStore) bool
	}{
		{"put", func(ctx context.Context, l *FaultStore, tr *fakeStore) error {
			_, err := l.Put(ctx, "k", store.PutBody{Bytes: []byte("v")}, store.PutOptions{Mode: store.PutOverwrite})
			return err
		}, func(tr *fakeStore) bool { return tr.has("k") }},
		{"delete", func(ctx context.Context, l *FaultStore, tr *fakeStore) error {
			tr.Put(context.Background(), "k", store.PutBody{Bytes: []byte("v")}, store.PutOptions{})
			return l.Delete(ctx, "k", "")
		}, func(tr *fakeStore) bool { return !tr.has("k") }},
		{"compose", func(ctx context.Context, l *FaultStore, tr *fakeStore) error {
			tr.Put(context.Background(), "src", store.PutBody{Bytes: []byte("s")}, store.PutOptions{})
			_, err := l.Compose(ctx, "dst", []string{"src"}, store.PutOptions{Mode: store.PutOverwrite})
			return err
		}, func(tr *fakeStore) bool { return tr.has("dst") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link, truth := newLink(t, 1)
			link.Set(Plan{PErrAfter: 1.0})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := tc.op(ctx, link, truth)
			wantRetryable(t, err, tc.name)
			if !tc.applied(truth) {
				t.Fatalf("err_after must apply the %s then fail", tc.name)
			}
			if got := link.Stats().ErrAfter.Load(); got != 1 {
				t.Fatalf("err_after counter = %d, want 1", got)
			}
		})
	}

	t.Run("reads_are_never_err_after", func(t *testing.T) {
		link, truth := newLink(t, 1)
		truth.Put(context.Background(), "k", store.PutBody{Bytes: []byte("v")}, store.PutOptions{})
		link.Set(Plan{PErrAfter: 1.0})
		ctx := context.Background()
		if _, err := link.Get(ctx, "k", store.GetOptions{}); err != nil {
			t.Fatalf("err_after must not fault reads: %v", err)
		}
		if _, err := link.Head(ctx, "k"); err != nil {
			t.Fatalf("err_after must not fault heads: %v", err)
		}
		if got := link.Stats().ErrAfter.Load(); got != 0 {
			t.Fatalf("err_after counter = %d, want 0", got)
		}
	})
}

// ---- cas_fail: conditional mutations answered 412 without applying ----

func TestFaultCASFail(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(tr *fakeStore)
		op      func(ctx context.Context, l *FaultStore, tr *fakeStore) error
		applied func(tr *fakeStore) bool
	}{
		{"put_create", func(tr *fakeStore) {
			tr.Put(context.Background(), "k", store.PutBody{Bytes: []byte("old")}, store.PutOptions{})
		}, func(ctx context.Context, l *FaultStore, tr *fakeStore) error {
			_, err := l.Put(ctx, "k", store.PutBody{Bytes: []byte("new")}, store.PutOptions{Mode: store.PutCreate})
			return err
		}, func(tr *fakeStore) bool { return string(tr.bytes("k")) == "new" }},
		{"put_update", func(tr *fakeStore) {}, func(ctx context.Context, l *FaultStore, tr *fakeStore) error {
			_, err := l.Put(ctx, "k", store.PutBody{Bytes: []byte("new")}, store.PutOptions{Mode: store.PutUpdate, IfVersion: "9"})
			return err
		}, func(tr *fakeStore) bool { return tr.has("k") }},
		{"delete_if_version", func(tr *fakeStore) {
			tr.Put(context.Background(), "k", store.PutBody{Bytes: []byte("v")}, store.PutOptions{})
		}, func(ctx context.Context, l *FaultStore, tr *fakeStore) error {
			return l.Delete(ctx, "k", "9")
		}, func(tr *fakeStore) bool { return !tr.has("k") }},
		{"compose_create", func(tr *fakeStore) {
			tr.Put(context.Background(), "src", store.PutBody{Bytes: []byte("s")}, store.PutOptions{})
			tr.Put(context.Background(), "dst", store.PutBody{Bytes: []byte("d")}, store.PutOptions{})
		}, func(ctx context.Context, l *FaultStore, tr *fakeStore) error {
			_, err := l.Compose(ctx, "dst", []string{"src"}, store.PutOptions{Mode: store.PutCreate})
			return err
		}, func(tr *fakeStore) bool { return string(tr.bytes("dst")) == "s" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link, truth := newLink(t, 1)
			tc.setup(truth)
			link.Set(Plan{PCASFail: 1.0})
			ctx := context.Background()
			err := tc.op(ctx, link, truth)
			wantPrecondition(t, err, tc.name)
			if tc.applied(truth) {
				t.Fatalf("%s: cas_fail must answer 412 without applying", tc.name)
			}
			if got := link.Stats().CASFail.Load(); got != 1 {
				t.Fatalf("cas_fail counter = %d, want 1", got)
			}
		})
	}

	t.Run("unconditional_put_is_not_conditional", func(t *testing.T) {
		link, _ := newLink(t, 1)
		link.Set(Plan{PCASFail: 1.0})
		if _, err := link.Put(context.Background(), "k", store.PutBody{Bytes: []byte("v")}, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
			t.Fatalf("overwrite put must ignore PCASFail: %v", err)
		}
		if got := link.Stats().CASFail.Load(); got != 0 {
			t.Fatalf("cas_fail counter = %d, want 0", got)
		}
	})
}

// ---- stale 304: a replica that never sees anyone else's writes ----

func TestFaultStale304(t *testing.T) {
	link, truth := newLink(t, 1)
	ctx := context.Background()
	m1, _ := truth.Put(ctx, "k", store.PutBody{Bytes: []byte("1")}, store.PutOptions{})
	truth.Put(ctx, "k", store.PutBody{Bytes: []byte("2")}, store.PutOptions{})

	link.Set(StaleForever())
	res, err := link.Get(ctx, "k", store.GetOptions{IfNoneMatch: m1.Version})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	nm, ok := res.(store.NotModified)
	if !ok || nm.Version != m1.Version {
		t.Fatalf("stale link must answer 304 with the known version, got %#v", res)
	}
	if got := link.Stats().Stale304.Load(); got != 1 {
		t.Fatalf("stale_304 counter = %d, want 1", got)
	}

	t.Run("unconditional_get_still_fresh", func(t *testing.T) {
		b, _, err := store.GetBytes(ctx, link, "k", store.GetOptions{})
		if err != nil {
			t.Fatalf("plain get under stale plan: %v", err)
		}
		if string(b) != "2" {
			t.Fatalf("plain get = %q, want fresh %q", b, "2")
		}
		if got := link.Stats().Stale304.Load(); got != 1 {
			t.Fatalf("stale_304 counter = %d, want still 1", got)
		}
	})

	t.Run("head_unaffected", func(t *testing.T) {
		m, err := link.Head(ctx, "k")
		if err != nil || m == nil {
			t.Fatalf("head under stale plan: %v, %v", m, err)
		}
	})

	t.Run("heal_restores_freshness", func(t *testing.T) {
		link.Heal()
		res, err := store.GetIfChanged(ctx, link, "k", m1.Version)
		if err != nil {
			t.Fatalf("healed get_if_changed: %v", err)
		}
		obj, ok := res.(store.Object)
		if !ok {
			t.Fatalf("healed link must return the object, got %#v", res)
		}
		defer obj.Body.Close()
		b, _ := io.ReadAll(obj.Body)
		if string(b) != "2" {
			t.Fatalf("healed body = %q, want %q", b, "2")
		}
	})
}

// ---- truncate: get bodies end early with Retryable ----

func TestFaultTruncate(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
	}{
		{"large_body", 4096},
		{"single_byte", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link, truth := newLink(t, 3)
			truth.Put(context.Background(), "k", store.PutBody{Bytes: bytes.Repeat([]byte{7}, tc.size)}, store.PutOptions{})
			link.Set(Plan{PTruncate: 1.0})
			res, err := link.Get(context.Background(), "k", store.GetOptions{})
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			obj := res.(store.Object)
			b, err := io.ReadAll(obj.Body)
			obj.Body.Close()
			if err == nil {
				t.Fatalf("truncated body must surface as an error, read %d bytes", len(b))
			}
			if !store.IsRetryable(err) {
				t.Fatalf("truncation error must be retryable, got %v", err)
			}
			if len(b) >= tc.size {
				t.Fatalf("truncated prefix %d must be shorter than %d", len(b), tc.size)
			}
			for _, v := range b {
				if v != 7 {
					t.Fatalf("truncated prefix corrupted: %v", b)
				}
			}
			if got := link.Stats().Truncate.Load(); got != 1 {
				t.Fatalf("truncate counter = %d, want 1", got)
			}
		})
	}

	t.Run("non_object_results_pass_through", func(t *testing.T) {
		link, truth := newLink(t, 3)
		m, _ := truth.Put(context.Background(), "k", store.PutBody{Bytes: []byte("v")}, store.PutOptions{})
		link.Set(Plan{PTruncate: 1.0})
		res, err := link.Get(context.Background(), "k", store.GetOptions{IfNoneMatch: m.Version})
		if err != nil {
			t.Fatalf("conditional get: %v", err)
		}
		if _, ok := res.(store.NotModified); !ok {
			t.Fatalf("NotModified must pass through the truncate path, got %#v", res)
		}
	})
}

// ---- hang: the op's context never completes on its own ----

func TestFaultHang(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan Plan
		op   func(ctx context.Context, l *FaultStore) error
	}{
		{"get", Plan{PHang: 1.0}, func(ctx context.Context, l *FaultStore) error {
			_, err := l.Get(ctx, "k", store.GetOptions{})
			return err
		}},
		{"head", Plan{PHang: 1.0}, func(ctx context.Context, l *FaultStore) error {
			_, err := l.Head(ctx, "k")
			return err
		}},
		{"put", Plan{PHang: 1.0}, func(ctx context.Context, l *FaultStore) error {
			_, err := l.Put(ctx, "k", store.PutBody{Bytes: []byte("v")}, store.PutOptions{Mode: store.PutOverwrite})
			return err
		}},
		{"delete", Plan{PHang: 1.0}, func(ctx context.Context, l *FaultStore) error {
			return l.Delete(ctx, "k", "")
		}},
		{"compose", Plan{PHang: 1.0}, func(ctx context.Context, l *FaultStore) error {
			_, err := l.Compose(ctx, "dst", nil, store.PutOptions{Mode: store.PutOverwrite})
			return err
		}},
		{"list_black_hole", Plan{BlackHole: true}, func(ctx context.Context, l *FaultStore) error {
			return l.List(ctx, "p/", "", func(store.ObjectMeta) error { return nil })
		}},
		{"list_prefixes_black_hole", Plan{BlackHole: true}, func(ctx context.Context, l *FaultStore) error {
			return l.ListPrefixes(ctx, "p/", func(string) error { return nil })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link, _ := newLink(t, 1)
			link.Set(tc.plan)
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := tc.op(ctx, link)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("hung op must surface as the ctx error, got %v", err)
			}
			if time.Since(start) < 40*time.Millisecond {
				t.Fatal("hung op returned before the caller's deadline")
			}
			if got := link.Stats().Hang.Load(); got != 1 {
				t.Fatalf("hang counter = %d, want 1", got)
			}
		})
	}

	t.Run("heal_does_not_unhang_pending_ops", func(t *testing.T) {
		link, _ := newLink(t, 1)
		link.Set(BlackHole())
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		go func() {
			time.Sleep(20 * time.Millisecond)
			link.Heal()
		}()
		start := time.Now()
		if _, err := link.Head(ctx, "k"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("healed plan must not unhang pending ops, got %v", err)
		}
		if time.Since(start) < 100*time.Millisecond {
			t.Fatal("pending op returned early after heal")
		}
	})
}

// ---- deny keys: object lost / not yet visible on non-mutations ----

func TestFaultDenyKeys(t *testing.T) {
	link, truth := newLink(t, 1)
	truth.Put(context.Background(), "lost/k", store.PutBody{Bytes: []byte("v")}, store.PutOptions{})
	link.Set(Plan{DenyKeys: []string{"lost/"}})

	t.Run("get_denied", func(t *testing.T) {
		if _, err := link.Get(context.Background(), "lost/k", store.GetOptions{}); !store.IsNotFound(err) {
			t.Fatalf("denied get must be NotFound, got %v", err)
		}
	})
	t.Run("head_denied", func(t *testing.T) {
		m, err := link.Head(context.Background(), "lost/k")
		if err != nil || m != nil {
			t.Fatalf("denied head must be (nil, nil), got %v, %v", m, err)
		}
	})
	t.Run("mutations_still_go_through", func(t *testing.T) {
		if _, err := link.Put(context.Background(), "lost/k", store.PutBody{Bytes: []byte("v2")}, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
			t.Fatalf("deny must not affect mutations: %v", err)
		}
		if string(truth.bytes("lost/k")) != "v2" {
			t.Fatal("mutation under deny plan did not land")
		}
	})
	t.Run("counter", func(t *testing.T) {
		if got := link.Stats().Denied.Load(); got != 2 {
			t.Fatalf("denied counter = %d, want 2", got)
		}
	})
}

// ---- panic-once: a crash in the middle of a protocol step ----

func TestFaultPanicOnce(t *testing.T) {
	t.Run("plain_pattern_fires_once", func(t *testing.T) {
		link, _ := newLink(t, 1)
		link.Set(Plan{PanicOnceKeys: []string{"boom"}})
		ctx := context.Background()

		rec := runWithRecover(func() { link.Get(ctx, "a-boom-b", store.GetOptions{}) })
		if rec == nil || !strings.Contains(fmt.Sprint(rec), "injected crash during get") {
			t.Fatalf("first touch must panic, got %v", rec)
		}
		if got := link.Stats().Panics.Load(); got != 1 {
			t.Fatalf("panics counter = %d, want 1", got)
		}
		if rec := runWithRecover(func() { link.Get(ctx, "a-boom-b", store.GetOptions{}) }); rec != nil {
			t.Fatalf("panic-once must fire only once, got %v", rec)
		}
		if rec := runWithRecover(func() { link.Get(ctx, "other-boom", store.GetOptions{}) }); rec != nil {
			t.Fatalf("fired set is per pattern, got %v", rec)
		}
	})

	t.Run("op_scoped_pattern", func(t *testing.T) {
		link, _ := newLink(t, 1)
		link.Set(Plan{PanicOnceKeys: []string{"put:manifest.pb"}})
		ctx := context.Background()
		if rec := runWithRecover(func() { link.Get(ctx, "manifest.pb", store.GetOptions{}) }); rec != nil {
			t.Fatalf("op-scoped pattern must not fire on get: %v", rec)
		}
		rec := runWithRecover(func() {
			link.Put(ctx, "manifest.pb", store.PutBody{Bytes: []byte("m")}, store.PutOptions{Mode: store.PutOverwrite})
		})
		if rec == nil {
			t.Fatal("op-scoped pattern must fire on put")
		}
		if got := link.Stats().Panics.Load(); got != 1 {
			t.Fatalf("panics counter = %d, want 1", got)
		}
	})

	t.Run("first_matching_pattern_wins_even_if_fired", func(t *testing.T) {
		link, _ := newLink(t, 1)
		link.Set(Plan{PanicOnceKeys: []string{"aaa", "bbb"}})
		ctx := context.Background()
		key := "x-aaa-y-bbb-z"
		if rec := runWithRecover(func() { link.Get(ctx, key, store.GetOptions{}) }); rec == nil {
			t.Fatal("first pattern must fire")
		}
		if rec := runWithRecover(func() { link.Get(ctx, key, store.GetOptions{}) }); rec != nil {
			t.Fatalf("no fallback to later patterns after a fired one: %v", rec)
		}
	})
}

// ---- only keys: scope of every probabilistic fault ----

func TestFaultOnlyKeysScope(t *testing.T) {
	t.Run("out_of_scope_keys_proceed", func(t *testing.T) {
		link, truth := newLink(t, 1)
		for _, k := range []string{"wal/other.pack", "wal/manifest.pack"} {
			truth.Put(context.Background(), k, store.PutBody{Bytes: []byte("v")}, store.PutOptions{})
		}
		link.Set(Chaos(1.0).WithOnly("manifest"))
		ctx := context.Background()
		if _, err := link.Get(ctx, "wal/other.pack", store.GetOptions{}); err != nil {
			t.Fatalf("out-of-scope key must proceed: %v", err)
		}
		if _, err := link.Get(ctx, "wal/manifest.pack", store.GetOptions{}); !store.IsRetryable(err) {
			t.Fatalf("in-scope key must fault (all dice at 1.0), got %v", err)
		}
	})

	t.Run("black_hole_deny_panic_ignore_scope", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		link, _ := newLink(t, 1)
		link.Set(Plan{BlackHole: true}.WithOnly("in-scope"))
		if _, err := link.Get(ctx, "out-of-scope", store.GetOptions{}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("black hole must ignore OnlyKeys, got %v", err)
		}

		l2, _ := newLink(t, 1)
		l2.Set(Plan{DenyKeys: []string{"deny"}}.WithOnly("in-scope"))
		if _, err := l2.Get(ctx, "deny/me", store.GetOptions{}); !store.IsNotFound(err) {
			t.Fatalf("deny must ignore OnlyKeys, got %v", err)
		}

		l3, _ := newLink(t, 1)
		l3.Set(Plan{PanicOnceKeys: []string{"boom"}}.WithOnly("in-scope"))
		if rec := runWithRecover(func() { l3.Get(ctx, "boom/key", store.GetOptions{}) }); rec == nil {
			t.Fatal("panic-once must ignore OnlyKeys")
		}
	})
}

// ---- delay and delay_after ----

func TestFaultDelay(t *testing.T) {
	t.Run("delay_before_every_op", func(t *testing.T) {
		link, _ := newLink(t, 1)
		link.Set(Plan{Delay: [2]time.Duration{5 * time.Millisecond, 10 * time.Millisecond}})
		start := time.Now()
		if _, err := link.Head(context.Background(), "k"); err != nil {
			t.Fatalf("head: %v", err)
		}
		if d := time.Since(start); d < 5*time.Millisecond {
			t.Fatalf("op returned in %v, delay floor is 5ms", d)
		}
	})

	t.Run("delay_after_reads_only_and_scoped", func(t *testing.T) {
		link, truth := newLink(t, 1)
		truth.Put(context.Background(), "in/k", store.PutBody{Bytes: []byte("v")}, store.PutOptions{})
		link.Set(Plan{DelayAfter: ptr(30 * time.Millisecond)}.WithOnly("in/"))

		start := time.Now()
		if _, err := link.Get(context.Background(), "in/k", store.GetOptions{}); err != nil {
			t.Fatalf("get: %v", err)
		}
		if d := time.Since(start); d < 30*time.Millisecond {
			t.Fatalf("get returned in %v, delay_after floor is 30ms", d)
		}

		start = time.Now()
		if _, err := link.Get(context.Background(), "out/k", store.GetOptions{}); !store.IsNotFound(err) {
			t.Fatalf("out-of-scope get: %v", err)
		}
		if d := time.Since(start); d > 25*time.Millisecond {
			t.Fatalf("delay_after must honor OnlyKeys, took %v", d)
		}
	})
}

func ptr[T any](v T) *T { return &v }

// ---- decide order (normative §3.2, step by step) ----

func TestDecideOrder(t *testing.T) {
	for _, tc := range []struct {
		name        string
		plan        Plan
		op          string
		key         string
		conditional bool // get: IfNoneMatch set; delete: ifVersion set
		want        func(t *testing.T, s *Stats)
	}{
		{
			name: "panic_beats_black_hole",
			plan: Plan{BlackHole: true, PanicOnceKeys: []string{"k"}},
			op:   "get", key: "k",
			want: func(t *testing.T, s *Stats) {
				if s.Panics.Load() != 1 || s.Hang.Load() != 0 {
					t.Fatalf("panic must win over black hole: %s", s.Summary())
				}
			},
		},
		{
			name: "black_hole_beats_deny_and_dice",
			plan: Plan{BlackHole: true, DenyKeys: []string{"k"}, PErrBefore: 1.0},
			op:   "get", key: "k",
			want: func(t *testing.T, s *Stats) {
				if s.Hang.Load() != 1 || s.Denied.Load() != 0 || s.ErrBefore.Load() != 0 {
					t.Fatalf("black hole must win: %s", s.Summary())
				}
			},
		},
		{
			name: "deny_beats_dice",
			plan: Plan{DenyKeys: []string{"k"}, PErrBefore: 1.0, PStale304: 1.0},
			op:   "get", key: "k", conditional: true,
			want: func(t *testing.T, s *Stats) {
				if s.Denied.Load() != 1 || s.ErrBefore.Load() != 0 || s.Stale304.Load() != 0 {
					t.Fatalf("deny must win over the dice: %s", s.Summary())
				}
			},
		},
		{
			name: "err_before_beats_stale_304_for_conditional_get",
			plan: Plan{PErrBefore: 1.0, PStale304: 1.0},
			op:   "get", key: "k", conditional: true,
			want: func(t *testing.T, s *Stats) {
				if s.ErrBefore.Load() != 1 || s.Stale304.Load() != 0 {
					t.Fatalf("err_before must precede stale_304: %s", s.Summary())
				}
			},
		},
		{
			name: "stale_304_needs_a_conditional_get",
			plan: Plan{PStale304: 1.0},
			op:   "get", key: "k", conditional: false,
			want: func(t *testing.T, s *Stats) {
				if s.Stale304.Load() != 0 {
					t.Fatalf("stale_304 requires if-none-match: %s", s.Summary())
				}
			},
		},
		{
			name: "unconditional_put_skips_cas_fail_takes_err_after",
			plan: Plan{PCASFail: 1.0, PErrAfter: 1.0},
			op:   "put", key: "k",
			want: func(t *testing.T, s *Stats) {
				if s.CASFail.Load() != 0 || s.ErrAfter.Load() != 1 {
					t.Fatalf("unconditional put must skip cas_fail: %s", s.Summary())
				}
			},
		},
		{
			name: "mutations_never_truncate",
			plan: Plan{PErrAfter: 1.0, PTruncate: 1.0},
			op:   "delete", key: "k",
			want: func(t *testing.T, s *Stats) {
				if s.ErrAfter.Load() != 1 || s.Truncate.Load() != 0 {
					t.Fatalf("err_after must precede truncate on mutations: %s", s.Summary())
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link, _ := newLink(t, 1)
			link.Set(tc.plan)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			opts := store.GetOptions{}
			if tc.conditional {
				opts.IfNoneMatch = "v0"
			}
			runWithRecover(func() {
				switch tc.op {
				case "get":
					link.Get(ctx, tc.key, opts)
				case "put":
					link.Put(ctx, tc.key, store.PutBody{Bytes: []byte("v")}, store.PutOptions{Mode: store.PutOverwrite})
				case "delete":
					link.Delete(ctx, tc.key, "")
				}
			})
			tc.want(t, link.Stats())
		})
	}
}

// ---- determinism under a seed ----

type scriptedOp struct {
	op  string // get | getc | put | putcas | delete
	key string
}

func runScripted(t *testing.T, seed uint64, ops []scriptedOp) (*Stats, []string) {
	t.Helper()
	truth := newFakeStore()
	truth.Put(context.Background(), "k", store.PutBody{Bytes: []byte("v")}, store.PutOptions{})
	link := New(truth, "inst", seed)
	link.Set(Chaos(0.9).WithOnly("k"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var outcomes []string
	for _, o := range ops {
		var out string
		switch o.op {
		case "get":
			res, err := link.Get(ctx, o.key, store.GetOptions{})
			out = classifyGet(res, err)
		case "getc":
			res, err := link.Get(ctx, o.key, store.GetOptions{IfNoneMatch: "1"})
			out = classifyGet(res, err)
		case "put":
			_, err := link.Put(ctx, o.key, store.PutBody{Bytes: []byte("v")}, store.PutOptions{Mode: store.PutOverwrite})
			out = outcome(err)
		case "putcas":
			_, err := link.Put(ctx, o.key, store.PutBody{Bytes: []byte("v")}, store.PutOptions{Mode: store.PutUpdate, IfVersion: "1"})
			out = outcome(err)
		case "delete":
			out = outcome(link.Delete(ctx, o.key, "1"))
		}
		outcomes = append(outcomes, o.op+" "+o.key+" -> "+out)
	}
	return link.Stats(), outcomes
}

func classifyGet(res store.GetResult, err error) string {
	if err != nil {
		return outcome(err)
	}
	switch res.(type) {
	case store.Object:
		return "object"
	case store.NotModified:
		return "304"
	default:
		return "unknown"
	}
}

func outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case store.IsRetryable(err):
		return "retryable"
	case store.IsPreconditionFailed(err):
		return "412"
	case store.IsNotFound(err):
		return "404"
	default:
		return "other:" + err.Error()
	}
}

func TestDeterministicUnderSeed(t *testing.T) {
	var ops []scriptedOp
	for range 6 {
		ops = append(ops,
			scriptedOp{"get", "k"}, scriptedOp{"getc", "k"},
			scriptedOp{"put", "k"}, scriptedOp{"putcas", "k"},
			scriptedOp{"delete", "k"})
	}

	s1, o1 := runScripted(t, 22, ops)
	s2, o2 := runScripted(t, 22, ops)
	if strings.Join(o1, "|") != strings.Join(o2, "|") {
		t.Fatalf("same seed must replay identically:\n%v\nvs\n%v", o1, o2)
	}
	if s1.Summary() != s2.Summary() {
		t.Fatalf("same seed must produce identical stats: %s vs %s", s1.Summary(), s2.Summary())
	}

	s3, o3 := runScripted(t, 23, ops)
	if strings.Join(o1, "|") == strings.Join(o3, "|") && s1.Summary() == s3.Summary() {
		t.Fatal("different seed must diverge under Chaos(0.9)")
	}
	if s3.Faults() == 0 || s1.Faults() == 0 {
		t.Fatalf("chaos must inject faults: %d, %d", s1.Faults(), s3.Faults())
	}
	t.Logf("seed 22: %s | seed 23: %s", s1.Summary(), s3.Summary())
}

func runScriptedOutcomes(t *testing.T, seed uint64, ops []scriptedOp) []string {
	t.Helper()
	_, out := runScripted(t, seed, ops)
	return out
}

// ---- stats, budget counting, and trace ----

func TestStatsAndBudgetCounting(t *testing.T) {
	truth := newFakeStore()
	link := New(truth, "inst-a", 22)
	ctx := context.Background()

	// §4.1-style budget counting: snapshot Ops around each awaited op on the
	// acting link only.
	opsBefore := link.Stats().Ops.Load()

	// warm refs sync: 1 conditional GET (0 within freshness TTL) → budget ≤ 1.
	if _, err := store.GetIfChanged(ctx, link, "manifest.pb", "1"); err != nil && !store.IsNotFound(err) {
		t.Fatalf("warm sync: %v", err)
	}
	warm := link.Stats().Ops.Load() - opsBefore
	if warm > 1 {
		t.Fatalf("warm refs sync budget: got %d ops, want <= 1", warm)
	}

	// push (per batch): freshness GET → (pack PUT ∥ log PUT) → manifest CAS
	// → budget ≤ 5.
	pushBefore := link.Stats().Ops.Load()
	if _, err := store.GetIfChanged(ctx, link, "manifest.pb", "1"); err != nil && !store.IsNotFound(err) {
		t.Fatalf("freshness get: %v", err)
	}
	for _, k := range []string{"wal/pack", "log/seg"} {
		if _, err := link.Put(ctx, k, store.PutBody{Bytes: []byte("b")}, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
			t.Fatalf("put %s: %v", k, err)
		}
	}
	if _, err := link.Put(ctx, "manifest.pb", store.PutBody{Bytes: []byte("m")}, store.PutOptions{Mode: store.PutUpdate, IfVersion: "1"}); err != nil && !store.IsPreconditionFailed(err) {
		t.Fatalf("manifest cas: %v", err)
	}
	pushOps := link.Stats().Ops.Load() - pushBefore
	if pushOps > 5 {
		t.Fatalf("push budget: got %d ops, want <= 5", pushOps)
	}

	s := link.Stats()
	if s.Ops.Load() != warm+pushOps {
		t.Fatalf("ops counter = %d, want %d", s.Ops.Load(), warm+pushOps)
	}
	if s.Faults() != 0 {
		t.Fatalf("healthy link must have zero faults: %s", s.Summary())
	}

	// Faults() sums the fault classes; ops excluded.
	link2, _ := newLink(t, 1)
	link2.Set(Plan{PErrBefore: 1.0, DenyKeys: []string{"deny"}})
	runWithRecover(func() { link2.Get(ctx, "deny/me", store.GetOptions{}) }) // denied
	link2.Get(ctx, "ok", store.GetOptions{})                                 // err_before
	if got, want := link2.Stats().Faults(), uint64(2); got != want {
		t.Fatalf("Faults() = %d, want %d", got, want)
	}
	if got, want := link2.Stats().Summary(),
		"ops=2 err_before=1 err_after=0 cas_fail=0 stale_304=0 truncate=0 hang=0 denied=1 panics=0"; got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}

func TestTraceRing(t *testing.T) {
	link, _ := newLink(t, 1)
	ctx := context.Background()

	if got := link.TakeTrace(); got != nil {
		t.Fatalf("trace off must return nil, got %v", got)
	}

	link.Set(Plan{PErrBefore: 1.0})
	link.SetTrace(true)
	link.Get(ctx, "k", store.GetOptions{})
	lines := link.TakeTrace()
	if len(lines) != 1 || lines[0] != "inst-a get k: err-before" {
		t.Fatalf("trace lines = %v", lines)
	}
	if again := link.TakeTrace(); len(again) != 0 {
		t.Fatalf("TakeTrace must empty the ring: %v", again)
	}
	link.SetTrace(false)
	link.Get(ctx, "k", store.GetOptions{})
	if got := link.TakeTrace(); got != nil {
		t.Fatalf("trace after disable = %v", got)
	}
}

// ---- truth oracle helpers ----

func TestOracleHelpers(t *testing.T) {
	truth := newFakeStore()
	link := New(truth, "inst-a", 1)
	link.Set(Plan{PErrBefore: 1.0})
	ctx := context.Background()

	truth.Put(ctx, "repos/o/r/manifest.pb", store.PutBody{Bytes: []byte("manifest")}, store.PutOptions{})
	truth.Put(ctx, "repos/o/r/log/0001.pb", store.PutBody{Bytes: []byte("segment")}, store.PutOptions{})

	b, err := TruthBytes(ctx, truth, "repos/o/r/manifest.pb")
	if err != nil || string(b) != "manifest" {
		t.Fatalf("TruthBytes = %q, %v", b, err)
	}
	if b, err := TruthBytes(ctx, truth, "absent"); err != nil || b != nil {
		t.Fatalf("TruthBytes absent = %q, %v", b, err)
	}

	keys, err := SnapshotKeys(ctx, truth, "repos/o/r/")
	if err != nil {
		t.Fatalf("SnapshotKeys: %v", err)
	}
	if len(keys) != 2 || keys["repos/o/r/manifest.pb"] != int64(len("manifest")) {
		t.Fatalf("SnapshotKeys = %v", keys)
	}

	// Through the faulted link the same get fails — the oracle bypasses it.
	if _, err := link.Get(ctx, "repos/o/r/manifest.pb", store.GetOptions{}); !store.IsRetryable(err) {
		t.Fatalf("faulted link must fail where the oracle succeeds: %v", err)
	}
}

// ---- delegation and passthrough surface ----

func TestDelegation(t *testing.T) {
	link, truth := newLink(t, 1)
	ctx := context.Background()

	truth.Put(ctx, "src", store.PutBody{Bytes: []byte("s")}, store.PutOptions{})
	truth.Put(ctx, "dir/a", store.PutBody{Bytes: []byte("a")}, store.PutOptions{})
	truth.Put(ctx, "dir/b", store.PutBody{Bytes: []byte("b")}, store.PutOptions{})

	if link.Backend() != "memory" {
		t.Fatalf("Backend = %q", link.Backend())
	}
	if !link.SupportsCompose() || !link.ComposeIsNative() {
		t.Fatal("compose support must delegate to inner")
	}
	url, err := link.SignedGetURL(ctx, "k", time.Minute)
	if err != nil || url == nil || *url != "https://signed/k" {
		t.Fatalf("SignedGetURL = %v, %v", url, err)
	}
	at, err := link.AccelTarget(ctx, "k")
	if err != nil || at == nil || at.URL != "https://accel/k" {
		t.Fatalf("AccelTarget = %v, %v", at, err)
	}

	if _, err := link.Put(ctx, "dst", store.PutBody{Bytes: []byte("d")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := link.Delete(ctx, "absent", ""); err != nil {
		t.Fatalf("delete of absent key must be ok: %v", err)
	}
	if _, err := link.Compose(ctx, "composed", []string{"src", "dst"}, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("compose: %v", err)
	}
	if string(truth.bytes("composed")) != "sd" {
		t.Fatalf("composed body = %q, want %q", truth.bytes("composed"), "sd")
	}
	var listed []string
	if err := link.List(ctx, "", "", func(m store.ObjectMeta) error { listed = append(listed, m.Key); return nil }); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 5 {
		t.Fatalf("list = %v", listed)
	}
	var prefixes []string
	if err := link.ListPrefixes(ctx, "", func(p string) error { prefixes = append(prefixes, p); return nil }); err != nil {
		t.Fatalf("list_prefixes: %v", err)
	}
	if len(prefixes) != 1 || prefixes[0] != "dir/" {
		t.Fatalf("list_prefixes = %v", prefixes)
	}

	ok, err := store.Exists(ctx, link, "src")
	if err != nil || !ok {
		t.Fatalf("Exists = %v, %v", ok, err)
	}
}

func TestHealRestoresLiveness(t *testing.T) {
	link, truth := newLink(t, 1)
	truth.Put(context.Background(), "k", store.PutBody{Bytes: []byte("v")}, store.PutOptions{})
	link.Set(Chaos(1.0).WithOnly("k"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	faulted := false
	for range 10 {
		if _, err := link.Get(ctx, "k", store.GetOptions{}); err != nil {
			faulted = true
			break
		}
	}
	if !faulted {
		t.Fatal("chaos(1.0) must fault at least one of ten gets")
	}
	link.Heal()
	faultsBefore := link.Stats().Faults()
	for range 10 {
		if _, err := link.Get(ctx, "k", store.GetOptions{}); err != nil {
			t.Fatalf("healed link must not fault: %v", err)
		}
	}
	if got := link.Stats().Faults(); got != faultsBefore {
		t.Fatalf("healed link injected faults: %s", link.Stats().Summary())
	}
}

// ---- unit-level edges ----

func TestRngUnit(t *testing.T) {
	r := newRng(0) // seed clamped to 1 before mixing
	if want := uint64(1) ^ 0x9E3779B97F4A7C15; r.s != want {
		t.Fatalf("newRng(0) state = %#x, want %#x", r.s, want)
	}
	if got := r.below(0); got != 0 {
		t.Fatalf("below(0) = %d, want 0", got)
	}
	r7 := newRng(7)
	if got := r7.f64(); got < 0 || got >= 1 {
		t.Fatalf("f64 out of [0,1): %v", got)
	}
}

func TestLinkName(t *testing.T) {
	link, _ := newLink(t, 1)
	if link.Name() != "inst-a" {
		t.Fatalf("Name() = %q", link.Name())
	}
}

func TestTruncateZeroSizeObject(t *testing.T) {
	link, truth := newLink(t, 3)
	truth.Put(context.Background(), "k", store.PutBody{Bytes: []byte{}}, store.PutOptions{})
	link.Set(Plan{PTruncate: 1.0})
	res, err := link.Get(context.Background(), "k", store.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	obj := res.(store.Object)
	defer obj.Body.Close()
	b, err := io.ReadAll(obj.Body)
	if len(b) != 0 || err != nil {
		t.Fatalf("zero-size truncation passes an empty body through cleanly (Rust flat_map on an empty stream), got %d bytes, %v", len(b), err)
	}
}

type errReader struct{ err error }

func TestTruncateReaderReadAfterFault(t *testing.T) {
	tr := newTruncateReader("k", io.NopCloser(strings.NewReader("0123456789")), 3, "trunc msg")
	p := make([]byte, 10)
	n, err := tr.Read(p)
	if n != 3 || !store.IsRetryable(err) {
		t.Fatalf("first read after the cut = %d, %v", n, err)
	}
	if _, err := tr.Read(p); !store.IsRetryable(err) {
		t.Fatalf("reads after the cut must keep faulting, got %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func (e errReader) Read(p []byte) (int, error) { return 0, e.err }
func (e errReader) Close() error               { return nil }

func TestTruncateReaderInnerErrorPassesThrough(t *testing.T) {
	boom := errors.New("boom")
	tr := newTruncateReader("k", io.NopCloser(errReader{boom}), 10, "trunc msg")
	if _, err := io.ReadAll(tr); !errors.Is(err, boom) {
		t.Fatalf("inner read error before the cut must pass through, got %v", err)
	}
}

func TestGetTruncatePropagatesInnerError(t *testing.T) {
	link, truth := newLink(t, 3)
	truth.failGet = errors.New("boom-get")
	link.Set(Plan{PTruncate: 1.0})
	if _, err := link.Get(context.Background(), "k", store.GetOptions{}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("truncate decision must propagate an inner get error, got %v", err)
	}
}

func TestErrAfterPropagatesInnerError(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name string
		op   func(ctx context.Context, l *FaultStore, tr *fakeStore) error
	}{
		{"put", func(ctx context.Context, l *FaultStore, tr *fakeStore) error {
			_, err := l.Put(ctx, "k", store.PutBody{Bytes: []byte("v")}, store.PutOptions{Mode: store.PutOverwrite})
			return err
		}},
		{"delete", func(ctx context.Context, l *FaultStore, tr *fakeStore) error {
			return l.Delete(ctx, "k", "")
		}},
		{"compose", func(ctx context.Context, l *FaultStore, tr *fakeStore) error {
			_, err := l.Compose(ctx, "dst", nil, store.PutOptions{Mode: store.PutOverwrite})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link, truth := newLink(t, 1)
			truth.failMutation = boom
			link.Set(Plan{PErrAfter: 1.0})
			if err := tc.op(context.Background(), link, truth); !errors.Is(err, boom) {
				t.Fatalf("err_after must propagate the inner error, got %v", err)
			}
		})
	}
}
