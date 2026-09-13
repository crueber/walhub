package pulls

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// This file pins issue #432: a fork whose share succeeds but whose later
// pre-commit step (access bootstrap, provenance write) fails must release
// the target name — the share reservation rolls back instead of stranding
// the child manifest. Companion cases prove the rollback never deletes
// live objects (prefix-scoped, ownership-verified) and pin the retry
// semantics (reusable after rollback, adopt-or-409 on races).

// wireRealFork wires the production ShareExecutor over the test store with
// a scripted refs reader: the #432 tests exercise the real share +
// rollback key shapes, not the fake.
func wireRealFork(e *testEnv) *ShareExecutor {
	refs := &fakeForkRefs{
		refs: []ForkRef{{Name: "refs/heads/main", Oid: strings.Repeat("a", 40)}},
		head: "refs/heads/main",
	}
	ex := &ShareExecutor{Store: e.flaky, Refs: refs, Host: "test-host", Now: func() time.Time {
		return time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	}}
	e.svc.ForkExec = ex
	return ex
}

func mustAbsent432(t *testing.T, st store.ObjectStore, key string) {
	t.Helper()
	raw, _, err := store.GetBytes(ctx(), st, key, store.GetOptions{})
	if err != nil && !store.IsNotFound(err) {
		t.Fatalf("get %s: %v", key, err)
	}
	if raw != nil {
		t.Fatalf("key %s must be absent", key)
	}
}

func mustPresent432(t *testing.T, st store.ObjectStore, key string) []byte {
	t.Helper()
	raw, _, err := store.GetBytes(ctx(), st, key, store.GetOptions{})
	if err != nil || raw == nil {
		t.Fatalf("key %s must be present: %v", key, err)
	}
	return raw
}

func childKeys432(owner, name string, seq uint64) []string {
	keys := []string{manifestKey(owner, name)}
	if seq != 0 {
		keys = append(keys,
			"repos/"+owner+"/"+name+"/"+store.CheckpointRefsKey(seq),
			"repos/"+owner+"/"+name+"/"+store.CheckpointKey(seq),
		)
	}
	return keys
}

// advancingAccessBoot simulates a racing WAL publish on the child: it
// advances the child manifest past Revision 1, then fails. The rollback
// must refuse to touch the advanced (live) manifest.
type advancingAccessBoot struct{ svc *Service }

func (f *advancingAccessBoot) EnsureRepoAccess(ctx context.Context, owner, repo, creator, visibility string) error {
	_, _ = creator, visibility
	raw, meta, err := store.GetBytes(ctx, f.svc.Store, manifestKey(owner, repo), store.GetOptions{})
	if err != nil || raw == nil {
		return errors.New("access store down")
	}
	cm, err := proto.UnmarshalManifest(raw)
	if err != nil {
		return errors.New("access store down")
	}
	cm.Revision = 2
	if _, err := store.PutBytes(ctx, f.svc.Store, manifestKey(owner, repo), cm.Marshal(),
		store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/x-protobuf"}); err != nil {
		return errors.New("access store down")
	}
	return errors.New("access store down")
}

func TestFork432AccessFailureRollsBackAndReusesName(t *testing.T) {
	e := newTestEnv()
	wireRealFork(e)
	seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
	ab := &fakeAccessBoot{err: errors.New("access store down")}
	e.svc.AccessBoot = ab
	rec := &TaskRecord{Progress: []string{}}
	in := ForkInput{TargetOwner: "f", Name: "c", Visibility: "private"}
	if err := e.svc.runFork(ctx(), "o", "r", in, writer(), rec); err == nil {
		t.Fatal("access failure must fail the task")
	}
	// The reservation is released: every child key is gone, so the
	// pre-check (fork.json-or-manifest probe) passes on retry.
	for _, k := range append(childKeys432("f", "c", 7),
		"repos/f/c/access.json", ForkKey("f", "c")) {
		mustAbsent432(t, e.store, k)
	}
	// The parent is untouched (rollback never reaches past the child).
	mustPresent432(t, e.store, manifestKey("o", "r"))
	mustPresent432(t, e.store, "repos/o/r/"+store.WalDir+"p1.pack")
	joined := strings.Join(rec.Progress, "\n")
	if !strings.Contains(joined, "rolled back share reservation") {
		t.Fatalf("rollback must be narrated, got:\n%s", joined)
	}
	// Retry with access healed completes the fork on the same name.
	ab.mu.Lock()
	ab.err = nil
	ab.mu.Unlock()
	if err := e.svc.runFork(ctx(), "o", "r", in, writer(), rec); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	raw := mustPresent432(t, e.store, ForkKey("f", "c"))
	var doc ForkDoc
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Parent != "o/r" {
		t.Fatalf("fork.json: %s %v", raw, err)
	}
	mustPresent432(t, e.store, manifestKey("f", "c"))
	ix := mustPresent432(t, e.store, ForksKey("o", "r"))
	if !strings.Contains(string(ix), `"repo":"f/c"`) {
		t.Fatalf("index must list the child: %s", ix)
	}
}

func TestFork432RollbackRefusesAdvancedManifest(t *testing.T) {
	e := newTestEnv()
	wireRealFork(e)
	seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
	e.svc.AccessBoot = &advancingAccessBoot{svc: e.svc}
	rec := &TaskRecord{Progress: []string{}}
	if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); err == nil {
		t.Fatal("access failure must fail the task")
	}
	// The manifest advanced under the task: the rollback refuses, and the
	// live objects survive (doubt keeps objects, law 4).
	raw := mustPresent432(t, e.store, manifestKey("f", "c"))
	cm, err := proto.UnmarshalManifest(raw)
	if err != nil || cm.Revision != 2 {
		t.Fatalf("advanced manifest must survive: %+v %v", cm, err)
	}
	for _, k := range childKeys432("f", "c", 7)[1:] {
		mustPresent432(t, e.store, k)
	}
	joined := strings.Join(rec.Progress, "\n")
	if !strings.Contains(joined, "rollback after access bootstrap failure shortfall") {
		t.Fatalf("rollback shortfall must be narrated, got:\n%s", joined)
	}
}

func TestFork432AdoptedShareNeverRollsBack(t *testing.T) {
	e := newTestEnv()
	fx := &fakeForkExec{err: errForkConflict}
	e.svc.ForkExec = fx
	own, _ := json.Marshal(&ForkDoc{Parent: "o/r", Root: "o/r", ForkedAt: "t", Version: 1})
	if err := e.svc.putCreate(ctx(), ForkKey("f", "c"), own); err != nil {
		t.Fatalf("seed: %v", err)
	}
	e.svc.AccessBoot = &fakeAccessBoot{err: errors.New("down")}
	rec := &TaskRecord{Progress: []string{}}
	if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); err == nil {
		t.Fatal("access failure must fail the task")
	}
	// Adopted, not created: no rollback runs, and the pre-existing
	// provenance is byte-identical.
	if n := fx.rollbackCount(); n != 0 {
		t.Fatalf("adopted share must never roll back, got %d", n)
	}
	raw, _, _ := e.svc.getJSON(ctx(), ForkKey("f", "c"))
	if string(raw) != string(own) {
		t.Fatalf("provenance must survive: %s", raw)
	}
}

func TestFork432ProvenanceArbitration(t *testing.T) {
	t.Run("foreign fork.json is 409 and our manifest rolls back", func(t *testing.T) {
		e := newTestEnv()
		wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		foreign, _ := json.Marshal(&ForkDoc{Parent: "x/y", Root: "x/y", ForkedAt: "t", Version: 1})
		if err := e.svc.putCreate(ctx(), ForkKey("f", "c"), foreign); err != nil {
			t.Fatalf("seed: %v", err)
		}
		e.svc.AccessBoot = &fakeAccessBoot{}
		rec := &TaskRecord{Progress: []string{}}
		if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); !errors.Is(err, ErrConflict) {
			t.Fatalf("foreign reservation: %v", err)
		}
		// Our share keys are released; the foreign reservation is
		// untouched — its owner can adopt-retry on a clean prefix.
		for _, k := range childKeys432("f", "c", 7) {
			mustAbsent432(t, e.store, k)
		}
		raw, _, _ := e.svc.getJSON(ctx(), ForkKey("f", "c"))
		if string(raw) != string(foreign) {
			t.Fatalf("foreign fork.json must survive: %s", raw)
		}
	})
	t.Run("stale own fork.json adopts and completes", func(t *testing.T) {
		e := newTestEnv()
		wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		// No Root: a pre-Root reservation; the adopt backfill converges it.
		stale, _ := json.Marshal(&ForkDoc{Parent: "o/r", ForkedAt: "t", Version: 1})
		if err := e.svc.putCreate(ctx(), ForkKey("f", "c"), stale); err != nil {
			t.Fatalf("seed: %v", err)
		}
		e.svc.AccessBoot = &fakeAccessBoot{}
		rec := &TaskRecord{Progress: []string{}}
		if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); err != nil {
			t.Fatalf("stale own reservation must adopt: %v", err)
		}
		mustPresent432(t, e.store, manifestKey("f", "c"))
		raw := mustPresent432(t, e.store, ForkKey("f", "c"))
		var doc ForkDoc
		if err := json.Unmarshal(raw, &doc); err != nil || doc.Parent != "o/r" || doc.Root != "o/r" {
			t.Fatalf("fork.json: %s %v", raw, err)
		}
	})
	t.Run("unreadable fork.json on provenance race fails closed", func(t *testing.T) {
		e := newTestEnv()
		wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		foreign, _ := json.Marshal(&ForkDoc{Parent: "x/y", Root: "x/y", ForkedAt: "t", Version: 1})
		if err := e.svc.putCreate(ctx(), ForkKey("f", "c"), foreign); err != nil {
			t.Fatalf("seed: %v", err)
		}
		e.svc.AccessBoot = &fakeAccessBoot{}
		e.failGet(ForkKey("f", "c"), errors.New("store down"))
		rec := &TaskRecord{Progress: []string{}}
		if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); !errors.Is(err, ErrConflict) {
			t.Fatalf("unreadable reservation: %v", err)
		}
		// Ownership unprovable (and the rollback's own guards fault
		// too): nothing is deleted — doubt keeps objects.
		mustPresent432(t, e.store, manifestKey("f", "c"))
		joined := strings.Join(rec.Progress, "\n")
		if !strings.Contains(joined, "shortfall") {
			t.Fatalf("rollback shortfall must be narrated, got:\n%s", joined)
		}
	})
}

func TestFork432RollbackSharePrefixScoping(t *testing.T) {
	mk := func(t *testing.T) (*ShareExecutor, store.ObjectStore) {
		t.Helper()
		refs := &fakeForkRefs{
			refs: []ForkRef{{Name: "refs/heads/main", Oid: strings.Repeat("a", 40)}},
			head: "refs/heads/main",
		}
		st := store.NewMemory()
		ex := &ShareExecutor{Store: st, Refs: refs, Host: "test-host", Now: func() time.Time {
			return time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
		}}
		return ex, st
	}
	seedBystanders := func(t *testing.T, st store.ObjectStore) {
		t.Helper()
		for _, k := range []string{
			"repos/o/r/access.json",
			"repos/o/r/fork.json",
			"repos/o/r/meta/forks.json",
			"repos/o/sib/manifest.pb",
		} {
			if _, err := store.PutBytes(ctx(), st, k, []byte(`{"v":1}`),
				store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
				t.Fatalf("seed %s: %v", k, err)
			}
		}
	}
	assertBystanders := func(t *testing.T, st store.ObjectStore) {
		t.Helper()
		mustPresent432(t, st, manifestKey("o", "r"))
		mustPresent432(t, st, "repos/o/r/"+store.WalDir+"p1.pack")
		mustPresent432(t, st, "repos/o/r/"+store.WalDir+"p2.pack")
		for _, k := range []string{
			"repos/o/r/access.json",
			"repos/o/r/fork.json",
			"repos/o/r/meta/forks.json",
			"repos/o/sib/manifest.pb",
		} {
			mustPresent432(t, st, k)
		}
	}
	t.Run("releases all share keys and nothing else", func(t *testing.T) {
		ex, st := mk(t)
		seedParentManifest(t, st, "o", "r", 7, []string{"p1", "p2"})
		seedBystanders(t, st)
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); err != nil {
			t.Fatalf("share: %v", err)
		}
		if _, err := store.PutBytes(ctx(), st, "repos/f/c/access.json", []byte(`{"v":1}`),
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
			t.Fatalf("seed access: %v", err)
		}
		if err := ex.RollbackShare(ctx(), "o/r", "f/c"); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		for _, k := range append(childKeys432("f", "c", 7), "repos/f/c/access.json") {
			mustAbsent432(t, st, k)
		}
		assertBystanders(t, st)
	})
	t.Run("disputed prefix keeps access.json", func(t *testing.T) {
		ex, st := mk(t)
		seedParentManifest(t, st, "o", "r", 7, []string{"p1", "p2"})
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); err != nil {
			t.Fatalf("share: %v", err)
		}
		for _, k := range []string{"repos/f/c/fork.json", "repos/f/c/access.json"} {
			if _, err := store.PutBytes(ctx(), st, k, []byte(`{"v":1}`),
				store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
				t.Fatalf("seed %s: %v", k, err)
			}
		}
		if err := ex.RollbackShare(ctx(), "o/r", "f/c"); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		// Share keys released; the disputed reservation's own keys stay.
		for _, k := range childKeys432("f", "c", 7) {
			mustAbsent432(t, st, k)
		}
		mustPresent432(t, st, "repos/f/c/fork.json")
		mustPresent432(t, st, "repos/f/c/access.json")
	})
	t.Run("empty parent releases its single key", func(t *testing.T) {
		ex, st := mk(t)
		seedParentManifest(t, st, "o", "r", 0, nil)
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); err != nil {
			t.Fatalf("share: %v", err)
		}
		if err := ex.RollbackShare(ctx(), "o/r", "f/c"); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		mustAbsent432(t, st, manifestKey("f", "c"))
		mustPresent432(t, st, manifestKey("o", "r"))
	})
	t.Run("absent manifest is a nil no-op", func(t *testing.T) {
		ex, _ := mk(t)
		if err := ex.RollbackShare(ctx(), "o/r", "f/c"); err != nil {
			t.Fatalf("rollback: %v", err)
		}
	})
	t.Run("advanced manifest refuses without deleting", func(t *testing.T) {
		ex, st := mk(t)
		seedParentManifest(t, st, "o", "r", 7, []string{"p1"})
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); err != nil {
			t.Fatalf("share: %v", err)
		}
		raw, meta, err := store.GetBytes(ctx(), st, manifestKey("f", "c"), store.GetOptions{})
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		cm, err := proto.UnmarshalManifest(raw)
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		cm.Revision = 2
		if _, err := store.PutBytes(ctx(), st, manifestKey("f", "c"), cm.Marshal(),
			store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/x-protobuf"}); err != nil {
			t.Fatalf("advance: %v", err)
		}
		if err := ex.RollbackShare(ctx(), "o/r", "f/c"); !errors.Is(err, ErrConflict) {
			t.Fatalf("advanced manifest: %v", err)
		}
		for _, k := range childKeys432("f", "c", 7) {
			mustPresent432(t, st, k)
		}
	})
	t.Run("foreign manifest refuses without deleting", func(t *testing.T) {
		ex, st := mk(t)
		seedParentManifest(t, st, "o", "r", 7, []string{"p1"})
		alien := &proto.Manifest{
			FormatVersion: proto.WALFormatVersion,
			Repo:          "x/y",
			ObjectFormat:  "sha1",
			HeadSeq:       7,
			MinSeq:        8,
			Revision:      1,
		}
		if _, err := store.PutBytes(ctx(), st, manifestKey("f", "c"), alien.Marshal(),
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := ex.RollbackShare(ctx(), "o/r", "f/c"); !errors.Is(err, ErrConflict) {
			t.Fatalf("foreign manifest: %v", err)
		}
		mustPresent432(t, st, manifestKey("f", "c"))
	})
	t.Run("corrupt manifest refuses without deleting", func(t *testing.T) {
		ex, st := mk(t)
		if _, err := store.PutBytes(ctx(), st, manifestKey("f", "c"), []byte("{bad"),
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream"}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := ex.RollbackShare(ctx(), "o/r", "f/c"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("corrupt manifest: %v", err)
		}
		mustPresent432(t, st, manifestKey("f", "c"))
	})
	t.Run("bad child shape is invalid", func(t *testing.T) {
		ex, _ := mk(t)
		if err := ex.RollbackShare(ctx(), "o/r", "bogus"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad child: %v", err)
		}
	})
}
