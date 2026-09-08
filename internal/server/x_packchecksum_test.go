package server

// x_packchecksum_test.go — #205: the push path must record bare-hex pack
// checksums (02_storage_protobuf.md §2.2: PackRef.checksum; key =
// wal/<checksum>.pack). Producers derived `pack-<hex>` from git's on-disk
// names (TrimSuffix-only), violating the bucket contract and stacking a new
// `pack-` layer per push; a pack-less idx additionally produced a hollow
// claim that silently skipped the pack-body upload.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// isBareHex reports whether s is a bare pack checksum (40/64 lowercase hex,
// no `pack-` infix).
func isBareHex(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// newScratchGitCmd builds a git command in a scratch repo with the pinned
// test identity (same env as gitCommitPack).
func newScratchGitCmd(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "HOME=" + dir}
	return cmd
}

// gitCommitMore appends a second commit to a gitCommitPack scratch repo and
// returns its pack + oid (descendant, hence distinct, pack bytes).
func gitCommitMore(t *testing.T, dir, content string) ([]byte, string) {
	t.Helper()
	run := func(args ...string) string {
		cmd := newScratchGitCmd(dir, args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	f, err := os.OpenFile(filepath.Join(dir, "f.txt"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "c2")
	oid := strings.TrimSpace(run("rev-parse", "HEAD"))
	var buf bytes.Buffer
	cmd := newScratchGitCmd(dir, "pack-objects", "--revs", "--stdout")
	cmd.Stdin = strings.NewReader(oid + "\n")
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("pack-objects: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("empty pack")
	}
	return buf.Bytes(), oid
}

// readTestManifest loads the repo manifest straight from the store.
func readTestManifest(t *testing.T, ctx context.Context, st store.ObjectStore, id git.RepoId) *proto.Manifest {
	t.Helper()
	body, _, err := store.GetBytes(ctx, st, id.StorePrefix()+store.Manifest, store.GetOptions{})
	if err != nil || body == nil {
		t.Fatalf("manifest read: %v %v", body == nil, err)
	}
	m, err := proto.UnmarshalManifest(body)
	if err != nil {
		t.Fatalf("manifest decode: %v", err)
	}
	return m
}

// TestReceivePackPushBareChecksums pushes two real packs through the HTTP
// receive-pack path (real WAL engine + memory store) and pins the #205
// contract: every manifest checksum is bare hex, every pack body + idx
// lands under its bare key (no silent upload skip), and no prefixed or
// doubly-prefixed key is ever written.
func TestReceivePackPushBareChecksums(t *testing.T) {
	ctx := context.Background()
	cfg := walTestCfg(t)
	cfg.Server.AutoCreateOnPush = true
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, cfg)
	defer reg.Close()
	s, _ := newTestServer(t, func(o *Options) {
		o.Config = cfg
		o.Store = st
		o.Engine = NewWalEngine(reg, cfg)
	})
	id := mustRepoID(t, "o/r")

	push := func(old, ref string, pack []byte, oid string) {
		t.Helper()
		var b bytes.Buffer
		b.Write(git.Pkt(old + " " + oid + " " + ref + "\000report-status side-band-64k object-format=sha1\n"))
		b.Write(git.Flush())
		b.Write(pack)
		req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", bytes.NewReader(b.Bytes()))
		req.Header.Set("Authorization", "Bearer tok123")
		req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
		rec := httptest.NewRecorder()
		s.receivePackLocal(rec, req, id, principalAlice)
		if rec.Code != http.StatusOK {
			t.Fatalf("push %s: status = %d body=%s", ref, rec.Code, rec.Body.String())
		}
		out := rec.Body.String()
		if !strings.Contains(out, "unpack ok") || !strings.Contains(out, "ok "+ref) {
			t.Fatalf("push %s report missing: %q", ref, out)
		}
	}

	// Two descendant commits from one scratch repo: oids (and packs) are
	// distinct by construction, so the manifest must hold two packs.
	scratch := t.TempDir()
	pack1, oid1 := gitCommitPack(t, scratch)
	pack2, oid2 := gitCommitMore(t, scratch, "second line\n")
	push(strings.Repeat("0", 40), "refs/heads/main", pack1, oid1)
	push(oid1, "refs/heads/main", pack2, oid2)

	m := readTestManifest(t, ctx, st, id)
	if len(m.Packs) != 2 {
		t.Fatalf("manifest packs = %d, want 2 (one per push, no duplicates)", len(m.Packs))
	}
	for _, p := range m.Packs {
		if !isBareHex(p.Checksum) {
			t.Fatalf("manifest checksum = %q, want bare hex (no pack- infix)", p.Checksum)
		}
		for _, suf := range []string{".pack", ".idx"} {
			ok, err := store.Exists(ctx, st, id.StorePrefix()+"wal/"+p.Checksum+suf)
			if err != nil || !ok {
				t.Fatalf("wal/%s%s missing (pack upload skipped): %v %v", p.Checksum, suf, ok, err)
			}
		}
	}
	// No prefixed key may exist anywhere under wal/ (the pre-#205 shape).
	if err := st.List(ctx, id.StorePrefix()+"wal/", "", func(meta store.ObjectMeta) error {
		rel := strings.TrimPrefix(meta.Key, id.StorePrefix()+"wal/")
		if strings.HasPrefix(rel, "pack-") || strings.HasPrefix(rel, "pack-pack-") {
			t.Errorf("prefixed bucket key written: %s", meta.Key)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestWalEngineNewLocalPackShapes is the table-driven #205 matrix for
// newLocalPack: canonical and non-canonical on-disk names, stray idx
// files, and legacy (prefixed-checksum) manifests.
func TestWalEngineNewLocalPackShapes(t *testing.T) {
	ctx := context.Background()
	hexA := strings.Repeat("a", 40)
	hexB := strings.Repeat("b", 40)

	// seedLegacy publishes a pre-#205 COMPACT entry whose checksum keeps
	// git's on-disk `pack-` infix, and returns the serving pack dir.
	seedLegacy := func(t *testing.T, h *wal.RepoHandle, hex string) string {
		t.Helper()
		pack := []byte("legacy pack " + hex)
		sum := "pack-" + hex
		packPath := filepath.Join(t.TempDir(), "pack-"+hex+".pack")
		idxPath := filepath.Join(t.TempDir(), "pack-"+hex+".idx")
		if err := os.WriteFile(packPath, pack, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(idxPath, []byte("idx"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := h.PublishCompact(ctx, &wal.PreparedPack{
			Checksum: sum, PackPath: packPath, IdxPath: idxPath,
			PackSize: uint64(len(pack)), IdxSize: 3,
		}, nil, nil); err != nil {
			t.Fatalf("seed legacy compact: %v", err)
		}
		return h.Repo().PackDir()
	}
	writePair := func(t *testing.T, dir, base string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, base+".pack"), []byte("pack-bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, base+".idx"), []byte("idx-bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name string
		// setup prepares the pack dir (and optionally the manifest);
		// wantSum is "" when newLocalPack must return nil.
		setup   func(t *testing.T, h *wal.RepoHandle)
		wantSum string
	}{
		{
			name: "canonical pair yields bare checksum",
			setup: func(t *testing.T, h *wal.RepoHandle) {
				writePair(t, h.Repo().PackDir(), "pack-"+hexA)
			},
			wantSum: hexA,
		},
		{
			name: "non-canonical base keeps its shape minus suffix",
			setup: func(t *testing.T, h *wal.RepoHandle) {
				writePair(t, h.Repo().PackDir(), "gen-"+hexA)
			},
			wantSum: "gen-" + hexA,
		},
		{
			name: "stray idx without pack is never claimed",
			setup: func(t *testing.T, h *wal.RepoHandle) {
				if err := os.WriteFile(filepath.Join(h.Repo().PackDir(), "pack-"+hexA+".idx"), []byte("junk"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantSum: "",
		},
		{
			name: "empty checksum is never claimed",
			setup: func(t *testing.T, h *wal.RepoHandle) {
				writePair(t, h.Repo().PackDir(), "pack-")
			},
			wantSum: "",
		},
		{
			name: "materialized legacy pack is not re-published",
			setup: func(t *testing.T, h *wal.RepoHandle) {
				dir := seedLegacy(t, h, hexA)
				// The read path materializes legacy entries under the
				// stored (prefixed) checksum: pack-pack-<hex>.*.
				writePair(t, dir, "pack-pack-"+hexA)
			},
			wantSum: "",
		},
		{
			name: "legacy manifest still accepts a fresh bare pack",
			setup: func(t *testing.T, h *wal.RepoHandle) {
				seedLegacy(t, h, hexA)
				writePair(t, h.Repo().PackDir(), "pack-"+hexB)
			},
			wantSum: hexB,
		},
		{
			name: "known bare pack is skipped",
			setup: func(t *testing.T, h *wal.RepoHandle) {
				dir := h.Repo().PackDir()
				writePair(t, dir, "pack-"+hexA)
				p, err := os.ReadFile(filepath.Join(dir, "pack-"+hexA+".pack"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.PublishCompact(ctx, &wal.PreparedPack{
					Checksum: hexA,
					PackPath: filepath.Join(dir, "pack-"+hexA+".pack"),
					IdxPath:  filepath.Join(dir, "pack-"+hexA+".idx"),
					PackSize: uint64(len(p)), IdxSize: 9,
				}, nil, nil); err != nil {
					t.Fatal(err)
				}
			},
			wantSum: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := walTestCfg(t)
			reg := wal.NewRegistry(ctx, store.NewMemory(), cfg)
			defer reg.Close()
			e := NewWalEngine(reg, cfg)
			h, err := reg.Create(ctx, "o/shapes", git.Sha1)
			if err != nil {
				t.Fatal(err)
			}
			tc.setup(t, h)
			p := e.newLocalPack(h)
			if tc.wantSum == "" {
				if p != nil {
					t.Fatalf("want nil, got %+v", p)
				}
				return
			}
			if p == nil {
				t.Fatal("want a candidate, got nil")
			}
			if p.Checksum != tc.wantSum {
				t.Fatalf("checksum = %q want %q", p.Checksum, tc.wantSum)
			}
			if strings.Contains(p.Checksum, "pack-") && !strings.HasPrefix(tc.wantSum, "gen-") {
				t.Fatalf("checksum %q keeps the pack- infix", p.Checksum)
			}
			if _, err := os.Stat(p.PackPath); err != nil {
				t.Fatalf("PackPath %q must exist: %v", p.PackPath, err)
			}
			if _, err := os.Stat(p.IdxPath); err != nil {
				t.Fatalf("IdxPath %q must exist: %v", p.IdxPath, err)
			}
		})
	}
}
