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

// This file pins Forgejo issue #458 (child of the #449 audit, F4+F5):
// fork failure-path residue. (1) A crashed attempt orphans checkpoint
// keys (ShareManifest Creates the checkpoint pair before the child
// manifest); a retry 412s on its own leftovers and — with no fork.json to
// adopt by — 409s permanently. The fix adopts orphan checkpoints by
// semantic compare (timestamps ignored) and adopts an orphan manifest
// when it provably tracks this parent, so the retry converges. (2)
// RollbackShare deleted access.json whenever no fork.json disputed the
// prefix — an adopted pre-existing access.json (EnsureRepoAccess
// adopt-don't-overwrite) could be deleted by a rollback that didn't
// create it. The fix deletes access.json only when this attempt created
// it (the EnsureRepoAccessCreated flag threaded through runFork).

// adoptFailAccessBoot fails the bootstrap without writing, reporting
// created=false (an adopted pre-existing doc that a later failure must
// not roll back).
type adoptFailAccessBoot struct{ err error }

func (f *adoptFailAccessBoot) EnsureRepoAccessCreated(_ context.Context, _, _, _, _ string) (bool, error) {
	return false, f.err
}

// createReportingAccessBoot performs a real Create-or-adopt of distinctive
// bytes and reports whether this call won the Create.
type createReportingAccessBoot struct {
	st   store.ObjectStore
	body string
}

func (f *createReportingAccessBoot) EnsureRepoAccessCreated(ctx context.Context, owner, repo, _, _ string) (bool, error) {
	_, err := store.PutBytes(ctx, f.st, "repos/"+owner+"/"+repo+"/access.json", []byte(f.body),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	if err != nil {
		if store.IsPreconditionFailed(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func mustAccess458(t *testing.T, st store.ObjectStore, owner, name string) string {
	t.Helper()
	raw, _, err := store.GetBytes(context.Background(), st, "repos/"+owner+"/"+name+"/access.json", store.GetOptions{})
	if err != nil || raw == nil {
		t.Fatalf("access.json must be present: %v", err)
	}
	return string(raw)
}

func TestFork458OrphanRetryConverges(t *testing.T) {
	t.Run("crash before manifest adopts checkpoints and completes", func(t *testing.T) {
		e := newTestEnv()
		ex := wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		// First attempt shares, then crashes before the child manifest
		// Create: orphan checkpoint pair, no manifest, no fork.json.
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); err != nil {
			t.Fatalf("share: %v", err)
		}
		if err := e.store.Delete(ctx(), manifestKey("f", "c"), ""); err != nil {
			t.Fatalf("simulate crash: %v", err)
		}
		e.svc.AccessBoot = &fakeAccessBoot{created: true}
		rec := &TaskRecord{Progress: []string{}}
		if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); err != nil {
			t.Fatalf("retry after crash must converge, got: %v", err)
		}
		raw := mustPresent432(t, e.store, ForkKey("f", "c"))
		var doc ForkDoc
		if err := json.Unmarshal(raw, &doc); err != nil || doc.Parent != "o/r" {
			t.Fatalf("fork.json: %s %v", raw, err)
		}
		mustPresent432(t, e.store, manifestKey("f", "c"))
		mustPresent432(t, e.store, "repos/f/c/"+store.CheckpointRefsKey(7))
		mustPresent432(t, e.store, "repos/f/c/"+store.CheckpointKey(7))
	})
	t.Run("crash after manifest adopts the orphan and completes", func(t *testing.T) {
		e := newTestEnv()
		ex := wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		// First attempt shares fully, then crashes before fork.json: the
		// pre-fix retry 412d on its own checkpoint leftovers with no
		// fork.json to adopt by and 409d permanently.
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); err != nil {
			t.Fatalf("share: %v", err)
		}
		e.svc.AccessBoot = &fakeAccessBoot{created: true}
		rec := &TaskRecord{Progress: []string{}}
		if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); err != nil {
			t.Fatalf("retry after crash must converge, got: %v", err)
		}
		raw := mustPresent432(t, e.store, ForkKey("f", "c"))
		var doc ForkDoc
		if err := json.Unmarshal(raw, &doc); err != nil || doc.Parent != "o/r" {
			t.Fatalf("fork.json: %s %v", raw, err)
		}
	})
	t.Run("crash after fork.json adopts provenance and completes", func(t *testing.T) {
		e := newTestEnv()
		ex := wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		// First attempt shares and commits provenance, then crashes
		// before the parent index: the retry's share 412s with our own
		// fork.json on the prefix (defers to the service adopt check)
		// and completes, backfilling the missing Root.
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); err != nil {
			t.Fatalf("share: %v", err)
		}
		stale, _ := json.Marshal(&ForkDoc{Parent: "o/r", ForkedAt: "t", Version: 1})
		if err := e.svc.putCreate(ctx(), ForkKey("f", "c"), stale); err != nil {
			t.Fatalf("seed fork.json: %v", err)
		}
		e.svc.AccessBoot = &fakeAccessBoot{created: true}
		rec := &TaskRecord{Progress: []string{}}
		if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); err != nil {
			t.Fatalf("retry after crash must converge, got: %v", err)
		}
		raw := mustPresent432(t, e.store, ForkKey("f", "c"))
		var doc ForkDoc
		if err := json.Unmarshal(raw, &doc); err != nil || doc.Parent != "o/r" || doc.Root != "o/r" {
			t.Fatalf("fork.json backfilled: %s %v", raw, err)
		}
		ix := mustPresent432(t, e.store, ForksKey("o", "r"))
		if !strings.Contains(string(ix), `"repo":"f/c"`) {
			t.Fatalf("index must list the child: %s", ix)
		}
	})
	t.Run("crash after access bootstrap re-adopts access and completes", func(t *testing.T) {
		e := newTestEnv()
		ex := wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		// First attempt shares and bootstraps access, then crashes before
		// fork.json. The retry must adopt both orphans — and must not
		// overwrite the bootstrapped bytes.
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); err != nil {
			t.Fatalf("share: %v", err)
		}
		first := &createReportingAccessBoot{st: e.store, body: `{"version":1,"first":true}`}
		created, err := first.EnsureRepoAccessCreated(ctx(), "f", "c", "jane@example.com", "private")
		if err != nil || !created {
			t.Fatalf("first bootstrap: %v %v", created, err)
		}
		retry := &createReportingAccessBoot{st: e.store, body: `{"version":1,"second":true}`}
		e.svc.AccessBoot = retry
		rec := &TaskRecord{Progress: []string{}}
		if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); err != nil {
			t.Fatalf("retry after crash must converge, got: %v", err)
		}
		if got := mustAccess458(t, e.store, "f", "c"); got != `{"version":1,"first":true}` {
			t.Fatalf("retry must adopt, not overwrite, access.json: %s", got)
		}
		mustPresent432(t, e.store, ForkKey("f", "c"))
	})
}

func TestFork458OrphanAdoptionFailsClosed(t *testing.T) {
	t.Run("rival checkpoints are 409 and untouched", func(t *testing.T) {
		e := newTestEnv()
		ex := wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		// A rival attempt's checkpoint pair (different refs) occupies the
		// prefix with no manifest: overwriting would corrupt the rival's
		// retry, so this attempt 409s and touches nothing.
		rival := &proto.RefSnapshot{
			Seq: 7, ObjectFormat: "sha1",
			Refs:       []*proto.Ref{{Name: "refs/heads/main", Oid: strings.Repeat("9", 40)}},
			HeadTarget: "refs/heads/main",
		}
		if _, err := store.PutBytes(ctx(), e.store,
			"repos/f/c/"+store.CheckpointRefsKey(7), rival.Marshal(),
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"}); err != nil {
			t.Fatalf("seed rival: %v", err)
		}
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); !errors.Is(err, ErrConflict) {
			t.Fatalf("rival checkpoints: %v", err)
		}
		raw, _, err := store.GetBytes(ctx(), e.store, "repos/f/c/"+store.CheckpointRefsKey(7), store.GetOptions{})
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		got := &proto.RefSnapshot{}
		if err := got.Unmarshal(raw); err != nil || !sameForkSnapshot(got, rival) {
			t.Fatalf("rival bytes must survive untouched")
		}
		e.svc.AccessBoot = &fakeAccessBoot{created: true}
		if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), &TaskRecord{}); !errors.Is(err, ErrConflict) {
			t.Fatalf("runFork on rival residue: %v", err)
		}
	})
	t.Run("corrupt checkpoint occupant is 409", func(t *testing.T) {
		e := newTestEnv()
		ex := wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		if _, err := store.PutBytes(ctx(), e.store,
			"repos/f/c/"+store.CheckpointRefsKey(7), []byte("{bad"),
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream"}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); !errors.Is(err, ErrConflict) {
			t.Fatalf("corrupt occupant: %v", err)
		}
	})
	t.Run("moved parent leaves the stale orphan and names it", func(t *testing.T) {
		e := newTestEnv()
		ex := wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); err != nil {
			t.Fatalf("share: %v", err)
		}
		// The parent moves under the crash (seq 7 → 8): the orphan no
		// longer tracks the parent, and replacing it would need a delete
		// this layer must not issue on doubt — 409 naming the residue.
		if _, err := store.PutBytes(ctx(), e.store, "repos/o/r/"+store.WalDir+"p2.pack", []byte("pack-p2"),
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream"}); err != nil {
			t.Fatalf("seed pack: %v", err)
		}
		raw, meta, err := store.GetBytes(ctx(), e.store, manifestKey("o", "r"), store.GetOptions{})
		if err != nil {
			t.Fatalf("get parent: %v", err)
		}
		pm, err := proto.UnmarshalManifest(raw)
		if err != nil {
			t.Fatalf("unmarshal parent: %v", err)
		}
		pm.HeadSeq = 8
		pm.Revision = 4
		pm.Packs = append(pm.Packs, &proto.PackRef{Checksum: "p2", PackSize: 100})
		if _, err := store.PutBytes(ctx(), e.store, manifestKey("o", "r"), pm.Marshal(),
			store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/x-protobuf"}); err != nil {
			t.Fatalf("advance parent: %v", err)
		}
		e.svc.AccessBoot = &fakeAccessBoot{created: true}
		// Executor level: the stale residue is named so the operator
		// knows the documented exact-key repair.
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "stale reservation") {
			t.Fatalf("moved parent: %v", err)
		}
		// Task level: still a loud 409 (fail closed), orphan survives.
		err = e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), &TaskRecord{})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("moved parent task: %v", err)
		}
		orphan, _, err := store.GetBytes(ctx(), e.store, manifestKey("f", "c"), store.GetOptions{})
		if err != nil || orphan == nil {
			t.Fatalf("stale orphan must survive for the documented repair: %v", err)
		}
		ocm, err := proto.UnmarshalManifest(orphan)
		if err != nil || ocm.Revision != 1 || ocm.HeadSeq != 7 {
			t.Fatalf("orphan: %+v %v", ocm, err)
		}
	})
	t.Run("empty-parent orphan fails closed", func(t *testing.T) {
		e := newTestEnv()
		ex := wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 0, nil)
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); err != nil {
			t.Fatalf("share: %v", err)
		}
		// An empty-parent orphan manifest is byte-identical to a fresh
		// repo manifest (#432): adopting could hijack a live repo, so
		// the retry 409s and the manifest survives.
		e.svc.AccessBoot = &fakeAccessBoot{created: true}
		if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), &TaskRecord{}); !errors.Is(err, ErrConflict) {
			t.Fatalf("empty orphan: %v", err)
		}
		mustPresent432(t, e.store, manifestKey("f", "c"))
	})
	t.Run("checkpoint store outage is not an adoption", func(t *testing.T) {
		e := newTestEnv()
		ex := wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		e.failPutErr("repos/f/c/"+store.CheckpointRefsKey(7), errors.New("disk down"))
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); err == nil || errors.Is(err, ErrConflict) {
			t.Fatalf("outage must surface, not adopt-or-409: %v", err)
		}
	})
	t.Run("unreadable fork.json on orphan adopt fails closed", func(t *testing.T) {
		e := newTestEnv()
		ex := wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); err != nil {
			t.Fatalf("share: %v", err)
		}
		// Ownership unprovable without the fork.json absence proof:
		// conflict, and the orphan survives.
		e.failGet("repos/f/c/fork.json", errors.New("store down"))
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); !errors.Is(err, ErrConflict) {
			t.Fatalf("unreadable fork.json: %v", err)
		}
		mustPresent432(t, e.store, manifestKey("f", "c"))
	})
	t.Run("corrupt checkpoint.pb occupant is 409", func(t *testing.T) {
		e := newTestEnv()
		ex := wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); err != nil {
			t.Fatalf("share: %v", err)
		}
		// Crash between the manifest and a checkpoint repair: refs
		// snapshot intact, checkpoint.pb replaced by garbage, manifest
		// gone. The refs adopt, the garbage 409s, nothing is overwritten.
		for _, k := range []string{manifestKey("f", "c"), "repos/f/c/" + store.CheckpointKey(7)} {
			if err := e.store.Delete(ctx(), k, ""); err != nil {
				t.Fatalf("simulate crash: %v", err)
			}
		}
		if _, err := store.PutBytes(ctx(), e.store, "repos/f/c/"+store.CheckpointKey(7), []byte("{bad"),
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream"}); err != nil {
			t.Fatalf("seed garbage: %v", err)
		}
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); !errors.Is(err, ErrConflict) {
			t.Fatalf("corrupt checkpoint: %v", err)
		}
	})
	t.Run("corrupt manifest occupant is 409", func(t *testing.T) {
		e := newTestEnv()
		ex := wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		if _, err := store.PutBytes(ctx(), e.store, manifestKey("f", "c"), []byte("{bad"),
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream"}); err != nil {
			t.Fatalf("seed garbage: %v", err)
		}
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); !errors.Is(err, ErrConflict) {
			t.Fatalf("corrupt manifest: %v", err)
		}
		mustPresent432(t, e.store, manifestKey("f", "c"))
	})
	occupantManifest := func(packs []*proto.PackRef, cp *proto.CheckpointRef) []byte {
		m := &proto.Manifest{
			FormatVersion: proto.WALFormatVersion,
			Repo:          "f/c",
			ObjectFormat:  "sha1",
			HeadSeq:       7,
			MinSeq:        8,
			Revision:      1,
			Checkpoint:    cp,
			Packs:         packs,
		}
		return m.Marshal()
	}
	t.Run("packs-mismatch occupant is 409", func(t *testing.T) {
		e := newTestEnv()
		ex := wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		// Same seq/revision but a different pack set: not this parent's
		// reservation — hands off.
		body := occupantManifest(
			[]*proto.PackRef{{Checksum: "rival", PackSize: 100}},
			&proto.CheckpointRef{Seq: 7, Key: store.CheckpointKey(7)},
		)
		if _, err := store.PutBytes(ctx(), e.store, manifestKey("f", "c"), body,
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"}); err != nil {
			t.Fatalf("seed occupant: %v", err)
		}
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); !errors.Is(err, ErrConflict) {
			t.Fatalf("packs mismatch: %v", err)
		}
		mustPresent432(t, e.store, manifestKey("f", "c"))
	})
	t.Run("checkpoint-ref-mismatch occupant is 409", func(t *testing.T) {
		e := newTestEnv()
		ex := wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		// Packs track the parent but no checkpoint pointer: not a share
		// reservation — hands off.
		body := occupantManifest(
			[]*proto.PackRef{{Checksum: "p1", PackSize: 100}},
			nil,
		)
		if _, err := store.PutBytes(ctx(), e.store, manifestKey("f", "c"), body,
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"}); err != nil {
			t.Fatalf("seed occupant: %v", err)
		}
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); !errors.Is(err, ErrConflict) {
			t.Fatalf("checkpoint-ref mismatch: %v", err)
		}
		mustPresent432(t, e.store, manifestKey("f", "c"))
	})
	t.Run("manifest store outage is not an adoption", func(t *testing.T) {
		e := newTestEnv()
		ex := wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		e.failPutErr(manifestKey("f", "c"), errors.New("disk down"))
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); err == nil || errors.Is(err, ErrConflict) {
			t.Fatalf("outage must surface, not adopt-or-409: %v", err)
		}
	})
	t.Run("manifest unreadable on orphan adopt fails closed", func(t *testing.T) {
		e := newTestEnv()
		ex := wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		if _, err := store.PutBytes(ctx(), e.store, manifestKey("f", "c"), []byte("{bad"),
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream"}); err != nil {
			t.Fatalf("seed occupant: %v", err)
		}
		// The 412 fires, but the evidence read faults: doubt keeps
		// objects — conflict, occupant survives.
		e.failGet(manifestKey("f", "c"), errors.New("store down"))
		if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); !errors.Is(err, ErrConflict) {
			t.Fatalf("unreadable manifest: %v", err)
		}
		e.clearFails()
		mustPresent432(t, e.store, manifestKey("f", "c"))
	})
}

func TestFork458RollbackSparesAdoptedAccess(t *testing.T) {
	t.Run("adopted pre-existing access.json survives the rollback", func(t *testing.T) {
		e := newTestEnv()
		wireRealFork(e)
		seedParentManifest(t, e.store, "o", "r", 7, []string{"p1"})
		// A pre-existing access.json from an earlier prefix occupant
		// (retired repo residue): this attempt's bootstrap adopts it
		// (created=false) and then fails — the rollback must release the
		// share keys but leave the adopted doc byte-identical.
		mine := `{"version":1,"visibility":"private","adopted":true}`
		if _, err := store.PutBytes(ctx(), e.store, "repos/f/c/access.json", []byte(mine),
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
			t.Fatalf("seed access: %v", err)
		}
		e.svc.AccessBoot = &adoptFailAccessBoot{err: errors.New("access store down")}
		rec := &TaskRecord{Progress: []string{}}
		if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); err == nil {
			t.Fatal("access failure must fail the task")
		}
		if got := mustAccess458(t, e.store, "f", "c"); got != mine {
			t.Fatalf("adopted access.json must survive: %s", got)
		}
		for _, k := range childKeys432("f", "c", 7) {
			mustAbsent432(t, e.store, k)
		}
		joined := strings.Join(rec.Progress, "\n")
		if !strings.Contains(joined, "rolled back share reservation") {
			t.Fatalf("rollback must be narrated, got:\n%s", joined)
		}
	})
	t.Run("executor deletes created access but spares adopted access", func(t *testing.T) {
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
		share := func(t *testing.T, ex *ShareExecutor) {
			t.Helper()
			seedParentManifest(t, ex.Store, "o", "r", 7, []string{"p1"})
			if err := ex.ShareManifest(ctx(), "o/r", "f/c", ForkOptions{}); err != nil {
				t.Fatalf("share: %v", err)
			}
		}
		t.Run("created", func(t *testing.T) {
			ex, st := mk(t)
			share(t, ex)
			if _, err := store.PutBytes(ctx(), st, "repos/f/c/access.json", []byte(`{"v":1}`),
				store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
				t.Fatalf("seed access: %v", err)
			}
			if err := ex.RollbackShare(ctx(), "o/r", "f/c", true); err != nil {
				t.Fatalf("rollback: %v", err)
			}
			for _, k := range append(childKeys432("f", "c", 7), "repos/f/c/access.json") {
				mustAbsent432(t, st, k)
			}
		})
		t.Run("adopted", func(t *testing.T) {
			ex, st := mk(t)
			share(t, ex)
			mine := `{"version":1,"adopted":true}`
			if _, err := store.PutBytes(ctx(), st, "repos/f/c/access.json", []byte(mine),
				store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
				t.Fatalf("seed access: %v", err)
			}
			if err := ex.RollbackShare(ctx(), "o/r", "f/c", false); err != nil {
				t.Fatalf("rollback: %v", err)
			}
			for _, k := range childKeys432("f", "c", 7) {
				mustAbsent432(t, st, k)
			}
			raw, _, err := store.GetBytes(ctx(), st, "repos/f/c/access.json", store.GetOptions{})
			if err != nil || string(raw) != mine {
				t.Fatalf("adopted access.json must survive: %s %v", raw, err)
			}
		})
	})
	t.Run("rollback carries the bootstrap created flag", func(t *testing.T) {
		for _, created := range []bool{true, false} {
			e := newTestEnv()
			fx := &fakeForkExec{}
			e.svc.ForkExec = fx
			e.svc.AccessBoot = &fakeAccessBoot{created: created}
			// Fail the provenance write so the rollback runs.
			e.failPutErr(ForkKey("f", "c"), errors.New("disk down"))
			rec := &TaskRecord{Progress: []string{}}
			if err := e.svc.runFork(ctx(), "o", "r", ForkInput{TargetOwner: "f", Name: "c"}, writer(), rec); err == nil {
				t.Fatal("provenance failure must fail the task")
			}
			if n := fx.rollbackCount(); n != 1 {
				t.Fatalf("created=%v: rollbacks = %d, want 1", created, n)
			}
			fx.mu.Lock()
			got := fx.access
			fx.mu.Unlock()
			if len(got) != 1 || got[0] != created {
				t.Fatalf("created=%v: rollback saw %v", created, got)
			}
		}
	})
}

func TestFork458SemanticCompare(t *testing.T) {
	snap := func() *proto.RefSnapshot {
		return &proto.RefSnapshot{
			Seq: 7, ObjectFormat: "sha1",
			Refs:       []*proto.Ref{{Name: "refs/heads/main", Oid: strings.Repeat("a", 40)}},
			HeadTarget: "refs/heads/main",
		}
	}
	t.Run("snapshots ignore timestamps", func(t *testing.T) {
		a, b := snap(), snap()
		later := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
		ts := proto.TimeFromGo(later)
		b.CreatedAt = &ts
		if !sameForkSnapshot(a, b) {
			t.Fatal("retries re-stamp: timestamps must not block adoption")
		}
		b.HeadTarget = "refs/heads/dev"
		if sameForkSnapshot(a, b) {
			t.Fatal("head target must matter")
		}
		b.HeadTarget = "refs/heads/main"
		b.Refs[0].Oid = strings.Repeat("b", 40)
		if sameForkSnapshot(a, b) {
			t.Fatal("ref oids must matter")
		}
		b.Refs[0].Oid = strings.Repeat("a", 40)
		b.Refs = append(b.Refs, &proto.Ref{Name: "refs/heads/x", Oid: strings.Repeat("c", 40)})
		if sameForkSnapshot(a, b) {
			t.Fatal("ref count must matter")
		}
		if sameForkSnapshot(a, nil) || !sameForkSnapshot(nil, nil) {
			t.Fatal("nil handling")
		}
		withNil := snap()
		withNil.Refs[0] = nil
		if sameForkSnapshot(snap(), withNil) {
			t.Fatal("nil ref entry must matter")
		}
		if !sameForkSnapshot(withNil, withNil) {
			t.Fatal("matching nil ref entries must adopt")
		}
	})
	t.Run("checkpoints ignore timestamps and writer", func(t *testing.T) {
		a := &proto.Checkpoint{
			Seq: 7, ObjectFormat: "sha1",
			Packs:   []*proto.PackRef{{Checksum: "p1", PackSize: 100}},
			RefsKey: store.CheckpointRefsKey(7), RefCount: 1,
			Writer: "walhub",
		}
		b := &proto.Checkpoint{
			Seq: 7, ObjectFormat: "sha1",
			Packs:   []*proto.PackRef{{Checksum: "p1", PackSize: 100}},
			RefsKey: store.CheckpointRefsKey(7), RefCount: 1,
			Writer: "other-host",
		}
		later := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
		ts := proto.TimeFromGo(later)
		b.CreatedAt = &ts
		if !sameForkCheckpoint(a, b) {
			t.Fatal("retries re-stamp: timestamps/writer must not block adoption")
		}
		b.Seq = 8
		if sameForkCheckpoint(a, b) {
			t.Fatal("seq must matter")
		}
		b.Seq = 7
		b.Packs[0].Checksum = "p2"
		if sameForkCheckpoint(a, b) {
			t.Fatal("pack sets must matter")
		}
		b.Packs[0].Checksum = "p1"
		b.Packs = append(b.Packs, &proto.PackRef{Checksum: "p2"})
		if sameForkCheckpoint(a, b) {
			t.Fatal("pack count must matter")
		}
		if sameForkCheckpoint(a, nil) || !sameForkCheckpoint(nil, nil) {
			t.Fatal("nil handling")
		}
		withNil := &proto.Checkpoint{
			Seq: 7, ObjectFormat: "sha1",
			Packs:   []*proto.PackRef{nil},
			RefsKey: store.CheckpointRefsKey(7), RefCount: 1,
		}
		withOne := &proto.Checkpoint{
			Seq: 7, ObjectFormat: "sha1",
			Packs:   []*proto.PackRef{{Checksum: "p1"}},
			RefsKey: store.CheckpointRefsKey(7), RefCount: 1,
		}
		if sameForkCheckpoint(withOne, withNil) {
			t.Fatal("nil pack entry must matter")
		}
		if !sameForkCheckpoint(withNil, withNil) {
			t.Fatal("matching nil pack entries must adopt")
		}
	})
}
