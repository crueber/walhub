// HasChecks existence probe (issue #505): the summary Checks-tab
// visibility flag, read index-first behind the Env hook. Table-driven
// over the absent/present/corrupt matrix, plus the law-4/law-6 budget
// pin (one exact-key GET, zero LIST — non-consumers pay nothing and the
// per-page-view summary never scans).
package checks

import (
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

func TestHasChecksAbsent(t *testing.T) {
	e := newTestEnv()
	counting := &countingStore{inner: e.store}
	e.svc.Store = counting
	has, ver, ok, err := e.svc.HasChecks(ctx(), "o", "r")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if ok || has || ver != 0 {
		t.Fatalf("absent = (%v, %d, %v), want (false, 0, false)", has, ver, ok)
	}
	if gets, _, lists := counting.snapshot(); gets != 1 || lists != 0 {
		t.Fatalf("budget: gets=%d lists=%d, want 1 GET and 0 LIST", gets, lists)
	}
}

func TestHasChecksAfterReports(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(11)
	e.knowSHA(sha)
	e.mustReport(t, sha, "ci/build", StatePending)
	has, ver, ok, err := e.svc.HasChecks(ctx(), "o", "r")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok || !has || ver != 1 {
		t.Fatalf("first report = (%v, %d, %v), want (true, 1, true)", has, ver, ok)
	}
	// A second report bumps the index version (the ~k ETag suffix
	// input) while the flag stays set.
	e.mustReport(t, sha, "ci/test", StateSuccess)
	has, ver, ok, err = e.svc.HasChecks(ctx(), "o", "r")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok || !has || ver != 2 {
		t.Fatalf("second report = (%v, %d, %v), want (true, 2, true)", has, ver, ok)
	}
}

func TestHasChecksEmptyIndex(t *testing.T) {
	e := newTestEnv()
	// A present-but-empty index (compacted past every sha) reads
	// present with the flag down — the ETag still covers the version.
	raw := encodeIndex(&IndexDoc{SHAs: []IndexSHA{}, Version: 4})
	if _, err := store.PutBytes(ctx(), e.store, IndexKey("o", "r"), raw, store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	has, ver, ok, err := e.svc.HasChecks(ctx(), "o", "r")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok || has || ver != 4 {
		t.Fatalf("empty index = (%v, %d, %v), want (false, 4, true)", has, ver, ok)
	}
}

func TestHasChecksCorrupt(t *testing.T) {
	e := newTestEnv()
	if _, err := store.PutBytes(ctx(), e.store, IndexKey("o", "r"), []byte("{bad"), store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, ok, err := e.svc.HasChecks(ctx(), "o", "r"); err == nil || ok {
		t.Fatalf("corrupt = (ok=%v, err=%v), want ok=false with an error (composition fails open)", ok, err)
	}
}
