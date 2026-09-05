// resume_test.go — issue #79 regression: a failed import never wedges
// its target. Deterministic fault injection at the ingest, admin, and
// doc stages shows the same-source retry always completes (rollback to
// clean, or resume-to-complete when cleanup cannot land); the probe
// keeps 409 for genuinely foreign manifests only; concurrent
// same-target imports still elect one winner.
package repoimport

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// --- fault-injection doubles -----------------------------------------------------------

// failPuts fails the first n Puts to keys with the given suffix with a
// generic (non-412) store error, then passes through.
type failPuts struct {
	store.ObjectStore
	mu     sync.Mutex
	suffix string
	n      int
}

func (f *failPuts) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n > 0 && strings.HasSuffix(key, f.suffix) {
		f.n--
		return store.ObjectMeta{}, store.NewOther(key, errInjected)
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

// failDocUpdates fails the first n version-checked (PutUpdate) writes to
// import.json — the completion CAS.
type failDocUpdates struct {
	store.ObjectStore
	mu sync.Mutex
	n  int
}

func (f *failDocUpdates) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n > 0 && strings.HasSuffix(key, ImportKey) && opts.Mode == store.PutUpdate {
		f.n--
		return store.ObjectMeta{}, store.NewOther(key, errInjected)
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

// blockManifestDelete refuses manifest deletes (rollback cannot clean —
// the retry must resume off the surviving claim instead of wedging).
type blockManifestDelete struct {
	store.ObjectStore
}

func (f *blockManifestDelete) Delete(ctx context.Context, key string, ver store.Version) error {
	if strings.HasSuffix(key, store.Manifest) {
		return store.NewOther(key, errInjected)
	}
	return f.ObjectStore.Delete(ctx, key, ver)
}

// failCreateOnce reports one phantom 412 on the first import.json
// PutCreate (lost the CAS to a deleter that landed between), then
// passes through — covers the create-retry arm deterministically.
type failCreateOnce struct {
	store.ObjectStore
	mu   sync.Mutex
	done bool
}

func (f *failCreateOnce) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.done && strings.HasSuffix(key, ImportKey) && opts.Mode == store.PutCreate {
		f.done = true
		return store.ObjectMeta{}, store.NewPrecondition(key, "")
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

// failUpdateAlways 412s every import.json PutUpdate (a rival completer
// that never records our source — the superseded arm).
type failUpdateAlways struct {
	store.ObjectStore
}

func (f *failUpdateAlways) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if strings.HasSuffix(key, ImportKey) && opts.Mode == store.PutUpdate {
		return store.ObjectMeta{}, store.NewPrecondition(key, "rival")
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

type errInjectedT string

func (e errInjectedT) Error() string { return string(e) }

const errInjected = errInjectedT("injected failure")

// gateRoles fails access.json writes while gated (deterministic
// fault injection for the admin stage — unlike flakyRoles, which is
// built for the retry-converges unit).
type gateRoles struct {
	FakeRoles
	mu   sync.Mutex
	fail bool
}

func (g *gateRoles) PutAccess(ctx context.Context, owner, repo string, ver store.Version, vis identity.Visibility, bindings []identity.AccessBinding) (*identity.AccessDoc, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fail {
		return nil, errInjected
	}
	return g.FakeRoles.PutAccess(ctx, owner, repo, ver, vis, bindings)
}

func (g *gateRoles) setFail(f bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fail = f
}

// headlessParams builds file:// params with an email importer (so the
// S7 admin write runs instead of the backstop skip).
func headlessParams(srcURL, owner, name string) Params {
	p := fileParams(owner, name, srcURL)
	p.importer = "importer@example.com"
	return p
}

// manifestVersion returns the manifest's current CAS version ("" when absent).
func manifestVersion(t *testing.T, st store.ObjectStore, owner, name string) string {
	t.Helper()
	meta, err := st.Head(context.Background(), store.RepoPrefix(owner, name)+store.Manifest)
	if err != nil || meta == nil {
		return ""
	}
	return string(meta.Version)
}

// --- ingest failure → rollback to clean → retry succeeds --------------------------------

func TestIssue79IngestFailureRetriesClean(t *testing.T) {
	cfg := testConfig(t)
	mem := store.NewMemory()
	inject := &failPuts{ObjectStore: mem, suffix: ".pack", n: 1}
	svc, _ := testServiceOnStore(t, cfg, &FakeRoles{}, inject)
	remote := t.TempDir() + "/src"
	srcURL := fixtureRepo(t, remote, 3, 0, 0)
	repackSingle(t, remote)

	if _, err := svc.RunHeadless(context.Background(), headlessParams(srcURL, "acme", "ing"), "", "importer@example.com"); err == nil {
		t.Fatalf("ingest failure must fail, not succeed")
	}
	// Rollback cleaned both commit points: the retry starts fresh.
	if v := manifestVersion(t, mem, "acme", "ing"); v != "" {
		t.Fatalf("manifest survived rollback (version %q)", v)
	}
	if doc, _, err := readImportDoc(context.Background(), mem, "acme", "ing"); err != nil || doc != nil {
		t.Fatalf("claim survived rollback: %+v %v", doc, err)
	}
	o, err := svc.RunHeadless(context.Background(), headlessParams(srcURL, "acme", "ing"), "", "importer@example.com")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(o.HeadSHAs) == 0 {
		t.Fatalf("retry outcome has no heads: %+v", o)
	}
	doc, _, err := readImportDoc(context.Background(), mem, "acme", "ing")
	if err != nil || doc == nil || !doc.Complete || doc.SourceURL != srcURL {
		t.Fatalf("landed doc = %+v %v", doc, err)
	}
}

// --- admin failure + uncleanable manifest → resume converges ----------------------------

func TestIssue79AdminFailureResumes(t *testing.T) {
	cfg := testConfig(t)
	mem := store.NewMemory()
	wrapped := &blockManifestDelete{ObjectStore: mem}
	roles := &gateRoles{}
	roles.setFail(true) // fail every access.json write in phase 1
	svc, _ := testServiceOnStore(t, cfg, roles, wrapped)
	remote := t.TempDir() + "/src"
	srcURL := fixtureRepo(t, remote, 3, 1, 1)
	repackSingle(t, remote)
	params := headlessParams(srcURL, "acme", "res")

	if _, err := svc.RunHeadless(context.Background(), params, "", "importer@example.com"); statusCode(err) != 500 {
		t.Fatalf("admin failure = %v, want 500", err)
	}
	// The manifest delete was blocked, so the claim MUST survive for
	// the resume (deleting it would wedge exactly as #79 describes).
	manVer := manifestVersion(t, mem, "acme", "res")
	if manVer == "" {
		t.Fatalf("manifest must survive when rollback cannot clean")
	}
	doc, claimVer, err := readImportDoc(context.Background(), mem, "acme", "res")
	if err != nil || doc == nil || doc.Complete || doc.SourceURL != srcURL {
		t.Fatalf("in-progress claim = %+v %v, want surviving claim", doc, err)
	}
	if claimVer == "" {
		t.Fatalf("claim has no CAS version")
	}

	// Same-source retry resumes: same manifest version (no
	// delete/recreate cycle), converged refs, landed doc, admin bound.
	roles.setFail(false)
	o, err := svc.RunHeadless(context.Background(), params, "", "importer@example.com")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(o.HeadSHAs) != 3 { // main + b0 + v0
		t.Fatalf("resumed heads = %v, want 3 refs", o.HeadSHAs)
	}
	if v := manifestVersion(t, mem, "acme", "res"); v != manVer {
		t.Fatalf("manifest version %q, want surviving %q (resume, not recreate)", v, manVer)
	}
	doc, _, err = readImportDoc(context.Background(), mem, "acme", "res")
	if err != nil || doc == nil || !doc.Complete {
		t.Fatalf("landed doc = %+v %v", doc, err)
	}
	for ref, sha := range o.HeadSHAs {
		if doc.HeadSHAs[ref] != sha {
			t.Fatalf("head_shas[%q] = %q, outcome %q", ref, doc.HeadSHAs[ref], sha)
		}
	}
	found := false
	for _, b := range roles.Bindings["acme/res"] {
		if b.Subject == "user:importer@example.com" && b.Role == "admin" {
			found = true
		}
	}
	if !found {
		t.Fatalf("importer-admin missing after resume: %+v", roles.Bindings["acme/res"])
	}
}

// --- doc-completion failure → rollback to clean → retry succeeds -------------------------

func TestIssue79DocFailureRetriesClean(t *testing.T) {
	cfg := testConfig(t)
	mem := store.NewMemory()
	inject := &failDocUpdates{ObjectStore: mem, n: 1}
	svc, _ := testServiceOnStore(t, cfg, &FakeRoles{}, inject)
	remote := t.TempDir() + "/src"
	srcURL := fixtureRepo(t, remote, 2, 0, 0)
	repackSingle(t, remote)

	if _, err := svc.RunHeadless(context.Background(), headlessParams(srcURL, "acme", "doc"), "", "importer@example.com"); err == nil {
		t.Fatalf("doc failure must fail, not succeed")
	}
	if v := manifestVersion(t, mem, "acme", "doc"); v != "" {
		t.Fatalf("manifest survived rollback (version %q)", v)
	}
	o, err := svc.RunHeadless(context.Background(), headlessParams(srcURL, "acme", "doc"), "", "importer@example.com")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(o.HeadSHAs) == 0 {
		t.Fatalf("retry outcome has no heads: %+v", o)
	}
}

// --- divergent ref under a wedged claim aborts loud, never overwrites ---------------------

func TestIssue79DivergentRefAborts(t *testing.T) {
	cfg := testConfig(t)
	svc, _ := testService(t, cfg, &FakeRoles{})
	ctx := context.Background()
	remote := t.TempDir() + "/src"
	srcURL := fixtureRepo(t, remote, 3, 0, 0)
	repackSingle(t, remote)
	oldTip := gitOutput(t, srcURL, "rev-parse", "refs/heads/main~1")

	// Wedged state, then a FOREIGN writer moves the ref elsewhere.
	if _, err := svc.reg.Create(ctx, "acme/div", 0); err != nil {
		t.Fatal(err)
	}
	claim := &ImportDoc{Version: 1, SourceURL: srcURL, SourceKind: "file", RequestedRefs: []string{}, HeadSHAs: map[string]string{}, Importer: "x", Format: "sha1", ImportedAt: nowRFC3339(), ClaimExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}
	if _, _, _, err := claimImportDoc(ctx, svc.store, "acme", "div", claim); err != nil {
		t.Fatal(err)
	}
	h, err := svc.reg.Open(ctx, "acme/div")
	if err != nil {
		t.Fatal(err)
	}
	seed := &proto.RefTransaction{Updates: []*proto.RefUpdate{
		{Name: "refs/heads/main", OldOid: strings.Repeat("0", 40), NewOid: oldTip},
	}}
	if _, err := h.Publish(ctx, wal.PublishRequest{Txn: seed, Meta: map[string]string{"agent": "test"}}); err != nil {
		t.Fatalf("seed divergent ref: %v", err)
	}

	in := &importNarr{print: &Printer{Context: ctx}}
	if err := svc.runImport(ctx, in, "i-div", headlessParams(srcURL, "acme", "div"), ""); statusCode(err) != 409 {
		t.Fatalf("divergent ref = %v, want loud 409 (never overwrite)", err)
	}
}

// --- Begin matrix: resume 202, live-foreign 409, expired takeover --------------------------

func TestIssue79BeginResumeMatrix(t *testing.T) {
	cfg := testConfig(t)
	svc, _ := testService(t, cfg, &FakeRoles{})
	h := testHandler(svc, adminPrincipal())
	ctx := context.Background()
	remote := t.TempDir() + "/src"
	srcURL := fixtureRepo(t, remote, 2, 0, 0)
	repackSingle(t, remote)
	other := t.TempDir() + "/other"
	otherURL := fixtureRepo(t, other, 1, 0, 0)

	mkBody := func(url, owner, name string) string {
		return `{"source_url":` + q(url) + `,"owner":` + q(owner) + `,"name":` + q(name) + `}`
	}
	// In-progress claim, same source → 202 resume task (never 409).
	live := &ImportDoc{Version: 1, SourceURL: srcURL, SourceKind: "file", RequestedRefs: []string{}, HeadSHAs: map[string]string{}, Importer: "x", Format: "sha1", ImportedAt: nowRFC3339(), ClaimExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}
	if _, _, _, err := claimImportDoc(ctx, svc.store, "acme", "rsm", live); err != nil {
		t.Fatal(err)
	}
	w := doPost(t, h, "/api/v1/repos/imports", mkBody(srcURL, "acme", "rsm"), "")
	if w.Code != 202 {
		t.Fatalf("resume re-POST = %d (%q), want 202", w.Code, w.Body.String())
	}
	var started struct {
		Task map[string]any `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	id, _ := started.Task["id"].(string)
	o := awaitDone(t, svc, id, 120*time.Second)
	if o.Err != nil {
		t.Fatalf("resumed import: %v", o.Err)
	}
	if doc, _, err := readImportDoc(ctx, svc.store, "acme", "rsm"); err != nil || doc == nil || !doc.Complete {
		t.Fatalf("resumed doc = %+v %v", doc, err)
	}

	// In-progress claim, different source, live lease → 409.
	w2 := doPost(t, h, "/api/v1/repos/imports", mkBody(otherURL, "acme", "rsm"), "")
	if w2.Code != 409 {
		t.Fatalf("foreign live re-POST = %d, want 409", w2.Code)
	}

	// In-progress claim WITH a manifest, same source → 202 resume
	// (the wedge state itself, driven through HTTP).
	if _, _, _, err := claimImportDoc(ctx, svc.store, "acme", "rsm2", live); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.reg.Create(ctx, "acme/rsm2", 0); err != nil {
		t.Fatal(err)
	}
	wR := doPost(t, h, "/api/v1/repos/imports", mkBody(srcURL, "acme", "rsm2"), "")
	if wR.Code != 202 {
		t.Fatalf("wedged re-POST = %d (%q), want 202 resume", wR.Code, wR.Body.String())
	}
	var startedR struct {
		Task map[string]any `json:"task"`
	}
	if err := json.Unmarshal(wR.Body.Bytes(), &startedR); err != nil {
		t.Fatal(err)
	}
	idR, _ := startedR.Task["id"].(string)
	if oR := awaitDone(t, svc, idR, 120*time.Second); oR.Err != nil {
		t.Fatalf("wedged resume: %v", oR.Err)
	}

	// In-progress claim WITH a manifest, different source → 409 (a
	// live repo is never taken over).
	if _, _, _, err := claimImportDoc(ctx, svc.store, "acme", "forprog", live); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.reg.Create(ctx, "acme/forprog", 0); err != nil {
		t.Fatal(err)
	}
	wF := doPost(t, h, "/api/v1/repos/imports", mkBody(otherURL, "acme", "forprog"), "")
	if wF.Code != 409 {
		t.Fatalf("foreign in-progress POST = %d, want 409", wF.Code)
	}

	// Expired claim + no manifest + different source → 202 takeover.
	stale := &ImportDoc{Version: 1, SourceURL: srcURL, SourceKind: "file", RequestedRefs: []string{}, HeadSHAs: map[string]string{}, Importer: "x", Format: "sha1", ImportedAt: nowRFC3339(), ClaimExpiresAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)}
	if _, _, _, err := claimImportDoc(ctx, svc.store, "acme", "take", stale); err != nil {
		t.Fatal(err)
	}
	w3 := doPost(t, h, "/api/v1/repos/imports", mkBody(otherURL, "acme", "take"), "")
	if w3.Code != 202 {
		t.Fatalf("takeover POST = %d (%q), want 202", w3.Code, w3.Body.String())
	}
	var started3 struct {
		Task map[string]any `json:"task"`
	}
	if err := json.Unmarshal(w3.Body.Bytes(), &started3); err != nil {
		t.Fatal(err)
	}
	id3, _ := started3.Task["id"].(string)
	o3 := awaitDone(t, svc, id3, 120*time.Second)
	if o3.Err != nil {
		t.Fatalf("takeover import: %v", o3.Err)
	}
	if doc, _, err := readImportDoc(ctx, svc.store, "acme", "take"); err != nil || doc == nil || !doc.Complete || doc.SourceURL != otherURL {
		t.Fatalf("takeover doc = %+v %v", doc, err)
	}
}

// --- concurrent same-target imports elect one winner ----------------------------------------

func TestIssue79ConcurrentSameTarget(t *testing.T) {
	cfg := testConfig(t)
	svc, _ := testService(t, cfg, &FakeRoles{})
	ctx := context.Background()
	remote := t.TempDir() + "/src"
	srcURL := fixtureRepo(t, remote, 2, 0, 0)
	repackSingle(t, remote)
	body := `{"source_url":` + q(srcURL) + `,"owner":"acme","name":"race"}`

	const n = 8
	type res struct {
		id     string
		joined bool
		err    error
	}
	out := make([]res, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, tok, perr := ParseRequest([]byte(body), cfg)
			if perr != nil {
				out[i] = res{err: perr}
				return
			}
			r, _, berr := svc.Begin(ctx, adminPrincipal(), p, tok)
			if berr != nil {
				out[i] = res{err: berr}
				return
			}
			out[i] = res{id: r.TaskID, joined: r.Joined}
		}(i)
	}
	wg.Wait()
	ids := map[string]int{}
	leaders := 0
	for _, r := range out {
		if r.err != nil {
			t.Fatalf("begin error: %v", r.err)
		}
		ids[r.id]++
		if !r.joined {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("leaders = %d, want exactly 1", leaders)
	}
	if len(ids) != 1 {
		t.Fatalf("task ids = %v, want exactly one shared id", ids)
	}
	for id := range ids {
		o := awaitDone(t, svc, id, 120*time.Second)
		if o.Err != nil {
			t.Fatalf("winner: %v", o.Err)
		}
	}
	doc, _, err := readImportDoc(ctx, svc.store, "acme", "race")
	if err != nil || doc == nil || !doc.Complete || doc.SourceURL != srcURL {
		t.Fatalf("landed doc = %+v %v", doc, err)
	}
}

// --- cross-instance same-source converge: both callers succeed -------------------------------

func TestIssue79CrossInstanceConverge(t *testing.T) {
	cfg := testConfig(t)
	mem := store.NewMemory()
	svcA, _ := testServiceOnStore(t, cfg, &FakeRoles{}, mem)
	svcB, _ := testServiceOnStore(t, cfg, &FakeRoles{}, mem)
	remote := t.TempDir() + "/src"
	srcURL := fixtureRepo(t, remote, 2, 0, 0)
	repackSingle(t, remote)

	// Each side retries until success or the deadline (a lost CAS
	// adopts or resumes on the next attempt — the caller-visible
	// contract). Completion is monotonic, so the loop always
	// terminates; a fixed attempt budget would flake under -race
	// scheduling skew.
	runUntilOK := func(svc *Service) error {
		deadline := time.Now().Add(60 * time.Second)
		var last error
		for time.Now().Before(deadline) {
			if _, err := svc.RunHeadless(context.Background(), headlessParams(srcURL, "acme", "xi"), "", "importer@example.com"); err == nil {
				return nil
			} else {
				last = err
			}
		}
		return last
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, svc := range []*Service{svcA, svcB} {
		wg.Add(1)
		go func(i int, svc *Service) {
			defer wg.Done()
			errs[i] = runUntilOK(svc)
		}(i, svc)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("sides failed: %v / %v", errs[0], errs[1])
	}
	doc, _, err := readImportDoc(context.Background(), mem, "acme", "xi")
	if err != nil || doc == nil || !doc.Complete || doc.SourceURL != srcURL {
		t.Fatalf("landed doc = %+v %v", doc, err)
	}
}

// --- coverage-gap units: every new error arm -----------------------------------------------

// failHeadStore fails Head probes for a key suffix.
type failHeadStore struct {
	store.ObjectStore
	suffix string
}

func (f *failHeadStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	if strings.HasSuffix(key, f.suffix) {
		return nil, store.NewOther(key, errInjected)
	}
	return f.ObjectStore.Head(ctx, key)
}

// failNthWrite scripts a Put sequence: each entry fires once in order
// (412-phantom or generic), then passes through.
type scriptedPut struct {
	store.ObjectStore
	mu    sync.Mutex
	steps []scriptedStep
}

type scriptedStep struct {
	suffix  string
	mode    store.PutMode
	fail412 bool // else generic error
}

func (f *scriptedPut) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.steps) > 0 && strings.HasSuffix(key, f.steps[0].suffix) && opts.Mode == f.steps[0].mode {
		st := f.steps[0]
		f.steps = f.steps[1:]
		if st.fail412 {
			return store.ObjectMeta{}, store.NewPrecondition(key, "")
		}
		return store.ObjectMeta{}, store.NewOther(key, errInjected)
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

// failManifestGet fails GETs of the manifest key only (resume Open
// fails while the claim read succeeds).
type failManifestGet struct {
	store.ObjectStore
}

func (f *failManifestGet) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if strings.HasSuffix(key, store.Manifest) {
		return nil, store.NewOther(key, errInjected)
	}
	return f.ObjectStore.Get(ctx, key, opts)
}

// plantCorruptManifest plants corrupt manifest bytes on the first
// manifest PutCreate, then reports a 412 (lost the race to a rival
// creator — the resume re-Open error arm, deterministically).
type plantCorruptManifest struct {
	store.ObjectStore
	mu   sync.Mutex
	done bool
}

// plantLiveManifest creates a REAL rival repo through a nested
// registry on the first manifest PutCreate, then reports a 412 (we
// lost the name race to a live creator — the resume re-Open success
// arm, deterministically).
type plantLiveManifest struct {
	store.ObjectStore
	mu   sync.Mutex
	done bool
	cfg  *config.Config
}

// plantRepo creates an empty repo id through a throwaway registry over
// st (the rival creator's commit; closed immediately).
func plantRepo(ctx context.Context, st store.ObjectStore, cfg *config.Config, id string) error {
	reg := wal.NewRegistry(ctx, st, cfg)
	defer reg.Close()
	_, err := reg.Create(ctx, id, 0)
	return err
}

func (f *plantLiveManifest) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	f.mu.Lock()
	plant := !f.done && strings.HasSuffix(key, store.Manifest) && opts.Mode == store.PutCreate
	if plant {
		// Set done BEFORE the nested create: the rival's own
		// manifest PutCreate re-enters this wrapper and must pass
		// through (a held mutex here would deadlock).
		f.done = true
	}
	f.mu.Unlock()
	if plant {
		id := strings.TrimSuffix(strings.TrimPrefix(key, "repos/"), "/"+store.Manifest)
		if err := plantRepo(ctx, f.ObjectStore, f.cfg, id); err != nil {
			return store.ObjectMeta{}, err
		}
		return store.ObjectMeta{}, store.NewPrecondition(key, "")
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func (f *plantCorruptManifest) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.done && strings.HasSuffix(key, store.Manifest) && opts.Mode == store.PutCreate {
		f.done = true
		if _, err := f.ObjectStore.Put(ctx, key, store.PutBody{Bytes: []byte("corrupt")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
			return store.ObjectMeta{}, err
		}
		return store.ObjectMeta{}, store.NewPrecondition(key, "")
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

// drainOnClaim drains the service right after the first import.json
// PutCreate lands (phase 1 between the claim and the manifest commit —
// the pre-commit re-guard must refuse without landing anything).
type drainOnClaim struct {
	store.ObjectStore
	mu   sync.Mutex
	svc  *Service
	done bool
}

func (f *drainOnClaim) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	f.mu.Lock()
	drain := !f.done && strings.HasSuffix(key, ImportKey) && opts.Mode == store.PutCreate
	if drain {
		f.done = true
	}
	svc := f.svc
	f.mu.Unlock()
	meta, err := f.ObjectStore.Put(ctx, key, body, opts)
	if drain && err == nil && svc != nil {
		svc.Drain()
	}
	return meta, err
}

// cancelOnClaim cancels the run ctx right after the first import.json
// PutCreate lands (the body-ctx pre-commit re-guard must refuse without
// landing anything).
type cancelOnClaim struct {
	store.ObjectStore
	mu     sync.Mutex
	cancel context.CancelFunc
	done   bool
}

func (f *cancelOnClaim) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	f.mu.Lock()
	fire := !f.done && strings.HasSuffix(key, ImportKey) && opts.Mode == store.PutCreate
	if fire {
		f.done = true
	}
	cancel := f.cancel
	f.mu.Unlock()
	meta, err := f.ObjectStore.Put(ctx, key, body, opts)
	if fire && err == nil && cancel != nil {
		cancel()
	}
	return meta, err
}

// gitCheckout checks out branch in the dir behind a file:// URL.
func gitCheckout(t *testing.T, dirURL, branch string) {
	t.Helper()
	dir := strings.TrimPrefix(dirURL, "file://")
	if out, err := exec.Command("git", "-C", dir, "checkout", "-q", branch).CombinedOutput(); err != nil {
		t.Fatalf("git checkout %s: %v\n%s", branch, err, out)
	}
}

func TestIssue79ErrorArms(t *testing.T) {
	ctx := context.Background()
	newClaim := func(src string) *ImportDoc {
		return &ImportDoc{Version: 1, SourceURL: src, SourceKind: "file", RequestedRefs: []string{}, HeadSHAs: map[string]string{}, Importer: "x", Format: "sha1", ImportedAt: nowRFC3339(), ClaimExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}
	}
	t.Run("claimTTL nil config", func(t *testing.T) {
		if claimTTL(nil) <= 0 {
			t.Fatalf("nil-config TTL must be positive")
		}
	})
	t.Run("isNotFound maps both families", func(t *testing.T) {
		if isNotFound(nil) || !isNotFound(store.NewNotFound("k")) ||
			!isNotFound(&wal.WalError{Kind: wal.WalErrNotFound}) ||
			isNotFound(store.NewOther("k", errInjected)) {
			t.Fatalf("isNotFound mapping wrong")
		}
	})
	t.Run("seed 412 then store error", func(t *testing.T) {
		st := store.NewMemory()
		if err := writeImportDoc(ctx, st, "a", "b", &ImportDoc{Version: 1, SourceURL: "u"}); err != nil {
			t.Fatal(err)
		}
		if err := writeImportDoc(ctx, st, "a", "b", &ImportDoc{Version: 1, SourceURL: "v"}); statusCode(err) != 409 {
			t.Fatalf("seed clash = %v, want 409", err)
		}
		if err := writeImportDoc(ctx, &errCreateStore{ObjectStore: store.NewMemory()}, "a", "b", &ImportDoc{Version: 1}); statusCode(err) != 500 {
			t.Fatalf("seed store-down = %v, want 500", err)
		}
	})
	t.Run("complete loss adopts same source", func(t *testing.T) {
		st := store.NewMemory()
		_, _, ver, err := claimImportDoc(ctx, st, "a", "b", newClaim("file:///a.git"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.PutBytes(ctx, st, store.RepoPrefix("a", "b")+store.Manifest, []byte("m"), store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		if _, err := completeImportDoc(ctx, st, "a", "b", &ImportDoc{Version: 1, SourceURL: "file:///a.git", HeadSHAs: map[string]string{}}, ver); err != nil {
			t.Fatalf("first complete: %v", err)
		}
		// The pre-first base is stale now: the CAS loses and adopts
		// the same-source completion.
		doc, err := completeImportDoc(ctx, st, "a", "b", &ImportDoc{Version: 1, SourceURL: "file:///a.git", HeadSHAs: map[string]string{}}, ver)
		if err != nil || doc == nil || !doc.Complete {
			t.Fatalf("superseded complete = %+v %v, want adopt", doc, err)
		}
	})
	t.Run("complete loss with corrupt readback", func(t *testing.T) {
		st := store.NewMemory()
		if _, err := store.PutBytes(ctx, st, importKey("a", "b"), []byte("{bad"), store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		done := &ImportDoc{Version: 1, SourceURL: "file:///a.git"}
		if _, err := completeImportDoc(ctx, &failUpdateAlways{ObjectStore: st}, "a", "b", done, "v"); statusCode(err) != 409 {
			t.Fatalf("corrupt readback = %v, want 409", err)
		}
	})
	t.Run("double phantom create 409s", func(t *testing.T) {
		st := &scriptedPut{ObjectStore: store.NewMemory(), steps: []scriptedStep{
			{suffix: ImportKey, mode: store.PutCreate, fail412: true},
			{suffix: ImportKey, mode: store.PutCreate, fail412: true},
		}}
		if _, _, _, err := claimImportDoc(ctx, st, "a", "b", newClaim("file:///a.git")); statusCode(err) != 409 {
			t.Fatalf("double phantom = %v, want 409", err)
		}
	})
	t.Run("phantom then store error 500s", func(t *testing.T) {
		st := &scriptedPut{ObjectStore: store.NewMemory(), steps: []scriptedStep{
			{suffix: ImportKey, mode: store.PutCreate, fail412: true},
			{suffix: ImportKey, mode: store.PutCreate, fail412: false},
		}}
		if _, _, _, err := claimImportDoc(ctx, st, "a", "b", newClaim("file:///a.git")); statusCode(err) != 500 {
			t.Fatalf("phantom+error = %v, want 500", err)
		}
	})
	t.Run("takeover CAS loss retries to fresh", func(t *testing.T) {
		st := &scriptedPut{ObjectStore: store.NewMemory(), steps: []scriptedStep{
			{suffix: ImportKey, mode: store.PutUpdate, fail412: true},
		}}
		stale := newClaim("file:///a.git")
		stale.ClaimExpiresAt = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
		if _, _, _, err := claimImportDoc(ctx, st, "a", "b", stale); err != nil {
			t.Fatal(err)
		}
		mode, _, ver, err := claimImportDoc(ctx, st, "a", "b", newClaim("file:///b.git"))
		if err != nil || mode != claimFresh || ver == "" {
			t.Fatalf("takeover retry = %v %q %v, want fresh", mode, ver, err)
		}
	})
	t.Run("takeover double CAS loss 409s", func(t *testing.T) {
		st := &scriptedPut{ObjectStore: store.NewMemory(), steps: []scriptedStep{
			{suffix: ImportKey, mode: store.PutUpdate, fail412: true},
			{suffix: ImportKey, mode: store.PutUpdate, fail412: true},
		}}
		stale := newClaim("file:///a.git")
		stale.ClaimExpiresAt = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
		if _, _, _, err := claimImportDoc(ctx, st, "a", "b", stale); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := claimImportDoc(ctx, st, "a", "b", newClaim("file:///b.git")); statusCode(err) != 409 {
			t.Fatalf("takeover double loss = %v, want 409", err)
		}
	})
	t.Run("takeover CAS store error 500s", func(t *testing.T) {
		st := &scriptedPut{ObjectStore: store.NewMemory(), steps: []scriptedStep{
			{suffix: ImportKey, mode: store.PutUpdate, fail412: false},
		}}
		stale := newClaim("file:///a.git")
		stale.ClaimExpiresAt = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
		if _, _, _, err := claimImportDoc(ctx, st, "a", "b", stale); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := claimImportDoc(ctx, st, "a", "b", newClaim("file:///b.git")); statusCode(err) != 500 {
			t.Fatalf("takeover store error = %v, want 500", err)
		}
	})
	t.Run("presence probe errors", func(t *testing.T) {
		cfg := testConfig(t)
		svc, _ := testServiceOnStore(t, cfg, &FakeRoles{}, &failHeadStore{ObjectStore: store.NewMemory(), suffix: ".pack"})
		p := fileParams("a", "b", "file:///x")
		if _, err := svc.packPresent(context.Background(), p, "abc"); statusCode(err) != 500 {
			t.Fatalf("pack probe error = %v, want 500", err)
		}
		svc2, _ := testServiceOnStore(t, cfg, &FakeRoles{}, &failHeadStore{ObjectStore: store.NewMemory(), suffix: ".idx"})
		if _, err := svc2.idxPresent(context.Background(), p, "abc"); statusCode(err) != 500 {
			t.Fatalf("idx probe error = %v, want 500", err)
		}
	})
	t.Run("currentRefs with broken git", func(t *testing.T) {
		cfg := testConfig(t)
		svc, _ := testService(t, cfg, &FakeRoles{})
		if _, err := svc.reg.Create(ctx, "acme/broken", 0); err != nil {
			t.Fatal(err)
		}
		h, err := svc.reg.Open(ctx, "acme/broken")
		if err != nil {
			t.Fatal(err)
		}
		svc.git.Binary = "/nonexistent/git-binary-xyz"
		if _, _, _, err := svc.currentRefs(ctx, h); statusCode(err) != 500 {
			t.Fatalf("broken git refs = %v, want 500", err)
		}
	})
	t.Run("installIdx with broken git", func(t *testing.T) {
		cfg := testConfig(t)
		svc, _ := testService(t, cfg, &FakeRoles{})
		if _, err := svc.reg.Create(ctx, "acme/idx", 0); err != nil {
			t.Fatal(err)
		}
		h, err := svc.reg.Open(ctx, "acme/idx")
		if err != nil {
			t.Fatal(err)
		}
		svc.git.Binary = "/nonexistent/git-binary-xyz"
		pack := t.TempDir() + "/x.pack"
		writeFile(t, pack, "PACK")
		if _, err := svc.installIdx(ctx, h, pack, "deadbeef"); statusCode(err) != 502 {
			t.Fatalf("broken git idx = %v, want 502", err)
		}
	})
	t.Run("idx upload failure rolls back clean", func(t *testing.T) {
		cfg := testConfig(t)
		mem := store.NewMemory()
		inject := &failPuts{ObjectStore: mem, suffix: ".idx", n: 100}
		svc, _ := testServiceOnStore(t, cfg, &FakeRoles{}, inject)
		remote := t.TempDir() + "/src"
		srcURL := fixtureRepo(t, remote, 2, 0, 0)
		repackSingle(t, remote)
		if _, err := svc.RunHeadless(ctx, headlessParams(srcURL, "acme", "idxfail"), "", "importer@example.com"); statusCode(err) != 500 {
			t.Fatalf("idx failure must 500")
		}
		if v := manifestVersion(t, mem, "acme", "idxfail"); v != "" {
			t.Fatalf("manifest survived idx-failure rollback")
		}
		if doc, _, err := readImportDoc(ctx, mem, "acme", "idxfail"); err != nil || doc != nil {
			t.Fatalf("claim survived idx-failure rollback: %+v %v", doc, err)
		}
	})
	t.Run("resume open failure 500s", func(t *testing.T) {
		cfg := testConfig(t)
		svc, _ := testServiceOnStore(t, cfg, &FakeRoles{}, &failManifestGet{ObjectStore: store.NewMemory()})
		remote := t.TempDir() + "/src"
		srcURL := fixtureRepo(t, remote, 1, 0, 0)
		if _, _, _, err := claimImportDoc(ctx, svc.store, "acme", "opnfail", &ImportDoc{Version: 1, SourceURL: srcURL, SourceKind: "file", RequestedRefs: []string{}, HeadSHAs: map[string]string{}, Importer: "x", Format: "sha1", ImportedAt: nowRFC3339(), ClaimExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}); err != nil {
			t.Fatal(err)
		}
		in := &importNarr{print: &Printer{Context: ctx}}
		if err := svc.runImport(ctx, in, "i-opn", headlessParams(srcURL, "acme", "opnfail"), ""); statusCode(err) != 500 {
			t.Fatalf("resume open failure = %v, want 500", err)
		}
	})
	t.Run("resume create race re-opens corrupt rival", func(t *testing.T) {
		cfg := testConfig(t)
		svc, _ := testServiceOnStore(t, cfg, &FakeRoles{}, &plantCorruptManifest{ObjectStore: store.NewMemory()})
		remote := t.TempDir() + "/src"
		srcURL := fixtureRepo(t, remote, 1, 0, 0)
		if _, _, _, err := claimImportDoc(ctx, svc.store, "acme", "crace", &ImportDoc{Version: 1, SourceURL: srcURL, SourceKind: "file", RequestedRefs: []string{}, HeadSHAs: map[string]string{}, Importer: "x", Format: "sha1", ImportedAt: nowRFC3339(), ClaimExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}); err != nil {
			t.Fatal(err)
		}
		in := &importNarr{print: &Printer{Context: ctx}}
		if err := svc.runImport(ctx, in, "i-crace", headlessParams(srcURL, "acme", "crace"), ""); err == nil {
			t.Fatalf("corrupt rival manifest must fail, not succeed")
		}
	})
	t.Run("fresh create race 409s foreign", func(t *testing.T) {
		cfg := testConfig(t)
		svc, _ := testService(t, cfg, &FakeRoles{})
		remote := t.TempDir() + "/src"
		srcURL := fixtureRepo(t, remote, 1, 0, 0)
		// Foreign manifest, no sidecar: our claim wins, the name is
		// taken → loud 409, and the claim is left for the retry to
		// resume off (never deleted under a potential live winner).
		if _, err := svc.reg.Create(ctx, "acme/frgn", 0); err != nil {
			t.Fatal(err)
		}
		in := &importNarr{print: &Printer{Context: ctx}}
		if err := svc.runImport(ctx, in, "i-frgn", headlessParams(srcURL, "acme", "frgn"), ""); statusCode(err) != 409 {
			t.Fatalf("foreign race = %v, want 409", err)
		}
		doc, _, err := readImportDoc(ctx, svc.store, "acme", "frgn")
		if err != nil || doc == nil || doc.Complete || doc.SourceURL != srcURL {
			t.Fatalf("claim = %+v %v, want surviving in-progress claim", doc, err)
		}
	})
	t.Run("fresh create store error 500s", func(t *testing.T) {
		cfg := testConfig(t)
		st := &scriptedPut{ObjectStore: store.NewMemory(), steps: []scriptedStep{
			{suffix: store.Manifest, mode: store.PutCreate, fail412: false},
		}}
		svc, _ := testServiceOnStore(t, cfg, &FakeRoles{}, st)
		remote := t.TempDir() + "/src"
		srcURL := fixtureRepo(t, remote, 1, 0, 0)
		in := &importNarr{print: &Printer{Context: ctx}}
		if err := svc.runImport(ctx, in, "i-c5xx", headlessParams(srcURL, "acme", "c5xx"), ""); statusCode(err) != 500 {
			t.Fatalf("create store error = %v, want 500", err)
		}
		// No manifest exists: the claim survives for the retry.
		if doc, _, err := readImportDoc(ctx, st, "acme", "c5xx"); err != nil || doc == nil || doc.Complete {
			t.Fatalf("claim = %+v %v, want surviving in-progress claim", doc, err)
		}
	})
	t.Run("drain between claim and commit refuses clean", func(t *testing.T) {
		cfg := testConfig(t)
		mem := store.NewMemory()
		gate := &drainOnClaim{ObjectStore: mem}
		svc, _ := testServiceOnStore(t, cfg, &FakeRoles{}, gate)
		gate.svc = svc
		remote := t.TempDir() + "/src"
		srcURL := fixtureRepo(t, remote, 1, 0, 0)
		in := &importNarr{print: &Printer{Context: ctx}}
		if err := svc.runImport(ctx, in, "i-drn", headlessParams(srcURL, "acme", "drn"), ""); statusCode(err) != 503 {
			t.Fatalf("drained commit = %v, want 503", err)
		}
		if v := manifestVersion(t, mem, "acme", "drn"); v != "" {
			t.Fatalf("manifest landed post-drain")
		}
		if doc, _, err := readImportDoc(ctx, mem, "acme", "drn"); err != nil || doc != nil {
			t.Fatalf("claim survived drain refusal: %+v %v", doc, err)
		}
	})
	t.Run("cancel between claim and commit refuses clean", func(t *testing.T) {
		cfg := testConfig(t)
		mem := store.NewMemory()
		cctx, cancel := context.WithCancel(context.Background())
		gate := &cancelOnClaim{ObjectStore: mem, cancel: cancel}
		svc, _ := testServiceOnStore(t, cfg, &FakeRoles{}, gate)
		remote := t.TempDir() + "/src"
		srcURL := fixtureRepo(t, remote, 1, 0, 0)
		in := &importNarr{print: &Printer{Context: cctx}}
		if err := svc.runImport(cctx, in, "i-cxl", headlessParams(srcURL, "acme", "cxl"), ""); statusCode(err) != 503 {
			t.Fatalf("canceled commit = %v, want 503", err)
		}
		if v := manifestVersion(t, mem, "acme", "cxl"); v != "" {
			t.Fatalf("manifest landed post-cancel")
		}
		if doc, _, err := readImportDoc(ctx, mem, "acme", "cxl"); err != nil || doc != nil {
			t.Fatalf("claim survived cancel refusal: %+v %v", doc, err)
		}
	})
	t.Run("pack probe failure in body 500s", func(t *testing.T) {
		cfg := testConfig(t)
		svc, _ := testServiceOnStore(t, cfg, &FakeRoles{}, &failHeadStore{ObjectStore: store.NewMemory(), suffix: ".pack"})
		remote := t.TempDir() + "/src"
		srcURL := fixtureRepo(t, remote, 1, 0, 0)
		if _, err := svc.RunHeadless(ctx, headlessParams(srcURL, "acme", "probe"), "", "importer@example.com"); statusCode(err) != 500 {
			t.Fatalf("pack probe failure must 500")
		}
	})
	t.Run("non-main HEAD is followed", func(t *testing.T) {
		cfg := testConfig(t)
		svc, _ := testService(t, cfg, &FakeRoles{})
		remote := t.TempDir() + "/src"
		srcURL := fixtureRepo(t, remote, 2, 1, 0)
		repackSingle(t, remote)
		gitCheckout(t, srcURL, "b0")
		o, err := svc.RunHeadless(ctx, headlessParams(srcURL, "acme", "nb"), "", "importer@example.com")
		if err != nil {
			t.Fatalf("non-main HEAD import: %v", err)
		}
		if o.HeadSHAs["refs/heads/b0"] == "" || len(o.HeadSHAs) != 2 {
			t.Fatalf("heads = %v, want main+b0", o.HeadSHAs)
		}
	})
	t.Run("resume create race adopts live rival", func(t *testing.T) {
		cfg := testConfig(t)
		inner := store.NewMemory()
		svc, _ := testServiceOnStore(t, cfg, &FakeRoles{}, &plantLiveManifest{ObjectStore: inner, cfg: cfg})
		remote := t.TempDir() + "/src"
		srcURL := fixtureRepo(t, remote, 1, 0, 0)
		repackSingle(t, remote)
		if _, _, _, err := claimImportDoc(ctx, svc.store, "acme", "lrace", &ImportDoc{Version: 1, SourceURL: srcURL, SourceKind: "file", RequestedRefs: []string{}, HeadSHAs: map[string]string{}, Importer: "x", Format: "sha1", ImportedAt: nowRFC3339(), ClaimExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}); err != nil {
			t.Fatal(err)
		}
		// Our Create loses to the planted rival; the re-Open finds a
		// live same-content repo and the run converges to completion.
		o, err := svc.RunHeadless(ctx, headlessParams(srcURL, "acme", "lrace"), "", "importer@example.com")
		if err != nil {
			t.Fatalf("live-rival resume: %v", err)
		}
		if len(o.HeadSHAs) == 0 {
			t.Fatalf("live-rival outcome has no heads: %+v", o)
		}
	})
}

// failAllLog fails every log-segment PutCreate and every log HEAD
// (the §5.4 slot loop retries a lone PUT failure against a healthy
// slot probe, so both must fail to surface the publisher error —
// exactly the "rival writer" outage shape).
type failAllLog struct {
	store.ObjectStore
}

func (f *failAllLog) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if strings.Contains(key, "/log/") && opts.Mode == store.PutCreate {
		return store.ObjectMeta{}, store.NewOther(key, errInjected)
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func (f *failAllLog) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	if strings.Contains(key, "/log/") {
		return nil, store.NewOther(key, errInjected)
	}
	return f.ObjectStore.Head(ctx, key)
}

// TestIssue79ConvergeArms drives completeBody directly: a rival writer
// racing the ref converge surfaces a retryable 500 (never a wedge),
// and an unreadable serving copy fails the converge loud.
func TestIssue79ConvergeArms(t *testing.T) {
	ctx := context.Background()
	t.Run("ref publish race is retryable", func(t *testing.T) {
		cfg := testConfig(t)
		mem := store.NewMemory()
		svc, _ := testServiceOnStore(t, cfg, &FakeRoles{}, mem)
		remote := t.TempDir() + "/src"
		srcURL := fixtureRepo(t, remote, 2, 1, 0)
		repackSingle(t, remote)
		params := headlessParams(srcURL, "acme", "cfl")
		if _, err := svc.RunHeadless(ctx, params, "", "importer@example.com"); err != nil {
			t.Fatalf("seed import: %v", err)
		}
		// Remove one ref behind the completed doc's back.
		h, err := svc.reg.Open(ctx, "acme/cfl")
		if err != nil {
			t.Fatal(err)
		}
		tips, _, _, err := svc.currentRefs(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		b0, ok := tips["refs/heads/b0"]
		if !ok {
			t.Fatalf("seed has no b0: %v", tips)
		}
		if _, err := h.Publish(ctx, wal.PublishRequest{Txn: &proto.RefTransaction{Updates: []*proto.RefUpdate{
			{Name: "refs/heads/b0", OldOid: b0, NewOid: strings.Repeat("0", 40)},
		}}, Meta: map[string]string{"agent": "test"}}); err != nil {
			t.Fatalf("delete b0: %v", err)
		}
		// Fresh scratch + refs, then converge with the log raced.
		wrapped := &failAllLog{ObjectStore: mem}
		svc2, _ := testServiceOnStore(t, cfg, &FakeRoles{}, wrapped)
		scratch, err := svc2.git.ScratchDir("acme", "cfl")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(scratch)
		if err := svc2.git.CloneMirror(ctx, srcURL, scratch, "file", "", "", "",
			func(string, uint64, uint64, string) {}, func() {}); err != nil {
			t.Fatalf("clone: %v", err)
		}
		all, err := svc2.git.ForEachRef(ctx, scratch)
		if err != nil {
			t.Fatal(err)
		}
		refs := FilterRefs(all, false, false, []string{}, false, HeadTarget(scratch))
		h2, err := svc2.reg.Open(ctx, "acme/cfl")
		if err != nil {
			t.Fatal(err)
		}
		_, claimVer, err := readImportDoc(ctx, mem, "acme", "cfl")
		if err != nil {
			t.Fatal(err)
		}
		n := &importNarr{print: &Printer{Context: ctx}}
		if err := svc2.completeBody(ctx, n, h2, params, scratch, refs, HeadTarget(scratch), "sha1", git.Sha1, claimVer); statusCode(err) != 500 {
			t.Fatalf("raced ref publish = %v, want retryable 500", err)
		}
	})
	t.Run("unreadable serving copy fails loud", func(t *testing.T) {
		cfg := testConfig(t)
		svc, _ := testService(t, cfg, &FakeRoles{})
		remote := t.TempDir() + "/src"
		srcURL := fixtureRepo(t, remote, 2, 0, 0)
		repackSingle(t, remote)
		scratch, err := svc.git.ScratchDir("acme", "udr")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(scratch)
		if err := svc.git.CloneMirror(ctx, srcURL, scratch, "file", "", "", "",
			func(string, uint64, uint64, string) {}, func() {}); err != nil {
			t.Fatalf("clone: %v", err)
		}
		// Pin the .idx up front so ingest needs no git at all.
		packs, _ := filepath.Glob(filepath.Join(scratch, "objects", "pack", "*.pack"))
		for _, pack := range packs {
			if _, err := svc.git.EnsurePackIdx(ctx, pack); err != nil {
				t.Fatalf("pin idx: %v", err)
			}
		}
		all, err := svc.git.ForEachRef(ctx, scratch)
		if err != nil {
			t.Fatal(err)
		}
		headTarget := HeadTarget(scratch)
		refs := FilterRefs(all, false, false, []string{}, false, headTarget)
		params := headlessParams(srcURL, "acme", "udr")
		claim := &ImportDoc{Version: 1, SourceURL: srcURL, SourceKind: "file", RequestedRefs: []string{}, HeadSHAs: map[string]string{}, Importer: "x", Format: "sha1", ImportedAt: nowRFC3339(), ClaimExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}
		mode, _, claimVer, err := claimImportDoc(ctx, svc.store, "acme", "udr", claim)
		if err != nil || mode != claimFresh {
			t.Fatalf("claim: %v %v", mode, err)
		}
		h, err := svc.reg.Create(ctx, "acme/udr", git.Sha1)
		if err != nil {
			t.Fatal(err)
		}
		svc.git.Binary = "/nonexistent/git-binary-xyz"
		n := &importNarr{print: &Printer{Context: ctx}}
		if err := svc.completeBody(ctx, n, h, params, scratch, refs, headTarget, "sha1", git.Sha1, claimVer); statusCode(err) != 500 {
			t.Fatalf("unreadable refs = %v, want 500", err)
		}
	})
}

func TestIssue79ClaimUnits(t *testing.T) {
	ctx := context.Background()
	t.Run("phantom 412 retries to fresh", func(t *testing.T) {
		st := store.NewMemory()
		claim := &ImportDoc{Version: 1, SourceURL: "file:///a.git", ClaimExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}
		mode, _, ver, err := claimImportDoc(ctx, &failCreateOnce{ObjectStore: st}, "a", "b", claim)
		if err != nil || mode != claimFresh || ver == "" {
			t.Fatalf("phantom-412 = %v %q %v, want fresh", mode, ver, err)
		}
	})
	t.Run("superseded completion 409s", func(t *testing.T) {
		st := store.NewMemory()
		claim := &ImportDoc{Version: 1, SourceURL: "file:///a.git", ClaimExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}
		if _, _, _, err := claimImportDoc(ctx, st, "a", "b", claim); err != nil {
			t.Fatal(err)
		}
		done := &ImportDoc{Version: 1, SourceURL: "file:///a.git", HeadSHAs: map[string]string{}}
		if _, err := completeImportDoc(ctx, &failUpdateAlways{ObjectStore: st}, "a", "b", done, "stale"); statusCode(err) != 409 {
			t.Fatalf("superseded complete = %v, want 409", err)
		}
	})
	t.Run("expired claim over a live repo never takes over", func(t *testing.T) {
		st := store.NewMemory()
		stale := &ImportDoc{Version: 1, SourceURL: "file:///a.git", ClaimExpiresAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)}
		if _, err := store.PutBytes(ctx, st, importKey("a", "b"), mustMarshalStale(t, stale), store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PutBytes(ctx, st, store.RepoPrefix("a", "b")+store.Manifest, []byte("m"), store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		other := &ImportDoc{Version: 1, SourceURL: "file:///b.git", ClaimExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}
		if _, _, _, err := claimImportDoc(ctx, st, "a", "b", other); statusCode(err) != 409 {
			t.Fatalf("takeover of live repo = %v, want 409", err)
		}
	})
	t.Run("undated claim never expires", func(t *testing.T) {
		if claimExpired(&ImportDoc{}) {
			t.Fatalf("undated claim must never expire")
		}
		if claimExpired(&ImportDoc{ClaimExpiresAt: "not-a-date"}) {
			t.Fatalf("unparseable lease must never expire")
		}
	})
	t.Run("expired manifest-less claim is taken over", func(t *testing.T) {
		st := store.NewMemory()
		stale := &ImportDoc{Version: 1, SourceURL: "file:///a.git", ClaimExpiresAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)}
		if _, _, _, err := claimImportDoc(ctx, st, "a", "b", stale); err != nil {
			t.Fatal(err)
		}
		other := &ImportDoc{Version: 1, SourceURL: "file:///b.git", ClaimExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}
		mode, _, ver, err := claimImportDoc(ctx, st, "a", "b", other)
		if err != nil || mode != claimFresh || ver == "" {
			t.Fatalf("takeover = %v %q %v, want fresh", mode, ver, err)
		}
		cur, _, err := readImportDoc(ctx, st, "a", "b")
		if err != nil || cur == nil || cur.SourceURL != "file:///b.git" {
			t.Fatalf("takeover doc = %+v %v", cur, err)
		}
	})
}

// mustMarshalStale encodes a doc for direct bucket seeding (tests only).
func mustMarshalStale(t *testing.T, doc *ImportDoc) []byte {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// drainOnClaimRead drains the service right after the first import.json
// GET lands. A resume run resolves its claim via a read (no PutCreate),
// which happens after the pre-claim drain guard already passed — the
// pre-commit re-guard must then refuse WITHOUT deleting the shared
// claim it converged off.
type drainOnClaimRead struct {
	store.ObjectStore
	mu   sync.Mutex
	svc  *Service
	done bool
}

func (f *drainOnClaimRead) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	res, err := f.ObjectStore.Get(ctx, key, opts)
	f.mu.Lock()
	drain := !f.done && strings.HasSuffix(key, ImportKey) && err == nil
	if drain {
		f.done = true
	}
	svc := f.svc
	f.mu.Unlock()
	if drain && svc != nil {
		svc.Drain()
	}
	return res, err
}

// TestIssue79DrainRefusalKeepsResumeClaim: a drain landing between the
// claim-resume and the manifest commit must refuse WITHOUT deleting the
// shared in-progress claim — with a surviving manifest, deleting it
// would wedge the retry on a foreign-manifest 409 (the #79 shape).
func TestIssue79DrainRefusalKeepsResumeClaim(t *testing.T) {
	cfg := testConfig(t)
	mem := store.NewMemory()
	gate := &drainOnClaimRead{ObjectStore: mem}
	svc, _ := testServiceOnStore(t, cfg, &FakeRoles{}, gate)
	gate.svc = svc
	ctx := context.Background()
	remote := t.TempDir() + "/src"
	srcURL := fixtureRepo(t, remote, 1, 0, 0)
	// Wedged state: prior attempt's manifest + its in-progress claim.
	if _, err := svc.reg.Create(ctx, "acme/drnk", 0); err != nil {
		t.Fatal(err)
	}
	claim := &ImportDoc{Version: 1, SourceURL: srcURL, SourceKind: "file", RequestedRefs: []string{}, HeadSHAs: map[string]string{}, Importer: "x", Format: "sha1", ImportedAt: nowRFC3339(), ClaimExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}
	if _, _, _, err := claimImportDoc(ctx, svc.store, "acme", "drnk", claim); err != nil {
		t.Fatal(err)
	}
	in := &importNarr{print: &Printer{Context: ctx}}
	if err := svc.runImport(ctx, in, "i-drnk", headlessParams(srcURL, "acme", "drnk"), ""); statusCode(err) != 503 {
		t.Fatalf("drained resume = %v, want 503", err)
	}
	// Both commit points must survive so the retry resumes.
	if v := manifestVersion(t, mem, "acme", "drnk"); v == "" {
		t.Fatalf("manifest deleted under drain refusal")
	}
	if doc, _, err := readImportDoc(ctx, mem, "acme", "drnk"); err != nil || doc == nil || doc.Complete || doc.SourceURL != srcURL {
		t.Fatalf("shared claim deleted under drain refusal: %+v %v", doc, err)
	}
}
