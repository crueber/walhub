// servehealth_test.go — the serve-health sidecar (issue #320): load
// shapes (absent/unreadable/corrupt), write preserving heal backoff
// state, heal-attempt accounting, and the handle mark/clear flag gate.
// Table-driven; `-race` mandatory.
package wal

import (
	"context"
	"errors"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
)

func TestServeHealthLoadShapes(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		seed       []byte // raw sidecar body (nil = absent)
		seedErr    error  // transport error instead of a body
		wantOK     bool
		wantReason string // substring; "" = don't care
	}{
		{name: "absent marker", seed: nil, wantOK: false},
		{name: "valid marker", seed: []byte(`{"version":1,"status":"degraded","reason":"serve sync wait timed out","at":"2026-09-11T00:00:00Z"}`), wantOK: true, wantReason: "timed out"},
		{name: "corrupt marker fails closed", seed: []byte(`{not json`), wantOK: true, wantReason: "unreadable"},
		{name: "transport error reads as unmarked", seedErr: errors.New("store down"), wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var st store.ObjectStore = store.NewMemory()
			if tc.seedErr != nil {
				st = &errStore{ObjectStore: st, err: tc.seedErr}
			} else if tc.seed != nil {
				if _, err := store.PutBytes(ctx, st, store.ServeHealthKey("acme", "api"), tc.seed,
					store.PutOptions{Mode: store.PutOverwrite}); err != nil {
					t.Fatal(err)
				}
			}
			doc, ok := LoadServeHealth(ctx, st, "acme", "api")
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantReason != "" {
				if doc == nil || !contains(doc.Reason, tc.wantReason) {
					t.Fatalf("reason = %+v, want substring %q", doc, tc.wantReason)
				}
			}
		})
	}
}

// errStore fails every Get with err (transport-outage shape).
type errStore struct {
	store.ObjectStore
	err error
}

func (s *errStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	return nil, s.err
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return len(sub) == 0
}

func TestServeHealthWritePreservesHealState(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	prev := &ServeHealth{Version: 1, Status: "degraded", Reason: "old", At: "2026-01-01T00:00:00Z", Attempts: 3, LastHealAt: "2026-09-10T00:00:00Z"}
	if err := WriteServeHealth(ctx, st, "acme", "api", "new failure", prev); err != nil {
		t.Fatal(err)
	}
	doc, ok := LoadServeHealth(ctx, st, "acme", "api")
	if !ok || doc == nil {
		t.Fatal("marker not readable after write")
	}
	if doc.Reason != "new failure" {
		t.Fatalf("reason = %q, want the fresh failure", doc.Reason)
	}
	if doc.Attempts != 3 || doc.LastHealAt != "2026-09-10T00:00:00Z" {
		t.Fatalf("heal state lost: %+v", doc)
	}
}

func TestServeHealthHealAttemptAccounting(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if err := WriteHealAttempt(ctx, st, "acme", "api", "heal probe: boom", nil); err != nil {
		t.Fatal(err)
	}
	doc, _ := LoadServeHealth(ctx, st, "acme", "api")
	if doc.Attempts != 1 || doc.LastHealAt == "" || doc.Reason != "heal probe: boom" {
		t.Fatalf("first attempt = %+v", doc)
	}
	// Second attempt over an older stamp: attempts increments, the
	// stamp advances, an empty reason preserves the last one.
	old := &ServeHealth{Version: 1, Status: "degraded", Reason: "heal probe: boom",
		At: "2026-01-01T00:00:00Z", Attempts: 1, LastHealAt: "2026-01-01T00:00:00Z"}
	if err := WriteHealAttempt(ctx, st, "acme", "api", "", old); err != nil {
		t.Fatal(err)
	}
	doc, _ = LoadServeHealth(ctx, st, "acme", "api")
	if doc.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", doc.Attempts)
	}
	if doc.Reason != "heal probe: boom" {
		t.Fatalf("empty reason should preserve, got %q", doc.Reason)
	}
	if doc.LastHealAt == "2026-01-01T00:00:00Z" {
		t.Fatal("last_heal_at did not advance")
	}
}

func TestServeHealthHandleMarkClearGate(t *testing.T) {
	ctx := context.Background()
	r, st := newTestRegistry(t)
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// A success with no prior failure pays zero store round trips: no
	// marker appears from the clear path alone.
	h.clearServeDegraded()
	if _, ok := LoadServeHealth(ctx, st, "acme", "api"); ok {
		t.Fatal("clear created a marker")
	}
	// Mark → sidecar present, flag set.
	h.markServeDegraded("serve sync wait timed out: objects not servable")
	doc, ok := LoadServeHealth(ctx, st, "acme", "api")
	if !ok || doc.Reason != "serve sync wait timed out: objects not servable" {
		t.Fatalf("marker = %+v %v", doc, ok)
	}
	if !h.serveDegradedFlag().Load() {
		t.Fatal("flag not set after mark")
	}
	// Clear → sidecar gone, flag unset.
	h.clearServeDegraded()
	if _, ok := LoadServeHealth(ctx, st, "acme", "api"); ok {
		t.Fatal("marker survived clear")
	}
	if h.serveDegradedFlag().Load() {
		t.Fatal("flag survived clear")
	}
}

func TestShortErrExported(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "single line passes through", err: errors.New("boom"), want: "boom"},
		{name: "multiline truncates at first break", err: errors.New("first\nsecond"), want: "first"},
		{name: "long errors bounded", err: errors.New(string(make([]byte, 500))), want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ShortErr(tc.err)
			if tc.want != "" && got != tc.want {
				t.Fatalf("ShortErr = %q, want %q", got, tc.want)
			}
			if len(got) > 300 || (len(got) > 0 && (got[len(got)-1] == '\n')) {
				t.Fatalf("ShortErr = %q, want ≤300 single-line", got)
			}
		})
	}
}

// failStore fails sideband writes (Put/Delete) on demand — scoped to
// the serve-health key so repo creation itself still works.
type failStore struct {
	store.ObjectStore
	putErr error
	delErr error
}

func (s *failStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if s.putErr != nil && strings.HasSuffix(key, store.ServeHealthKeySuffix) {
		return store.ObjectMeta{}, s.putErr
	}
	return s.ObjectStore.Put(ctx, key, body, opts)
}

func (s *failStore) Delete(ctx context.Context, key string, ver store.Version) error {
	if s.delErr != nil && strings.HasSuffix(key, store.ServeHealthKeySuffix) {
		return s.delErr
	}
	return s.ObjectStore.Delete(ctx, key, ver)
}

func TestServeHealthSidebandFailures(t *testing.T) {
	ctx := context.Background()
	if _, ok := LoadServeHealth(ctx, nil, "acme", "api"); ok {
		t.Fatal("nil store loaded a marker")
	}
	if err := ClearServeHealth(ctx, nil, "acme", "api"); err != nil {
		t.Fatalf("nil-store clear: %v", err)
	}

	// A lost mark write never fails the caller and leaves no flag lie:
	// the flag is set (a later success retries the clear), the store
	// simply has no marker.
	r, _ := newTestRegistry(t)
	h, err := r.Create(ctx, "acme/flaky", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	_ = h
	r2 := NewRegistry(ctx, &failStore{ObjectStore: store.NewMemory(), putErr: errors.New("put down")}, testConfig(t))
	defer r2.Close()
	h2, err := r2.Create(ctx, "acme/flaky", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h2.markServeDegraded("boom") // lost write: must not panic or fail
	if !h2.serveDegradedFlag().Load() {
		t.Fatal("flag not set after mark attempt")
	}

	// A lost clear keeps the flag (the next success retries the delete).
	r3 := NewRegistry(ctx, &failStore{ObjectStore: store.NewMemory(), delErr: errors.New("delete down")}, testConfig(t))
	defer r3.Close()
	h3, err := r3.Create(ctx, "acme/flaky", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h3.serveDegraded.Store(true)
	h3.clearServeDegraded()
	if !h3.serveDegradedFlag().Load() {
		t.Fatal("flag cleared despite lost delete")
	}
}
