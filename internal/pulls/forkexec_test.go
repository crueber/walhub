package pulls

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// fakeForkRefs scripts the parent's live refs.
type fakeForkRefs struct {
	refs []ForkRef
	head string
	err  error
}

func (f *fakeForkRefs) ParentRefs(_ context.Context, _ string) ([]ForkRef, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	return f.refs, f.head, nil
}

func seedParentManifest(t *testing.T, st store.ObjectStore, owner, repo string, head uint64, packs []string) {
	t.Helper()
	prefs := make([]*proto.PackRef, 0, len(packs))
	for _, c := range packs {
		prefs = append(prefs, &proto.PackRef{Checksum: c, PackSize: 100, Tier: 0})
		if _, err := store.PutBytes(context.Background(), st,
			"repos/"+owner+"/"+repo+"/"+store.WalDir+c+".pack", []byte("pack-"+c),
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream"}); err != nil {
			t.Fatalf("seed pack %s: %v", c, err)
		}
	}
	m := &proto.Manifest{
		FormatVersion: proto.WALFormatVersion,
		Repo:          owner + "/" + repo,
		ObjectFormat:  "sha1",
		HeadSeq:       head,
		MinSeq:        0,
		Revision:      3,
		Writer:        "test",
		Packs:         prefs,
	}
	if _, err := store.PutBytes(context.Background(), st, manifestKey(owner, repo), m.Marshal(),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"}); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
}

func TestShareExecutor(t *testing.T) {
	mk := func(t *testing.T) (*ShareExecutor, store.ObjectStore, *fakeForkRefs) {
		t.Helper()
		refs := &fakeForkRefs{
			refs: []ForkRef{
				{Name: "refs/heads/main", Oid: strings.Repeat("a", 40)},
				{Name: "refs/heads/dev", Oid: strings.Repeat("b", 40)},
			},
			head: "refs/heads/main",
		}
		st := store.NewMemory()
		ex := &ShareExecutor{Store: st, Refs: refs, Host: "test-host", Now: func() time.Time {
			return time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
		}}
		return ex, st, refs
	}
	t.Run("shares packs verbatim with fresh snapshot", func(t *testing.T) {
		ex, st, _ := mk(t)
		seedParentManifest(t, st, "o", "r", 7, []string{"p1", "p2"})
		opt := ForkOptions{Description: "a \"quoted\" fork\nline2", Creator: "Jane@X"}
		if err := ex.ShareManifest(context.Background(), "o/r", "f/c", opt); err != nil {
			t.Fatalf("share: %v", err)
		}
		raw, _, err := store.GetBytes(context.Background(), st, manifestKey("f", "c"), store.GetOptions{})
		if err != nil || raw == nil {
			t.Fatalf("child manifest: %v", err)
		}
		cm, err := proto.UnmarshalManifest(raw)
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if cm.Repo != "f/c" || cm.HeadSeq != 7 || cm.MinSeq != 8 || cm.Revision != 1 {
			t.Fatalf("child manifest: %+v", cm)
		}
		if len(cm.Packs) != 2 || cm.Packs[0].Checksum != "p1" || cm.Packs[1].Checksum != "p2" {
			t.Fatalf("packs: %+v", cm.Packs)
		}
		if len(cm.LogSegments) != 0 {
			t.Fatalf("segments: %+v", cm.LogSegments)
		}
		if cm.Checkpoint == nil || cm.Checkpoint.Seq != 7 || cm.Checkpoint.Key != store.CheckpointKey(7) {
			t.Fatalf("checkpoint ref: %+v", cm.Checkpoint)
		}
		if cm.Settings == nil || cm.Settings.Revision != 1 || cm.Settings.Author != "jane@x" {
			t.Fatalf("settings: %+v", cm.Settings)
		}
		if !strings.Contains(cm.Settings.Toml, `description = "a \"quoted\" fork\nline2"`) {
			t.Fatalf("toml: %q", cm.Settings.Toml)
		}
		// Fresh refs snapshot: sorted, parent HEAD by default.
		rraw, _, err := store.GetBytes(context.Background(), st, childKey("f", "c", store.CheckpointRefsKey(7)), store.GetOptions{})
		if err != nil || rraw == nil {
			t.Fatalf("refs.pb: %v", err)
		}
		snap := &proto.RefSnapshot{}
		if err := snap.Unmarshal(rraw); err != nil {
			t.Fatalf("refs unmarshal: %v", err)
		}
		if snap.HeadTarget != "refs/heads/main" || len(snap.Refs) != 2 || snap.Refs[0].Name != "refs/heads/dev" {
			t.Fatalf("snapshot: %+v", snap)
		}
		// Checkpoint carries the verbatim pack set.
		craw, _, err := store.GetBytes(context.Background(), st, childKey("f", "c", store.CheckpointKey(7)), store.GetOptions{})
		if err != nil || craw == nil {
			t.Fatalf("checkpoint.pb: %v", err)
		}
		cp := &proto.Checkpoint{}
		if err := cp.Unmarshal(craw); err != nil {
			t.Fatalf("checkpoint unmarshal: %v", err)
		}
		if len(cp.Packs) != 2 || cp.RefsKey != store.CheckpointRefsKey(7) || cp.RefCount != 2 {
			t.Fatalf("checkpoint: %+v", cp)
		}
	})
	t.Run("branch override", func(t *testing.T) {
		ex, st, _ := mk(t)
		seedParentManifest(t, st, "o", "r", 3, []string{"p1"})
		if err := ex.ShareManifest(context.Background(), "o/r", "f/c", ForkOptions{Branch: "refs/heads/dev"}); err != nil {
			t.Fatalf("share: %v", err)
		}
		rraw, _, _ := store.GetBytes(context.Background(), st, childKey("f", "c", store.CheckpointRefsKey(3)), store.GetOptions{})
		snap := &proto.RefSnapshot{}
		_ = snap.Unmarshal(rraw)
		if snap.HeadTarget != "refs/heads/dev" {
			t.Fatalf("head: %+v", snap)
		}
	})
	t.Run("unknown branch is 422", func(t *testing.T) {
		ex, st, _ := mk(t)
		seedParentManifest(t, st, "o", "r", 3, []string{"p1"})
		err := ex.ShareManifest(context.Background(), "o/r", "f/c", ForkOptions{Branch: "refs/heads/nope"})
		if err == nil || !isUnprocessable(err) {
			t.Fatalf("branch: %v", err)
		}
	})
	t.Run("empty parent", func(t *testing.T) {
		ex, st, _ := mk(t)
		seedParentManifest(t, st, "o", "r", 0, nil)
		if err := ex.ShareManifest(context.Background(), "o/r", "f/c", ForkOptions{}); err != nil {
			t.Fatalf("share: %v", err)
		}
		raw, _, _ := store.GetBytes(context.Background(), st, manifestKey("f", "c"), store.GetOptions{})
		cm, _ := proto.UnmarshalManifest(raw)
		if cm.HeadSeq != 0 || cm.MinSeq != 0 || cm.Checkpoint != nil || len(cm.Packs) != 0 {
			t.Fatalf("empty child: %+v", cm)
		}
		if err := ex.ShareManifest(context.Background(), "o/r", "f/c2", ForkOptions{Branch: "refs/heads/main"}); !isUnprocessable(err) {
			t.Fatalf("branch on empty: %v", err)
		}
	})
	t.Run("missing parent is 404", func(t *testing.T) {
		ex, _, _ := mk(t)
		if err := ex.ShareManifest(context.Background(), "o/r", "f/c", ForkOptions{}); !isNotFound(err) {
			t.Fatalf("missing: %v", err)
		}
	})
	t.Run("missing pack fails loud", func(t *testing.T) {
		ex, st, _ := mk(t)
		seedParentManifest(t, st, "o", "r", 3, []string{"p1"})
		_ = st.Delete(context.Background(), "repos/o/r/"+store.WalDir+"p1.pack", "")
		if err := ex.ShareManifest(context.Background(), "o/r", "f/c", ForkOptions{}); err == nil {
			t.Fatal("missing pack must fail")
		}
	})
	t.Run("taken child is 409", func(t *testing.T) {
		ex, st, _ := mk(t)
		seedParentManifest(t, st, "o", "r", 3, []string{"p1"})
		seedParentManifest(t, st, "f", "c", 0, nil)
		if err := ex.ShareManifest(context.Background(), "o/r", "f/c", ForkOptions{}); !isConflict(err) {
			t.Fatalf("taken: %v", err)
		}
	})
	t.Run("no description means no settings", func(t *testing.T) {
		ex, st, _ := mk(t)
		seedParentManifest(t, st, "o", "r", 2, nil)
		if err := ex.ShareManifest(context.Background(), "o/r", "f/c", ForkOptions{Description: "  "}); err != nil {
			t.Fatalf("share: %v", err)
		}
		raw, _, _ := store.GetBytes(context.Background(), st, manifestKey("f", "c"), store.GetOptions{})
		cm, _ := proto.UnmarshalManifest(raw)
		if cm.Settings != nil {
			t.Fatalf("settings: %+v", cm.Settings)
		}
	})
}

func isNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

func isUnprocessable(err error) bool { return errors.Is(err, ErrUnprocessable) }

func isConflict(err error) bool { return errors.Is(err, ErrConflict) }

func TestShareExecutorEdges(t *testing.T) {
	ctx := context.Background()
	t.Run("bad ids", func(t *testing.T) {
		ex := &ShareExecutor{Store: store.NewMemory()}
		if err := ex.ShareManifest(ctx, "bogus", "f/c", ForkOptions{}); err == nil {
			t.Fatal("bad parent must fail")
		}
		if err := ex.ShareManifest(ctx, "o/r", "bogus", ForkOptions{}); err == nil {
			t.Fatal("bad child must fail")
		}
	})
	t.Run("unwired store", func(t *testing.T) {
		ex := &ShareExecutor{}
		if err := ex.ShareManifest(ctx, "o/r", "f/c", ForkOptions{}); err == nil {
			t.Fatal("nil store must fail")
		}
	})
	t.Run("corrupt parent manifest", func(t *testing.T) {
		st := store.NewMemory()
		_, _ = store.PutBytes(ctx, st, manifestKey("o", "r"), []byte("{bad"),
			store.PutOptions{Mode: store.PutCreate})
		ex := &ShareExecutor{Store: st}
		if err := ex.ShareManifest(ctx, "o/r", "f/c", ForkOptions{}); err == nil {
			t.Fatal("corrupt manifest must fail")
		}
	})
	t.Run("bad object format", func(t *testing.T) {
		st := store.NewMemory()
		m := &proto.Manifest{Repo: "o/r", ObjectFormat: "md5", HeadSeq: 1, Revision: 1}
		_, _ = store.PutBytes(ctx, st, manifestKey("o", "r"), m.Marshal(),
			store.PutOptions{Mode: store.PutCreate})
		ex := &ShareExecutor{Store: st}
		if err := ex.ShareManifest(ctx, "o/r", "f/c", ForkOptions{}); err == nil {
			t.Fatal("bad format must fail")
		}
	})
	t.Run("unwired refs on live parent", func(t *testing.T) {
		st := store.NewMemory()
		seedParentManifest(t, st, "o", "r", 2, nil)
		ex := &ShareExecutor{Store: st}
		if err := ex.ShareManifest(ctx, "o/r", "f/c", ForkOptions{}); err == nil {
			t.Fatal("nil refs must fail on a live parent")
		}
	})
	t.Run("refs outage fails loud", func(t *testing.T) {
		st := store.NewMemory()
		seedParentManifest(t, st, "o", "r", 2, nil)
		ex := &ShareExecutor{Store: st, Refs: &fakeForkRefs{err: errTestDown}}
		if err := ex.ShareManifest(ctx, "o/r", "f/c", ForkOptions{}); err == nil {
			t.Fatal("refs outage must fail")
		}
	})
}

// bumpRefs mutates the parent manifest on call (a racing parent push).
type bumpRefs struct {
	st     store.ObjectStore
	owner  string
	repo   string
	head   uint64
	refs   []ForkRef
	calls  int
	always bool
}

func (b *bumpRefs) ParentRefs(ctx context.Context, _ string) ([]ForkRef, string, error) {
	b.calls++
	if b.always || b.calls == 1 {
		b.head++
		m := &proto.Manifest{Repo: b.owner + "/" + b.repo, ObjectFormat: "sha1",
			HeadSeq: b.head, MinSeq: 0, Revision: b.head + 2, Writer: "test"}
		_, meta, _ := store.GetBytes(ctx, b.st, manifestKey(b.owner, b.repo), store.GetOptions{})
		_, _ = store.PutBytes(ctx, b.st, manifestKey(b.owner, b.repo), m.Marshal(),
			store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/x-protobuf"})
	}
	return b.refs, "refs/heads/main", nil
}

func TestShareExecutorRacingParent(t *testing.T) {
	ctx := context.Background()
	mkrefs := func() []ForkRef {
		return []ForkRef{{Name: "refs/heads/main", Oid: strings.Repeat("a", 40)}}
	}
	t.Run("converges after one race", func(t *testing.T) {
		st := store.NewMemory()
		seedParentManifest(t, st, "o", "r", 3, []string{"p1"})
		br := &bumpRefs{st: st, owner: "o", repo: "r", head: 3, refs: mkrefs()}
		ex := &ShareExecutor{Store: st, Refs: br, Host: "test"}
		if err := ex.ShareManifest(ctx, "o/r", "f/c", ForkOptions{}); err != nil {
			t.Fatalf("share: %v", err)
		}
		if br.calls != 2 {
			t.Fatalf("calls = %d, want 2 (torn first attempt retried)", br.calls)
		}
		raw, _, _ := store.GetBytes(ctx, st, manifestKey("f", "c"), store.GetOptions{})
		cm, _ := proto.UnmarshalManifest(raw)
		if cm.HeadSeq != 4 || cm.MinSeq != 5 {
			t.Fatalf("child tracks the settled parent: %+v", cm)
		}
	})
	t.Run("hot parent fails loud", func(t *testing.T) {
		st := store.NewMemory()
		seedParentManifest(t, st, "o", "r", 3, []string{"p1"})
		br := &bumpRefs{st: st, owner: "o", repo: "r", head: 3, refs: mkrefs(), always: true}
		ex := &ShareExecutor{Store: st, Refs: br, Host: "test"}
		if err := ex.ShareManifest(ctx, "o/r", "f/c", ForkOptions{}); !errors.Is(err, ErrConflict) {
			t.Fatalf("hot parent: %v", err)
		}
	})
}
