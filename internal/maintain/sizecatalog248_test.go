// sizecatalog248_test.go — Forgejo #248 maintainer fold wiring: one bounded
// pass folds per-repo sidecars + the aggregate catalog; best-effort (never
// fails the pass); resumable via the in-memory cursor.
package maintain

import (
	"context"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/sizecatalog"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func TestFoldSizeCatalog(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	eng := newFakeEngine(cfg)
	eng.repos = []string{"o/big", "o/small"}
	putManifest := func(id string, packs []*proto.PackRef, head uint64) {
		t.Helper()
		m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: id, ObjectFormat: "sha1", HeadSeq: head, Revision: 1, Packs: packs}
		if _, err := eng.st.Put(ctx, "repos/"+id+"/manifest.pb", store.PutBody{Bytes: m.Marshal()}, store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
	}
	putManifest("o/small", []*proto.PackRef{{Checksum: "a", PackSize: 100, IdxSize: 10, ObjectCount: 2}}, 1)
	putManifest("o/big", []*proto.PackRef{{Checksum: "b", PackSize: 900, IdxSize: 100, ObjectCount: 8}}, 3)

	m := New(eng, Options{Logf: func(string, ...any) {}})
	m.foldSizeCatalog(ctx)

	for id, want := range map[string]uint64{"o/small": 110, "o/big": 1000} {
		body, _, err := store.GetBytes(ctx, eng.st, "repos/"+id+"/meta/stats.json", store.GetOptions{})
		if err != nil || body == nil {
			t.Fatalf("%s sidecar: %v", id, err)
		}
		s, ok, err := sizecatalog.DecodeStats(body)
		if err != nil || !ok || s.SizeBytes != want {
			t.Fatalf("%s sidecar: %+v %v %v", id, s, ok, err)
		}
	}
	cat, err := sizecatalog.ReadCatalog(ctx, eng.st)
	if err != nil || len(cat.Entries) != 2 {
		t.Fatalf("catalog: %v %+v", err, cat)
	}
	// Idempotent: second fold changes nothing and clears no cursor.
	m.foldSizeCatalog(ctx)
	m.sizeMu.Lock()
	cur := m.sizeCursor
	m.sizeMu.Unlock()
	if cur != "" {
		t.Fatalf("cursor after complete pass: %q", cur)
	}
}
