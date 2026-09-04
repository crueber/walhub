package releases

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// ctxT anchors closure signatures.
type ctxT = context.Context

func decodeJSON(raw []byte, v any) error { return json.Unmarshal(raw, v) }

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func contextWithCancelValue() context.Context { return context.Background() }

// writeFakeGit installs an executable shell stub as the git binary.
func writeFakeGit(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// memStoreOf unwraps failStore layers to the backing memory store.
func memStoreOf(x *harness) store.ObjectStore {
	st := x.svc.Store
	for {
		fs, ok := st.(*failStore)
		if !ok {
			return st
		}
		st = fs.ObjectStore
	}
}

// delTags removes the fixture tags (empty-list probe).
func delTags(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "tag", "-d", "v1", "v2")
	cmd.Dir = dir
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tag -d: %v %s", err, out)
	}
}

// cover2 sweeps the remaining service/handler branches one by one (each
// block below names the branch it covers).

func TestCover2PutEdges(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	// Bad tag at the service layer.
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "", ReleaseInput{}, ""); !isErr(err, ErrInvalid) {
		t.Fatalf("empty tag: %v", err)
	}
	// If-Match "*" on absent → 404 (create-only token).
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v1", ReleaseInput{}, "*"); !isErr(err, ErrNotFound) {
		t.Fatalf("star absent: %v", err)
	}
	// Corrupt header on update → ErrCorrupt.
	mustStore(t, x, ReleaseKey("o", "r", "bad"), []byte("{oops"))
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "bad", ReleaseInput{}, ""); !isErr(err, ErrCorrupt) {
		t.Fatalf("corrupt update: %v", err)
	}
	// Body update + rename + prerelease flag on existing.
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	rel, created, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v1",
		ReleaseInput{Name: strptr("N"), Body: strptr("B"), Prerelease: boolptr(true)}, "")
	if err != nil || created || rel.Name != "N" || rel.Body != "B" || !rel.Prerelease {
		t.Fatalf("field update: %+v %v", rel, err)
	}
	// Re-draft clears published_at without fan-out.
	var notified int
	x.svc.Notify = func(_ ctxT, _ NotifyEvent) { notified++ }
	rel2, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v1", ReleaseInput{Draft: boolptr(true)}, "")
	if err != nil || !rel2.Draft || rel2.PublishedAt != nil || notified != 0 {
		t.Fatalf("re-draft: %+v %v %d", rel2, err, notified)
	}
	// If-Match "*" on absent → 404 (update-only token).
	recStar := putJSON(t, x, "ghost", map[string]any{"name": "G"},
		mergeHeaders(asWriter(), map[string]string{"If-Match": "*"}))
	if recStar.Code != 404 {
		t.Fatalf("star absent: %d", recStar.Code)
	}
	// Dirs outage at resolve (creation path — update would skip resolve).
	x.svc.Dirs = errDirs{}
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v9", ReleaseInput{}, ""); !isErr(err, ErrUnavailable) {
		t.Fatalf("dirs down: %v", err)
	}
}

func TestCover2ReadEdges(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	// GetRelease: anon / bad tag / store error.
	if _, _, err := x.svc.GetRelease(ctx(), "o", "r", auth.Anonymous(), "v1"); !isErr(err, ErrUnauthorized) {
		t.Fatalf("anon get: %v", err)
	}
	if _, _, err := x.svc.GetRelease(ctx(), "o", "r", writer(), ""); !isErr(err, ErrInvalid) {
		t.Fatalf("empty tag get: %v", err)
	}
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, getErr: errors.New("down")}
	if _, _, err := x.svc.GetRelease(ctx(), "o", "r", writer(), "v1"); err == nil {
		t.Fatal("getJSON error swallowed")
	}
	// ListReleases: anon / clamps / bad cursors / LIST error / skip / cap.
	x2 := newHarness(t)
	grantWrite(x2)
	x2.git.tags["v1"] = strings.Repeat("a", 40)
	x2.git.tags["v2"] = strings.Repeat("b", 40)
	mustPut(t, x2, writer(), "v1", ReleaseInput{})
	mustPut(t, x2, writer(), "v2", ReleaseInput{})
	if _, _, err := x2.svc.ListReleases(ctx(), "o", "r", auth.Anonymous(), 10, ""); !isErr(err, ErrUnauthorized) {
		t.Fatalf("anon list: %v", err)
	}
	if rels, _, err := x2.svc.ListReleases(ctx(), "o", "r", writer(), 0, ""); err != nil || len(rels) != 2 {
		t.Fatalf("n=0 default: %+v %v", rels, err)
	}
	if rels, _, err := x2.svc.ListReleases(ctx(), "o", "r", writer(), 500, ""); err != nil || len(rels) != 2 {
		t.Fatalf("n clamp: %+v %v", rels, err)
	}
	badCursors := []string{"2026-13-99T99:99:99Z|v1", "2026-09-04T12:00:00Z|"}
	for _, c := range badCursors {
		if _, _, err := x2.svc.ListReleases(ctx(), "o", "r", writer(), 10, c); !isErr(err, ErrInvalid) {
			t.Fatalf("cursor %q: %v", c, err)
		}
	}
	if ts, tg, ok := splitCursor("2026-09-04T12:00:00Z|v1|extra"); !ok || ts == "" || tg != "v1|extra" {
		t.Fatalf("pipe tag cursor: %q %q %v", ts, tg, ok)
	}
	x2.svc.Store = &failStore{ObjectStore: x2.svc.Store, listErr: errors.New("down")}
	if _, _, err := x2.svc.ListReleases(ctx(), "o", "r", writer(), 10, ""); err == nil {
		t.Fatal("LIST error swallowed")
	}
	// LatestRelease: anon / pointer error / scan error.
	x3 := newHarness(t)
	if _, _, err := x3.svc.LatestRelease(ctx(), "o", "r", auth.Anonymous(), false); !isErr(err, ErrUnauthorized) {
		t.Fatalf("anon latest: %v", err)
	}
	x3.svc.Store = &failStore{ObjectStore: x3.svc.Store, getErr: errors.New("down")}
	if _, _, err := x3.svc.LatestRelease(ctx(), "o", "r", writer(), false); err == nil {
		t.Fatal("pointer error swallowed")
	}
}

func TestCover2ScanEdges(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	x.git.tags["v2"] = strings.Repeat("b", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	mustPut(t, x, writer(), "v2", ReleaseInput{})
	upload(t, x, "v1", "tool", []byte("bytes"))
	// Filter hit: asset keys + latest.json never fetched as headers.
	got, err := x.svc.scanHeaders(ctx(), "o", "r", ListScanCap)
	if err != nil || len(got) != 2 {
		t.Fatalf("scan: %+v %v", got, err)
	}
	// Cap break.
	got2, err := x.svc.scanHeaders(ctx(), "o", "r", 1)
	if err != nil || len(got2) != 1 {
		t.Fatalf("cap: %+v %v", got2, err)
	}
	// Skip on unreadable header.
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, getErr: errors.New("flaky"), getErrSubstr: "v2.json"}
	rels, _, err := x.svc.ListReleases(ctx(), "o", "r", writer(), 10, "")
	if err != nil || len(rels) != 1 || rels[0].rel.Tag != "v1" {
		t.Fatalf("skip: %+v %v", rels, err)
	}
}

func TestCover2LatestScanError(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	// Pointer absent + LIST down → scan error surfaces.
	if err := x.svc.Store.Delete(ctx(), LatestKey("o", "r"), ""); err != nil {
		t.Fatal(err)
	}
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, listErr: errors.New("down")}
	if _, _, err := x.svc.LatestRelease(ctx(), "o", "r", writer(), false); err == nil {
		t.Fatal("scan error swallowed")
	}
}

func TestCover2DeleteEdges(t *testing.T) {
	newDeleteHarness := func(t *testing.T) *harness {
		t.Helper()
		x := newHarness(t)
		grantMaintain(x)
		x.git.tags["v1"] = strings.Repeat("a", 40)
		mustPut(t, x, writer(), "v1", ReleaseInput{})
		return x
	}
	// Bad tag / store read error.
	x := newDeleteHarness(t)
	if err := x.svc.DeleteRelease(ctx(), "o", "r", writer(), ""); !isErr(err, ErrInvalid) {
		t.Fatalf("empty tag: %v", err)
	}
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, getErr: errors.New("down")}
	if err := x.svc.DeleteRelease(ctx(), "o", "r", writer(), "v1"); err == nil {
		t.Fatal("read error swallowed")
	}
	// Header delete error (fresh fixture: header present).
	x = newDeleteHarness(t)
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, deleteErr: errors.New("down")}
	if err := x.svc.DeleteRelease(ctx(), "o", "r", writer(), "v1"); err == nil {
		t.Fatal("delete error swallowed")
	}
	// Release with assets: bytes removed with the header.
	x = newDeleteHarness(t)
	upload(t, x, "v1", "tool", []byte("bytes"))
	if err := x.svc.DeleteRelease(ctx(), "o", "r", writer(), "v1"); err != nil {
		t.Fatal(err)
	}
	if raw, _, _ := x.svc.getJSON(ctx(), AssetKey("o", "r", "v1", "tool")); raw != nil {
		t.Fatal("asset bytes survive release delete")
	}
	// Asset delete error (header delete passes).
	x = newDeleteHarness(t)
	upload(t, x, "v1", "tool", []byte("bytes"))
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, deleteErr: errors.New("down"), deleteErrSub: "/assets/"}
	if err := x.svc.DeleteRelease(ctx(), "o", "r", writer(), "v1"); err == nil {
		t.Fatal("asset delete error swallowed")
	}
	// Scan error during repair (assets LIST passes, scan LIST fails).
	x = newDeleteHarness(t)
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, listErr: errors.New("down"), listErrPrefix: ReleasesPrefix("o", "r")}
	if err := x.svc.DeleteRelease(ctx(), "o", "r", writer(), "v1"); err == nil {
		t.Fatal("repair scan error swallowed")
	}
}

func TestCover2UploadEdges(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	body := []byte("data")
	sha := shaOf(body)
	// Anonymous upload → 401.
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", auth.Anonymous(), "v1", "f",
		bytes.NewReader(body), 4, sha, ""); !isErr(err, ErrUnauthorized) {
		t.Fatalf("anon upload: %v", err)
	}
	// Header read error.
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, getErr: errors.New("down")}
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "f",
		bytes.NewReader(body), 4, sha, ""); err == nil {
		t.Fatal("header read error swallowed")
	}
}

func TestCover2SpoolEdges(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	body := []byte("data")
	sha := shaOf(body)
	// Empty spool dir → os.TempDir fallback (upload still succeeds).
	x.svc.SpoolDir = ""
	e, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "tmpdir", bytes.NewReader(body), 4, sha, "")
	if err != nil || e.Name != "tmpdir" {
		t.Fatalf("tempdir spool: %+v %v", e, err)
	}
	// Unwritable spool dir → CreateTemp error (skipped for root: perms moot).
	if os.Geteuid() != 0 {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		x.svc.SpoolDir = dir
		if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "nope",
			bytes.NewReader(body), 4, sha, ""); err == nil {
			t.Fatal("unwritable spool accepted")
		}
	}
}

func TestCover2ClashVerify(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	// Orphan bytes, different digest → 409 on verify-mismatch.
	if _, err := store.PutBytes(ctx(), x.svc.Store, AssetKey("o", "r", "v1", "clash"),
		[]byte("stored"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	other := []byte("upld!!")
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "clash",
		bytes.NewReader(other), int64(len(other)), shaOf(other), ""); !isErr(err, ErrConflict) {
		t.Fatalf("verify mismatch: %v", err)
	}
	// Resolver direct: store read error.
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, getErr: errors.New("down")}
	if _, _, err := x.svc.resolveAssetClash(ctx(), "o", "r", "v1", &AssetEntry{Name: "f"}); err == nil {
		t.Fatal("clash read error swallowed")
	}
	// assetBytesMatch direct: store read error + body read error.
	if _, err := x.svc.assetBytesMatch(ctx(), "k", shaOf([]byte("z"))); err == nil {
		t.Fatal("match read error swallowed")
	}
	x.svc.Store = &failStore{ObjectStore: memStoreOf(x), bodyErr: errors.New("mid-stream")}
	if _, err := x.svc.assetBytesMatch(ctx(), AssetKey("o", "r", "v1", "f"), "x"); err == nil {
		t.Fatal("match body error swallowed")
	}
}

func TestCover2DeleteAssetEdges(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	if _, err := x.svc.DeleteAsset(ctx(), "o", "r", auth.Anonymous(), "v1", "f"); !isErr(err, ErrUnauthorized) {
		t.Fatalf("anon: %v", err)
	}
	if _, err := x.svc.DeleteAsset(ctx(), "o", "r", writer(), "", "f"); !isErr(err, ErrInvalid) {
		t.Fatalf("empty tag: %v", err)
	}
	if _, err := x.svc.DeleteAsset(ctx(), "o", "r", writer(), "v1", "a/b"); !isErr(err, ErrInvalid) {
		t.Fatalf("slash name: %v", err)
	}
	x.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	upload(t, x, "v1", "f", []byte("data"))
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, deleteErr: errors.New("down"), deleteErrSub: "/assets/"}
	if _, err := x.svc.DeleteAsset(ctx(), "o", "r", writer(), "v1", "f"); err == nil {
		t.Fatal("bytes delete error swallowed")
	}
}

func TestCover2AutodraftEdges(t *testing.T) {
	// Empty tag.
	x := newHarness(t)
	if _, err := x.svc.Autodraft(ctx(), "o", "r", writer(), "", ""); !isErr(err, ErrInvalid) {
		t.Fatalf("empty tag: %v", err)
	}
	// Index read error (tag resolves; store fails).
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	x.svc.Store = &failStore{ObjectStore: x.svc.Store, getErr: errors.New("down")}
	if _, err := x.svc.Autodraft(ctx(), "o", "r", writer(), "v1", ""); err == nil {
		t.Fatal("index error swallowed")
	}
	// Corrupt index.
	x2 := newHarness(t)
	grantWrite(x2)
	x2.git.tags["v1"] = strings.Repeat("a", 40)
	mustStore(t, x2, "repos/o/r/issues/index.json", []byte("{oops"))
	if _, err := x2.svc.Autodraft(ctx(), "o", "r", writer(), "v1", ""); !isErr(err, ErrCorrupt) {
		t.Fatalf("corrupt index: %v", err)
	}
	// Card without sidecar + corrupt sidecar + empty index.
	x3 := newHarness(t)
	grantWrite(x3)
	x3.git.tags["v1"] = strings.Repeat("a", 40)
	seedIndex(t, x3, prCard(1, "Solo", "amy"), prCard(2, "Bad", "bob"))
	mustStore(t, x3, fmt.Sprintf("repos/o/r/pulls/%06x/pr.json", 2), []byte("{oops"))
	ad, err := x3.svc.Autodraft(ctx(), "o", "r", writer(), "v1", "")
	if err != nil || len(ad.PRs) != 0 {
		t.Fatalf("skips: %+v %v", ad, err)
	}
	x4 := newHarness(t)
	grantWrite(x4)
	x4.git.tags["v1"] = strings.Repeat("a", 40)
	seedIndex(t, x4)
	ad4, err := x4.svc.Autodraft(ctx(), "o", "r", writer(), "v1", "")
	if err != nil || len(ad4.PRs) != 0 || ad4.Body != "" {
		t.Fatalf("empty index: %+v %v", ad4, err)
	}
	// Ancestry skip (!inTag) via an unmerged-elsewhere sha.
	x5 := newHarness(t)
	grantWrite(x5)
	x5.git.tags["v1"] = strings.Repeat("a", 40)
	seedIndex(t, x5, prCard(7, "Elsewhere", "zed"))
	seedPR(t, x5, 7, "Elsewhere", "zed", true, strings.Repeat("e", 40), "2026-09-02T10:00:00Z")
	ad5, err := x5.svc.Autodraft(ctx(), "o", "r", writer(), "v1", "")
	if err != nil || len(ad5.PRs) != 0 {
		t.Fatalf("not-in-tag: %+v %v", ad5, err)
	}
}

func TestCover2SinceEdges(t *testing.T) {
	// Dirs outage inside defaultSince (2nd Dir call).
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	flaky := &flakyDirs{dir: t.TempDir(), failOn: 2}
	x.svc.Dirs = flaky
	ad, err := x.svc.Autodraft(ctx(), "o", "r", writer(), "v1", "")
	if err != nil || ad.Since != "" || len(ad.PRs) != 0 {
		t.Fatalf("dirs outage: %+v %v", ad, err)
	}
	// ListTags outage → "" (tag list path, no latest).
	x2 := newHarness(t)
	grantWrite(x2)
	x2.git.tags["v1"] = strings.Repeat("a", 40)
	x2.git.listErr = errors.New("down")
	ad2, err := x2.svc.Autodraft(ctx(), "o", "r", writer(), "v1", "")
	if err != nil || ad2.Since != "" {
		t.Fatalf("list outage: %+v %v", ad2, err)
	}
	// Previous tag unresolvable → label kept, sha vacuous.
	x3 := newHarness(t)
	grantWrite(x3)
	x3.git.tags["v2"] = strings.Repeat("2", 40)
	x3.git.tagList = []string{"v2", "v1"}
	ad3, err := x3.svc.Autodraft(ctx(), "o", "r", writer(), "v2", "")
	if err != nil || ad3.Since != "v1" {
		t.Fatalf("unresolvable prev: %+v %v", ad3, err)
	}
	// Dirs outage inside resolveAnyRef (explicit since, 2nd Dir call).
	x4 := newHarness(t)
	grantWrite(x4)
	x4.git.tags["v1"] = strings.Repeat("a", 40)
	x4.git.tags["v0"] = strings.Repeat("0", 40)
	x4.svc.Dirs = &flakyDirs{dir: t.TempDir(), failOn: 2}
	if _, err := x4.svc.Autodraft(ctx(), "o", "r", writer(), "v1", "v0"); !isErr(err, ErrUnavailable) {
		t.Fatalf("since dirs: %v", err)
	}
	// Since-probe failure → 503 (first probe ok, second fails).
	x5 := newHarness(t)
	grantWrite(x5)
	x5.git.tags["v2"] = strings.Repeat("2", 40)
	x5.git.tags["v1"] = strings.Repeat("1", 40)
	m1 := strings.Repeat("a", 40)
	x5.git.ancestors[m1+"\x00"+strings.Repeat("2", 40)] = true
	x5.git.ancestorErr = map[string]error{m1 + "\x00" + strings.Repeat("1", 40): errors.New("git down")}
	seedIndex(t, x5, prCard(1, "First", "amy"))
	seedPR(t, x5, 1, "First", "amy", true, m1, "2026-09-01T10:00:00Z")
	if _, err := x5.svc.Autodraft(ctx(), "o", "r", writer(), "v2", "v1"); !isErr(err, ErrUnavailable) {
		t.Fatalf("since probe: %v", err)
	}
}

func TestCover2NewerThanFallback(t *testing.T) {
	// Valid-vs-corrupt falls back to lexical compare (corrupt sorts oldest).
	if newerThan("2026-09-04T13:00:00Z", "garbage") {
		t.Fatal("lexical fallthrough")
	}
	if !newerThan("2026-09-04T13:00:00Z", "!") {
		t.Fatal("lexical fallthrough true")
	}
}

func TestCover2AutodraftDirsLate(t *testing.T) {
	// defaultSince succeeds without extra Dir calls (tag list covers the
	// default); the Autodraft body's own Dir is the 3rd call and fails.
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	x.git.tagList = []string{"v1"}
	seedIndex(t, x, prCard(1, "First", "amy"))
	seedPR(t, x, 1, "First", "amy", true, strings.Repeat("a", 40), "2026-09-01T10:00:00Z")
	x.svc.Dirs = &flakyDirs{dir: t.TempDir(), failOn: 3}
	if _, err := x.svc.Autodraft(ctx(), "o", "r", writer(), "v1", ""); !isErr(err, ErrUnavailable) {
		t.Fatalf("late dirs: %v", err)
	}
}

func TestCover2ModelDirect(t *testing.T) {
	if _, err := normalizeSHA256(strings.Repeat("z", 64)); !isErr(err, ErrInvalid) {
		t.Fatalf("non-hex sha: %v", err)
	}
	long := strings.Repeat("n", MaxNameLen+1)
	if _, _, err := validateReleaseInput(ReleaseInput{Name: &long}); !isErr(err, ErrInvalid) {
		t.Fatalf("long name: %v", err)
	}
	blank := "   "
	if _, _, err := validateReleaseInput(ReleaseInput{Name: &blank}); !isErr(err, ErrInvalid) {
		t.Fatalf("blank name: %v", err)
	}
	r, err := parseRelease([]byte(`{"tag":"x"}`))
	if err != nil || r.Assets == nil {
		t.Fatalf("nil assets: %+v %v", r, err)
	}
	raw := encodeRelease(&Release{Tag: "x"})
	if !strings.Contains(string(raw), `"assets":[]`) {
		t.Fatalf("encode nil: %s", raw)
	}
	if _, err := parseRelease([]byte("{oops")); !isErr(err, ErrCorrupt) {
		t.Fatalf("parse corrupt: %v", err)
	}
	if _, err := parseLatest([]byte("{oops")); !isErr(err, ErrCorrupt) {
		t.Fatalf("latest corrupt: %v", err)
	}
	// Nil clock.
	s := &Service{Store: store.NewMemory()}
	if s.nowUTC().IsZero() || s.maxAssetBytes() != DefaultMaxAssetBytes {
		t.Fatal("nil service defaults")
	}
}

func TestCover2HandleFalse(t *testing.T) {
	x := newHarness(t)
	// Top-level api path is never a releases route.
	req := httptest.NewRequest("GET", "/api/v1/me", nil)
	if x.handler.Handle(httptest.NewRecorder(), req) {
		t.Fatal("top-level claimed")
	}
	// Unparseable repo id is not claimed.
	req2 := httptest.NewRequest("GET", "/bad!owner/r/api/releases", nil)
	if x.handler.Handle(httptest.NewRecorder(), req2) {
		t.Fatal("bad repo claimed")
	}
	// writeCached encode failure.
	rec := httptest.NewRecorder()
	writeCached(rec, httptest.NewRequest("GET", "/", nil), ccSWR, "e", 200, func() {})
	if rec.Code != 500 {
		t.Fatalf("cached encode: %d", rec.Code)
	}
	// Null (non-object) JSON body.
	rec2 := httptest.NewRecorder()
	if decodeStrict(rec2, httptest.NewRequest("PUT", "/", strings.NewReader("null")), 100, map[string]bool{}, &map[string]any{}) {
		t.Fatal("null body accepted")
	}
}

func TestCover2LaneAuthError(t *testing.T) {
	x := newHarness(t)
	x.handler.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Anonymous(), &auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}
	}
	rec := do(t, x, "PUT", "/o/r/api/releases/v1", []byte(`{}`), nil)
	if rec.Code != 503 {
		t.Fatalf("lane auth: %d", rec.Code)
	}
}

func TestCover2WireWithAssets(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	putJSON(t, x, "v1", map[string]any{"name": "R"}, asWriter())
	upload(t, x, "v1", "tool", []byte("payload"))
	rec := do(t, x, "GET", "/o/r/api/releases/v1", nil, asReader("bob"))
	var wire map[string]any
	if err := decodeJSON(rec.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	assets, ok := wire["assets"].([]any)
	if !ok || len(assets) != 1 {
		t.Fatalf("wire assets: %v", wire["assets"])
	}
	first, _ := assets[0].(map[string]any)
	if first["browser_download_url"] != "/o/r/releases/v1/assets/tool" {
		t.Fatalf("download url: %v", first)
	}
	// Autodraft handler error path.
	rec2 := do(t, x, "GET", "/o/r/api/releases/autodraft?tag=nope", nil, asReader("bob"))
	if rec2.Code != 404 {
		t.Fatalf("autodraft unknown: %d", rec2.Code)
	}
}

func TestCover2ServeAssetEdges(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	putJSON(t, x, "v1", map[string]any{}, asWriter())
	upload(t, x, "v1", "tool", []byte("0123456789abcdef"))
	id := git.RepoId{Owner: "o", Name: "r"}
	// Header read error.
	mem := x.svc.Store
	x.svc.Store = &failStore{ObjectStore: mem, getErr: errors.New("down")}
	rec := httptest.NewRecorder()
	x.handler.HandleRepo(rec, authedGet("/o/r/releases/v1/assets/tool"), id, []string{"releases", "v1", "assets", "tool"})
	if rec.Code != 500 {
		t.Fatalf("header read: %d", rec.Code)
	}
	x.svc.Store = &failStore{ObjectStore: mem}
	// Header gone → 404.
	if err := mem.Delete(ctx(), ReleaseKey("o", "r", "v1"), ""); err != nil {
		t.Fatal(err)
	}
	rec2 := httptest.NewRecorder()
	x.handler.HandleRepo(rec2, authedGet("/o/r/releases/v1/assets/tool"), id, []string{"releases", "v1", "assets", "tool"})
	if rec2.Code != 404 {
		t.Fatalf("header gone: %d", rec2.Code)
	}
	// Backend Head 404-error (S3/GCS shape) → 404.
	x2 := newHarness(t)
	grantWrite(x2)
	x2.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x2, writer(), "v1", ReleaseInput{})
	upload(t, x2, "v1", "tool", []byte("data"))
	x2.svc.Store = &failStore{ObjectStore: x2.svc.Store, headErr: store.NewNotFound("k")}
	rec3 := httptest.NewRecorder()
	x2.handler.HandleRepo(rec3, authedGet("/o/r/releases/v1/assets/tool"), id, []string{"releases", "v1", "assets", "tool"})
	if rec3.Code != 404 {
		t.Fatalf("head 404: %d", rec3.Code)
	}
	x2.svc.Store = store.ObjectStore(memStoreOf(x2))
	// If-Range mismatch → full 200 despite Range.
	rec4 := httptest.NewRecorder()
	req4 := authedGet("/o/r/releases/v1/assets/tool")
	req4.Header.Set("Range", "bytes=0-3")
	req4.Header.Set("If-Range", `"stale"`)
	x2.handler.HandleRepo(rec4, req4, id, []string{"releases", "v1", "assets", "tool"})
	if rec4.Code != 200 || rec4.Body.String() != "data" {
		t.Fatalf("if-range: %d %q", rec4.Code, rec4.Body.String())
	}
	// Stream Get error + NotModified bodies.
	x2.svc.Store = &failStore{ObjectStore: x2.svc.Store, getErr: errors.New("down"), getErrSubstr: "/assets/"}
	rec5 := httptest.NewRecorder()
	x2.handler.HandleRepo(rec5, authedGet("/o/r/releases/v1/assets/tool"), id, []string{"releases", "v1", "assets", "tool"})
	if rec5.Code != 200 {
		t.Fatalf("stream head: %d", rec5.Code)
	}
	x2.svc.Store = &failStore{ObjectStore: memStoreOf(x2), notMod: true, notModSubstr: "/assets/"}
	rec6 := httptest.NewRecorder()
	x2.handler.HandleRepo(rec6, authedGet("/o/r/releases/v1/assets/tool"), id, []string{"releases", "v1", "assets", "tool"})
	if rec6.Code != 200 {
		t.Fatalf("not-modified: %d", rec6.Code)
	}
}

func TestCover2RangeExtra(t *testing.T) {
	for _, spec := range []string{"bytes=-", "bytes= - 3 "} {
		if _, _, ok := parseRange(spec, 16); spec == "bytes=-" && ok {
			t.Fatalf("%q parsed", spec)
		}
	}
	if s, e, ok := parseRange("bytes=-99", 16); !ok || s != 0 || e != 15 {
		t.Fatalf("suffix clamp: %d %d %v", s, e, ok)
	}
}

func TestCover2GitEdges(t *testing.T) {
	// Pool cancel without running.
	p := newGitPool(1)
	p.sem <- struct{}{}
	cctx, cancel := contextWithCancel()
	cancel()
	if err := p.run(cctx, func() error { return nil }); err == nil {
		t.Fatal("canceled pool ran")
	}
	<-p.sem
	// Default binary.
	if g := NewSubprocessGit(""); g.Binary != "git" {
		t.Fatalf("default: %q", g.Binary)
	}
	// Bounded stderr caps at 8 KiB.
	var b boundedStderr
	if n, _ := b.Write(bytes.Repeat([]byte("x"), 9000)); n != 8192 || b.buf.Len() != 8192 {
		t.Fatalf("cap: %d %d", n, b.buf.Len())
	}
	if n, _ := b.Write([]byte("y")); n != 1 || b.buf.Len() != 8192 {
		t.Fatalf("full: %d %d", n, b.buf.Len())
	}
	// Nil pool still runs.
	dir := gitTestRepo(t)
	g := &SubprocessGit{Binary: "git", Timeout: time.Second * 30}
	sha, err := g.ResolveRef(contextWithCancelValue(), dir, "refs/tags/v1")
	if err != nil || len(sha) != 40 {
		t.Fatalf("nil pool: %q %v", sha, err)
	}
	// Garbage-printing git → unknown revision (validateSHA gate).
	script := writeFakeGit(t, "#!/bin/sh\necho not-a-sha\n")
	gb := NewSubprocessGit(script)
	if _, err := gb.ResolveRef(contextWithCancelValue(), dir, "refs/tags/v1"); !isErr(err, ErrNotFound) {
		t.Fatalf("garbage sha: %v", err)
	}
	// Exit-3 git → 503 with the exit text (covers Error() via %v).
	script3 := writeFakeGit(t, "#!/bin/sh\necho oops >&2\nexit 3\n")
	g3 := NewSubprocessGit(script3)
	_, err = g3.IsAncestor(contextWithCancelValue(), dir, "a", "b")
	if !isErr(err, ErrUnavailable) {
		t.Fatalf("exit 3: %v", err)
	}
	if !strings.Contains(err.Error(), "oops") {
		t.Fatalf("exit text: %v", err)
	}
	// ListTags through the same failing runner → exit-error wrap.
	if _, err := g3.ListTags(contextWithCancelValue(), dir); !isErr(err, ErrUnavailable) {
		t.Fatalf("tags exit 3: %v", err)
	}
	// Error() renders directly (same package).
	ge := &gitExitError{argv: []string{"git", "x"}, err: errors.New("boom"), errText: "tail"}
	if s := ge.Error(); !strings.Contains(s, "boom") || !strings.Contains(s, "tail") {
		t.Fatalf("exit error: %q", s)
	}
	// Missing binary → 503 (backend, never a verdict).
	gx := NewSubprocessGit("definitely-not-git-xyz")
	if _, err := gx.ResolveRef(contextWithCancelValue(), dir, "refs/tags/v1"); !isErr(err, ErrUnavailable) {
		t.Fatalf("no binary resolve: %v", err)
	}
	if _, err := gx.IsAncestor(contextWithCancelValue(), dir, "a", "b"); !isErr(err, ErrUnavailable) {
		t.Fatalf("no binary ancestor: %v", err)
	}
	if _, err := gx.ListTags(contextWithCancelValue(), dir); !isErr(err, ErrUnavailable) {
		t.Fatalf("no binary tags: %v", err)
	}
	// ListTags on a bad dir → 503.
	g4 := NewSubprocessGit("git")
	if _, err := g4.ListTags(contextWithCancelValue(), "/nonexistent-dir-xyz"); !isErr(err, ErrUnavailable) {
		t.Fatalf("bad dir: %v", err)
	}
	// IsAncestor exit≠1 (garbage shas, exit 128) → 503.
	if _, err := g4.IsAncestor(contextWithCancelValue(), dir, "zz", "yy"); !isErr(err, ErrUnavailable) {
		t.Fatalf("exit 128: %v", err)
	}
	// Empty tag list.
	delTags(t, dir)
	tags, err := g4.ListTags(contextWithCancelValue(), dir)
	if err != nil || len(tags) != 0 {
		t.Fatalf("empty tags: %v %v", tags, err)
	}
}
