// import_test.go — end-to-end file:// import over real git + memory
// store: 202 → attach → outcome, provenance, importer-admin, no-op,
// secret scrub, and the S1/S8/format gates.
package repoimport

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// q quotes a string for JSON embedding (file:// URLs carry no quotes).
func q(s string) string { return `"` + s + `"` }

// --- full success path (real identity: proves S7 wiring) -----------------------------------

func TestImportFileEndToEnd(t *testing.T) {
	cfg := testConfig(t)
	svc, st := testService(t, cfg, nil)
	roles := realRoles(st, cfg)
	svc.roles = roles
	h := testHandler(svc, auth.Principal{Name: "carol@example.com", Write: true})

	remote := t.TempDir() + "/src"
	srcURL := fixtureRepo(t, remote, 10, 1, 1)
	repackSingle(t, remote)

	w := doPost(t, h, "/api/v1/repos/imports", `{"source_url":`+q(srcURL)+`,"owner":"acme","name":"widget","token":"tok-for-test"}`, "")
	if w.Code != 202 {
		t.Fatalf("POST = %d (%q), want 202", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	var started struct {
		Task   map[string]any `json:"task"`
		Target string         `json:"target"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	id, _ := started.Task["id"].(string)
	if id == "" || started.Target != "acme/widget" {
		t.Fatalf("202 body = %s", w.Body.String())
	}

	o := awaitDone(t, svc, id, 120*time.Second)
	if o.Err != nil {
		t.Fatalf("outcome error: %v (log below)", o.Err)
	}
	if o.Repo != "acme/widget" || o.Format != "sha1" || o.ImportedAt == "" {
		t.Fatalf("outcome = %+v", o)
	}
	if len(o.HeadSHAs) != 3 { // main + b0 + v0
		t.Fatalf("head_shas = %v, want 3 refs", o.HeadSHAs)
	}

	// Provenance: source URL, heads, importer, format (S2: no token).
	doc, _, err := readImportDoc(context.Background(), st, "acme", "widget")
	if err != nil || doc == nil {
		t.Fatalf("import.json: %v %+v", err, doc)
	}
	if doc.SourceURL != srcURL || doc.SourceKind != "file" || doc.Importer != "carol@example.com" || doc.Format != "sha1" {
		t.Fatalf("doc = %+v", doc)
	}
	if len(doc.RequestedRefs) != 0 {
		t.Fatalf("requested_refs = %v, want []", doc.RequestedRefs)
	}
	for ref, sha := range o.HeadSHAs {
		if len(sha) != 40 || doc.HeadSHAs[ref] != sha {
			t.Fatalf("head_shas[%q] = %q", ref, sha)
		}
	}

	// Importer-admin via the identity service (S7).
	acc, _, err := roles.GetAccess(context.Background(), "acme", "widget")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range acc.RoleBindings {
		if b.Subject == "user:carol@example.com" && b.Role == "admin" {
			found = true
		}
	}
	if !found {
		t.Fatalf("importer-admin binding missing: %+v", acc.RoleBindings)
	}

	// GET JSON: frozen shape, []-not-null, RFC3339, full SHAs.
	g := doGet(t, h, "/api/v1/repos/imports/"+id, "")
	if g.Code != 200 {
		t.Fatalf("GET = %d (%q)", g.Code, g.Body.String())
	}
	var rec struct {
		ID       string            `json:"id"`
		Kind     string            `json:"kind"`
		OK       *bool             `json:"ok"`
		LogTail  []string          `json:"log_tail"`
		Params   map[string]string `json:"params"`
		Started  string            `json:"started"`
		Finished string            `json:"finished"`
	}
	if err := json.Unmarshal(g.Body.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.OK == nil || !*rec.OK || len(rec.LogTail) == 0 {
		t.Fatalf("record = %s", g.Body.String())
	}
	if _, err := time.Parse(time.RFC3339, rec.Finished); err != nil {
		t.Fatalf("finished not RFC3339: %q", rec.Finished)
	}
	if rec.Params["source_url"] != srcURL || rec.Params["secret_set"] != "true" {
		t.Fatalf("params = %v", rec.Params)
	}
	if strings.Contains(g.Body.String(), "tok-for-test") {
		t.Fatalf("task JSON leaks token: %s", g.Body.String())
	}

	// Re-POST same source → 200 no-op, zero pack traffic.
	w2 := doPost(t, h, "/api/v1/repos/imports", `{"source_url":`+q(srcURL)+`,"owner":"acme","name":"widget"}`, "")
	if w2.Code != 200 {
		t.Fatalf("re-POST = %d (%q), want 200 no-op", w2.Code, w2.Body.String())
	}
	// Different source, same target → 409.
	w3 := doPost(t, h, "/api/v1/repos/imports", `{"source_url":"file:///elsewhere.git","owner":"acme","name":"widget"}`, "")
	if w3.Code != 409 {
		t.Fatalf("different-source re-POST = %d, want 409", w3.Code)
	}
}

// --- SSE attach: replay → live → terminal exactly once ----------------------------------------

func TestSSEAttach(t *testing.T) {
	cfg := testConfig(t)
	svc, st := testService(t, cfg, nil)
	svc.roles = realRoles(st, cfg)
	h := testHandler(svc, adminPrincipal())

	remote := t.TempDir() + "/src"
	srcURL := fixtureRepo(t, remote, 30, 2, 2)
	repackSingle(t, remote)

	w := doPost(t, h, "/api/v1/repos/imports", `{"source_url":`+q(srcURL)+`,"owner":"acme","name":"stream"}`, "")
	if w.Code != 202 {
		t.Fatalf("POST = %d (%q)", w.Code, w.Body.String())
	}
	var started struct {
		Task map[string]any `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	id, _ := started.Task["id"].(string)

	// Attach immediately (live) — replay covers whatever already ran.
	g := doGet(t, h, "/api/v1/repos/imports/"+id, "text/event-stream")
	if g.Code != 200 {
		t.Fatalf("SSE attach = %d (%q)", g.Code, g.Body.String())
	}
	if ct := g.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	frames := parseSSE(t, g.Body.String())
	var sawNotice, sawProgress, sawResult bool
	var result map[string]any
	for _, f := range frames {
		switch f.event {
		case "notice":
			sawNotice = true
		case "progress":
			sawProgress = true
		case "task":
		case "result":
			sawResult = true
			var err error
			result, err = asMap(f.data)
			if err != nil {
				t.Fatalf("result data: %v", err)
			}
		case "error":
			t.Fatalf("unexpected error frame: %v", f.data)
		}
	}
	if !sawNotice || !sawProgress || !sawResult {
		t.Fatalf("frames miss phases (notice=%v progress=%v result=%v): %d frames", sawNotice, sawProgress, sawResult, len(frames))
	}
	if result["repo"] != "acme/stream" {
		t.Fatalf("result = %v", result)
	}
	heads, _ := result["head_shas"].(map[string]any)
	if len(heads) != 5 { // main + 2 branches + 2 tags
		t.Fatalf("result head_shas = %v", result["head_shas"])
	}

	// Re-attach after finish: replay + terminal, then EOF.
	g2 := doGet(t, h, "/api/v1/repos/imports/"+id, "text/event-stream")
	frames2 := parseSSE(t, g2.Body.String())
	last := frames2[len(frames2)-1]
	if last.event != "result" {
		t.Fatalf("re-attach last frame = %q, want result", last.event)
	}
}

type sseFrame struct {
	event string
	data  any
}

func parseSSE(t *testing.T, body string) []sseFrame {
	t.Helper()
	var out []sseFrame
	for _, chunk := range strings.Split(body, "\n\n") {
		chunk = strings.Trim(chunk, "\n")
		if chunk == "" || strings.HasPrefix(chunk, ":") {
			continue
		}
		var event, data string
		for _, line := range strings.Split(chunk, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if strings.HasPrefix(line, "event:") {
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if event == "" {
			continue
		}
		var v any = data
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err == nil {
			v = m
		}
		out = append(out, sseFrame{event: event, data: v})
	}
	if len(out) == 0 {
		t.Fatalf("no frames in %q", body)
	}
	return out
}

func asMap(v any) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errNotMap
	}
	return m, nil
}

type errString string

func (e errString) Error() string { return string(e) }

var errNotMap errString = "not a map"

// --- secret scrub (S2: params/packets/errors/logs/bucket grep-clean) --------------------------

func TestSecretScrub(t *testing.T) {
	cfg := testConfig(t)
	svc, st := testService(t, cfg, &FakeRoles{})
	h := testHandler(svc, adminPrincipal())
	const token = "ghp_s3cr3t_t0k3n_zzz"
	w := doPost(t, h, "/api/v1/repos/imports", `{"source_url":"file:///srv/missing.git","owner":"acme","name":"scrub","token":`+q(token)+`}`, "")
	if w.Code != 202 {
		t.Fatalf("POST = %d (%q)", w.Code, w.Body.String())
	}
	var started struct {
		Task map[string]any `json:"task"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &started)
	id, _ := started.Task["id"].(string)
	o := awaitDone(t, svc, id, 60*time.Second)
	if o.Err == nil {
		t.Fatalf("missing-file import should fail")
	}
	// Params carry secret_set, never the token.
	g := doGet(t, h, "/api/v1/repos/imports/"+id, "")
	if strings.Contains(g.Body.String(), token) {
		t.Fatalf("task JSON leaks token: %s", g.Body.String())
	}
	if !strings.Contains(g.Body.String(), "secret_set") {
		t.Fatalf("task JSON must carry secret_set: %s", g.Body.String())
	}
	// SSE error frame scrubbed.
	ge := doGet(t, h, "/api/v1/repos/imports/"+id, "text/event-stream")
	if strings.Contains(ge.Body.String(), token) {
		t.Fatalf("SSE stream leaks token")
	}
	// Bucket grep-clean.
	if err := grepBucket(t, st, token); err != nil {
		t.Fatal(err)
	}
	// Outcome error scrubbed.
	if strings.Contains(o.Err.Message, token) {
		t.Fatalf("outcome leaks token: %q", o.Err.Message)
	}
}

func grepBucket(t *testing.T, st store.ObjectStore, needle string) error {
	t.Helper()
	return st.List(context.Background(), "", "", func(m store.ObjectMeta) error {
		raw, _, err := store.GetBytes(context.Background(), st, m.Key, store.GetOptions{})
		if err != nil {
			return nil // races with janitors; absence is fine
		}
		if strings.Contains(string(raw), needle) {
			t.Fatalf("bucket object %s contains %q", m.Key, needle)
		}
		return nil
	})
}

// --- gates: size, refs, format, concurrency, LFS -------------------------------------------------

func TestImportGates(t *testing.T) {
	mk := func(t *testing.T, mutate func(*config.Config)) (*Service, *Handler, string) {
		t.Helper()
		cfg := testConfig(t)
		mutate(cfg)
		svc, _ := testService(t, cfg, &FakeRoles{})
		h := testHandler(svc, adminPrincipal())
		remote := t.TempDir() + "/src"
		srcURL := fixtureRepo(t, remote, 5, 1, 1)
		repackSingle(t, remote)
		return svc, h, srcURL
	}
	start := func(t *testing.T, h *Handler, srcURL, name, extra string) string {
		t.Helper()
		w := doPost(t, h, "/api/v1/repos/imports", `{"source_url":`+q(srcURL)+`,"owner":"acme","name":`+q(name)+extra+`}`, "")
		if w.Code != 202 {
			t.Fatalf("POST = %d (%q)", w.Code, w.Body.String())
		}
		var started struct {
			Task map[string]any `json:"task"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &started)
		id, _ := started.Task["id"].(string)
		return id
	}
	t.Run("max_bytes 413", func(t *testing.T) {
		svc, h, srcURL := mk(t, func(c *config.Config) { c.Import.MaxBytes = 1 })
		o := awaitDone(t, svc, start(t, h, srcURL, "big", ""), 60*time.Second)
		if o.Err == nil || o.Err.Status != 413 || !strings.Contains(o.Err.Message, "import.max_bytes") {
			t.Fatalf("outcome = %+v, want 413 naming import.max_bytes", o)
		}
	})
	t.Run("max_refs 422", func(t *testing.T) {
		svc, h, srcURL := mk(t, func(c *config.Config) { c.Import.MaxRefs = 1 })
		o := awaitDone(t, svc, start(t, h, srcURL, "manyrefs", ""), 60*time.Second)
		if o.Err == nil || o.Err.Status != 422 || !strings.Contains(o.Err.Message, "import.max_refs") {
			t.Fatalf("outcome = %+v, want 422 naming import.max_refs", o)
		}
	})
	t.Run("format pin 422", func(t *testing.T) {
		svc, h, srcURL := mk(t, func(c *config.Config) {})
		o := awaitDone(t, svc, start(t, h, srcURL, "fmtpin", `,"format":"sha256"`), 60*time.Second)
		if o.Err == nil || o.Err.Status != 422 || !strings.Contains(o.Err.Message, "never convert") {
			t.Fatalf("outcome = %+v, want 422 never-convert", o)
		}
	})
	t.Run("max_concurrent 503", func(t *testing.T) {
		svc, h, srcURL := mk(t, func(c *config.Config) {})
		// Saturate the clone semaphore: the task fails loudly, never queues.
		svc.clones <- struct{}{}
		svc.clones <- struct{}{}
		defer func() { <-svc.clones; <-svc.clones }()
		o := awaitDone(t, svc, start(t, h, srcURL, "busy", ""), 60*time.Second)
		if o.Err == nil || o.Err.Status != 503 {
			t.Fatalf("outcome = %+v, want 503", o)
		}
	})
	t.Run("lfs notice", func(t *testing.T) {
		svc, h, _ := mk(t, func(c *config.Config) {})
		remote := t.TempDir() + "/lfs"
		srcURL := fixtureRepo(t, remote, 2, 0, 0)
		writeFile(t, remote+"/.gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
		gitAddCommit(t, remote, ".gitattributes", "lfs tracking")
		repackSingle(t, remote)
		id := start(t, h, srcURL, "lfsrepo", "")
		o := awaitDone(t, svc, id, 60*time.Second)
		if o.Err != nil {
			t.Fatalf("outcome error: %v", o.Err)
		}
		g := doGet(t, h, "/api/v1/repos/imports/"+id, "")
		if !strings.Contains(g.Body.String(), "pointer blobs are imported as-is") {
			t.Fatalf("LFS notice missing: %s", g.Body.String())
		}
	})
	t.Run("annotated tag peel", func(t *testing.T) {
		svc, h, srcURL := mk(t, func(c *config.Config) {})
		id := start(t, h, srcURL, "peel", "")
		o := awaitDone(t, svc, id, 60*time.Second)
		if o.Err != nil {
			t.Fatalf("outcome error: %v", o.Err)
		}
		sha, ok := o.HeadSHAs["refs/tags/v0"]
		if !ok || len(sha) != 40 {
			t.Fatalf("head_shas = %v", o.HeadSHAs)
		}
		// The tag sha must be the TAG object (peeled commit differs).
		peeled := gitOutput(t, srcURL, "rev-parse", "refs/tags/v0^{}")
		if sha == peeled {
			t.Fatalf("tag sha %s equals peeled commit; peel lost", sha)
		}
	})
}

// --- misc units ------------------------------------------------------------------------------------

func TestClassifyCloneError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		err    string
		status int
		part   string
	}{
		{name: "canceled", ctx: canceledCtx(), err: "signal killed", status: 503, part: "clone_timeout"},
		{name: "auth", ctx: context.Background(), err: "Authentication failed for https://x", status: 401, part: "token scope"},
		{name: "username prompt", ctx: context.Background(), err: "could not read Username", status: 401},
		{name: "not found", ctx: context.Background(), err: "Repository not found.", status: 422},
		{name: "other", ctx: context.Background(), err: "weird git failure", status: 502, part: "weird git failure"},
		{name: "scrubbed", ctx: context.Background(), err: "password=hunter2 nope", status: 502, part: "[redacted]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			se := classifyCloneError(tc.ctx, errTest(tc.err), "30m0s")
			if se.Status != tc.status {
				t.Fatalf("status = %d, want %d (%q)", se.Status, tc.status, se.Message)
			}
			if tc.part != "" && !strings.Contains(se.Message, tc.part) {
				t.Fatalf("message %q lacks %q", se.Message, tc.part)
			}
		})
	}
}

func canceledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

type errTest string

func (e errTest) Error() string { return string(e) }

func TestRegisterKindPanics(t *testing.T) {
	RegisterKind("repoimport-test-kind")
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("duplicate kind must panic")
		}
	}()
	RegisterKind("repoimport-test-kind")
}

func TestJanitor(t *testing.T) {
	svc, _ := testService(t, nil, &FakeRoles{})
	st := newStream()
	st.target = "acme/old"
	st.finish(&Outcome{Repo: "acme/old", HeadSHAs: map[string]string{}})
	st.finished = st.finished.Add(-2 * time.Hour)
	svc.mu.Lock()
	svc.streams["i-old"] = st
	svc.mu.Unlock()
	svc.Janitor()
	svc.mu.Lock()
	_, ok := svc.streams["i-old"]
	svc.mu.Unlock()
	if ok {
		t.Fatalf("expired stream not pruned")
	}
}

func TestDriveNilRegistry(t *testing.T) {
	cfg := testConfig(t)
	svc := New(Deps{Store: store.NewMemory(), Roles: &FakeRoles{}, Cfg: cfg})
	st := newStream()
	st.target = "acme/nil"
	r := &running{id: "i-nil", params: map[string]string{}, done: make(chan struct{})}
	svc.drive("acme/nil", "i-nil", fileParams("acme", "nil", "file:///x"), "", st, r)
	_, outcome, done := st.snapshot()
	if !done || outcome.Err == nil || outcome.Err.Status != 503 {
		t.Fatalf("outcome = %+v done=%v", outcome, done)
	}
}

func TestTableRecordFallback(t *testing.T) {
	// A table record with no import stream serves JSON (pruned-window rule).
	svc, _ := testService(t, nil, &FakeRoles{})
	h := testHandler(svc, adminPrincipal())
	rec, err := svc.reg.Tasks().Run(context.Background(), "acme/widget", "sync", nil, func(ctx context.Context, task *wal.Task) error {
		task.Notice("hello")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	g := doGet(t, h, "/api/v1/repos/imports/"+rec.ID, "")
	if g.Code != 200 {
		t.Fatalf("GET table record = %d (%q)", g.Code, g.Body.String())
	}
	if !strings.Contains(g.Body.String(), "hello") {
		t.Fatalf("record body = %s", g.Body.String())
	}
}

func TestRunHeadless(t *testing.T) {
	cfg := testConfig(t)
	svc, st := testService(t, cfg, &FakeRoles{})
	remote := t.TempDir() + "/src"
	srcURL := fixtureRepo(t, remote, 4, 0, 0)
	repackSingle(t, remote)
	p := fileParams("acme", "cli", srcURL)
	o, err := svc.RunHeadless(context.Background(), p, "", "op")
	if err != nil {
		t.Fatalf("headless: %v", err)
	}
	if o.Repo != "acme/cli" || len(o.HeadSHAs) != 1 {
		t.Fatalf("outcome = %+v", o)
	}
	doc, _, err := readImportDoc(context.Background(), st, "acme", "cli")
	if err != nil || doc.Importer != "op" {
		t.Fatalf("doc = %+v err=%v", doc, err)
	}
	// Headless failure path (missing source).
	p2 := fileParams("acme", "cli2", "file:///srv/missing.git")
	if _, err := svc.RunHeadless(context.Background(), p2, "", ""); err == nil {
		t.Fatalf("expected headless error")
	}
}
