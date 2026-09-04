package releases

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
)

func upload(t *testing.T, x *harness, tag, name string, body []byte) *AssetEntry {
	t.Helper()
	e, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), tag, name,
		bytes.NewReader(body), int64(len(body)), shaOf(body), "application/octet-stream")
	if err != nil {
		t.Fatalf("upload %s/%s: %v", tag, name, err)
	}
	return e
}

func TestAssetUploadDownloadRoundTrip(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})

	body := []byte("fake-binary-payload-0123456789")
	e := upload(t, x, "v1", "walhub-linux-amd64", body)
	if e.Size != int64(len(body)) || e.SHA256 != shaOf(body) || e.Uploader != "jane" {
		t.Fatalf("entry: %+v", e)
	}
	// Header carries the entry; bytes are byte-identical.
	rel, _, _ := x.svc.GetRelease(ctx(), "o", "r", writer(), "v1")
	if len(rel.Assets) != 1 || rel.Assets[0].SHA256 != shaOf(body) {
		t.Fatalf("header assets: %+v", rel.Assets)
	}
	match, err := x.svc.assetBytesMatch(ctx(), AssetKey("o", "r", "v1", "walhub-linux-amd64"), shaOf(body))
	if err != nil || !match {
		t.Fatalf("stored bytes: %v %v", match, err)
	}
}

func TestAssetUploadGates(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	body := []byte("data")
	sha := shaOf(body)

	// Unknown release → 404.
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "nope", "f", bytes.NewReader(body), 4, sha, ""); !isErr(err, ErrNotFound) {
		t.Fatalf("unknown release: %v", err)
	}
	// Missing Content-Length → 400.
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "f", bytes.NewReader(body), -1, sha, ""); !isErr(err, ErrInvalid) {
		t.Fatalf("missing length: %v", err)
	}
	// Over cap by declared length → 413 before reading.
	x.svc.MaxAssetBytes = 3
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "f", bytes.NewReader(body), 4, sha, ""); !isErr(err, ErrTooLarge) {
		t.Fatalf("declared over cap: %v", err)
	}
	// Lying Content-Length (short body) → 400 truncated.
	x.svc.MaxAssetBytes = 0
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "f", bytes.NewReader(body[:2]), 4, sha, ""); !isErr(err, ErrInvalid) {
		t.Fatalf("truncated: %v", err)
	}
	// SHA mismatch → 400, nothing stored.
	other := shaOf([]byte("other"))
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "f", bytes.NewReader(body), 4, other, ""); !isErr(err, ErrInvalid) {
		t.Fatalf("sha mismatch: %v", err)
	}
	if _, _, err := x.svc.getJSON(ctx(), AssetKey("o", "r", "v1", "f")); err != nil {
		t.Fatalf("getJSON: %v", err)
	} else {
		// absent expected — getJSON returns nil body, verified below.
	}
	raw, _, _ := x.svc.getJSON(ctx(), AssetKey("o", "r", "v1", "f"))
	if raw != nil {
		t.Fatal("mismatched upload stored bytes")
	}
	// Empty name / bad sha / bad content type → 400.
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "", bytes.NewReader(body), 4, sha, ""); !isErr(err, ErrInvalid) {
		t.Fatalf("empty name: %v", err)
	}
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "f", bytes.NewReader(body), 4, "zz", ""); !isErr(err, ErrInvalid) {
		t.Fatalf("bad sha: %v", err)
	}
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "f", bytes.NewReader(body), 4, sha, strings.Repeat("x", 201)); !isErr(err, ErrInvalid) {
		t.Fatalf("bad ct: %v", err)
	}
}

func TestAssetIdempotentAndClash(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})

	a := []byte("aaa")
	b := []byte("bbb")
	e1 := upload(t, x, "v1", "f", a)
	// Same bytes again → idempotent success (same entry).
	e2 := upload(t, x, "v1", "f", a)
	if e2.SHA256 != e1.SHA256 || e2.Size != e1.Size {
		t.Fatalf("idempotent: %+v vs %+v", e1, e2)
	}
	rel, _, _ := x.svc.GetRelease(ctx(), "o", "r", writer(), "v1")
	if len(rel.Assets) != 1 {
		t.Fatalf("dup entry: %+v", rel.Assets)
	}
	// Same name, different bytes → 409.
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "f",
		bytes.NewReader(b), int64(len(b)), shaOf(b), ""); !isErr(err, ErrConflict) {
		t.Fatalf("clash: %v", err)
	}
}

func TestAssetConcurrentUploadsConverge(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})

	// 16 racers, 4 distinct payloads: every upload either wins its name or
	// 409s/adopts; the header ends with exactly the winners, one entry per
	// name, each pointing at matching bytes (bytes-first-then-header:
	// no dangling entries possible).
	payloads := [][]byte{[]byte("p0"), []byte("p1-payload"), []byte("p2!"), []byte("p3-longer-payload")}
	var wg sync.WaitGroup
	errs := make([]error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := payloads[i%len(payloads)]
			name := "asset"
			if i%2 == 1 {
				name = "asset-b"
			}
			_, errs[i] = x.svc.UploadAsset(context.Background(), "o", "r", writer(), "v1", name,
				bytes.NewReader(body), int64(len(body)), shaOf(body), "")
		}(i)
	}
	wg.Wait()
	var ok409 int
	for _, err := range errs {
		if err == nil {
			continue
		}
		if !isErr(err, ErrConflict) {
			t.Fatalf("unexpected upload error: %v", err)
		}
		ok409++
	}
	rel, _, err := x.svc.GetRelease(ctx(), "o", "r", writer(), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rel.Assets) != 2 {
		t.Fatalf("assets: %+v", rel.Assets)
	}
	seen := map[string]bool{}
	for _, a := range rel.Assets {
		if seen[a.Name] {
			t.Fatalf("dup entry: %+v", rel.Assets)
		}
		seen[a.Name] = true
		match, merr := x.svc.assetBytesMatch(ctx(), AssetKey("o", "r", "v1", a.Name), a.SHA256)
		if merr != nil || !match {
			t.Fatalf("dangling entry %q: %v %v", a.Name, match, merr)
		}
	}
}

func TestDeleteAsset(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	mustPut(t, x, writer(), "v1", ReleaseInput{})
	body := []byte("gone")
	upload(t, x, "v1", "f", body)

	removed, err := x.svc.DeleteAsset(ctx(), "o", "r", writer(), "v1", "f")
	if err != nil || removed.SHA256 != shaOf(body) {
		t.Fatalf("delete: %+v %v", removed, err)
	}
	if _, err := x.svc.DeleteAsset(ctx(), "o", "r", writer(), "v1", "f"); !isErr(err, ErrNotFound) {
		t.Fatalf("double delete: %v", err)
	}
	raw, _, _ := x.svc.getJSON(ctx(), AssetKey("o", "r", "v1", "f"))
	if raw != nil {
		t.Fatal("bytes survive delete")
	}
	if _, err := x.svc.DeleteAsset(ctx(), "o", "r", writer(), "nope", "f"); !isErr(err, ErrNotFound) {
		t.Fatalf("unknown release delete: %v", err)
	}
}
