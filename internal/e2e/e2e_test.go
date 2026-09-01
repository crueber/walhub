package e2e

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

// testOwner is the owner segment used for every e2e repo. With auth mode
// none, any owner name is writable.
const testOwner = "e2e"

// TestMain cleans up the shared build directory after the whole run.
func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// newServer builds the binary and boots a fresh defaults-mode server on its
// own data dir and port; both are cleaned up by t.Cleanup.
func newServer(t *testing.T) *e2eServer {
	t.Helper()
	bin := buildWALHub(t)
	dataDir := t.TempDir()
	s := startServer(t, bin, dataDir, freePort(t))
	t.Cleanup(s.stop)
	return s
}

// pushFreshRepo creates owner/repo from scratch (the push auto-creates the
// repository on the server) with one committed file, and returns the HEAD sha.
func pushFreshRepo(t *testing.T, g *gitClient, s *e2eServer, repo, file, content string) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "seed")
	g.initRepo(dir)
	sha := g.commitFile(dir, file, content, "seed commit")
	url := s.gitURL(testOwner, repo)
	g.run(dir, "remote", "add", "origin", url)
	g.run(dir, "push", "-u", "origin", "main")
	return dir, sha
}

// ---- (a) push → clone → fetch round trip ---------------------------------------

func TestE2E_PushCloneFetchRoundTrip(t *testing.T) {
	requireModernGit(t)
	g := newGitClient(t)
	s := newServer(t)
	repo := uniqueRepo("roundtrip")
	url := s.gitURL(testOwner, repo)

	// Seed repo: commit + push main.
	dir1 := filepath.Join(t.TempDir(), "origin")
	g.initRepo(dir1)
	sha1 := g.commitFile(dir1, "hello.txt", "hello walhub\n", "first commit")
	g.run(dir1, "remote", "add", "origin", url)
	g.run(dir1, "push", "-u", "origin", "main")

	refs := g.lsRemote(url)
	if got := refs["refs/heads/main"]; got != sha1 {
		t.Fatalf("ls-remote refs/heads/main = %q, want pushed sha %q", got, sha1)
	}

	// Clone into a second working tree and verify the content round-trips.
	dir2 := filepath.Join(t.TempDir(), "clone")
	g.run(filepath.Dir(dir2), "clone", url, dir2)
	got, err := os.ReadFile(filepath.Join(dir2, "hello.txt"))
	if err != nil {
		t.Fatalf("cloned file missing: %v", err)
	}
	if string(got) != "hello walhub\n" {
		t.Fatalf("cloned content = %q, want %q", got, "hello walhub\n")
	}

	// Push a second branch from the clone.
	g.run(dir2, "checkout", "-b", "feature")
	sha2 := g.commitFile(dir2, "feature.txt", "from clone\n", "feature commit")
	g.run(dir2, "push", "origin", "feature")
	refs = g.lsRemote(url)
	if got := refs["refs/heads/feature"]; got != sha2 {
		t.Fatalf("ls-remote refs/heads/feature = %q, want %q", got, sha2)
	}

	// Fetch the new branch into the first repo.
	g.run(dir1, "fetch", "origin")
	if got := g.run(dir1, "rev-parse", "refs/remotes/origin/feature"); got != sha2 {
		t.Fatalf("fetched origin/feature = %q, want %q", got, sha2)
	}

	// Push a tag, then delete it.
	g.run(dir1, "tag", "v1", sha1)
	g.run(dir1, "push", "origin", "refs/tags/v1")
	tags := g.lsRemote(url, "--tags")
	if got := tags["refs/tags/v1"]; got != sha1 {
		t.Fatalf("ls-remote refs/tags/v1 = %q, want %q", got, sha1)
	}
	g.run(dir1, "push", "origin", ":refs/tags/v1")
	tags = g.lsRemote(url, "--tags")
	if got, ok := tags["refs/tags/v1"]; ok {
		t.Fatalf("tag v1 still present after delete: %s", got)
	}
}

// ---- (b) JSON API reads reflect the push ---------------------------------------

type apiRef struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
}

type apiTreeEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

type apiCommit struct {
	SHA     string   `json:"sha"`
	Parents []string `json:"parents"`
	Message string   `json:"message,omitempty"`
}

func TestE2E_JSONAPIReflectsPush(t *testing.T) {
	requireModernGit(t)
	g := newGitClient(t)
	s := newServer(t)
	repo := uniqueRepo("api")
	const content = "the api must see this\n"
	_, sha := pushFreshRepo(t, g, s, repo, "hello.txt", content)

	// Summary: head sha == pushed sha.
	var summary struct {
		FullName string  `json:"full_name"`
		Head     *apiRef `json:"head"`
	}
	getJSON(t, lane(s, repo, ""), &summary)
	if summary.Head == nil || summary.Head.SHA != sha {
		t.Fatalf("summary head = %+v, want sha %q", summary.Head, sha)
	}

	// Refs page contains the pushed branch.
	var refPage struct {
		Refs []apiRef `json:"refs"`
	}
	getJSON(t, lane(s, repo, "refs/branches"), &refPage)
	found := false
	for _, r := range refPage.Refs {
		if r.Name == "refs/heads/main" && r.SHA == sha {
			found = true
		}
	}
	if !found {
		t.Fatalf("refs/branches = %+v, want refs/heads/main at %q", refPage.Refs, sha)
	}

	// Resolve main → the pushed commit.
	var resolve struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Kind string `json:"kind"`
	}
	getJSON(t, lane(s, repo, "resolve/main"), &resolve)
	if resolve.SHA != sha {
		t.Fatalf("resolve/main sha = %q, want %q", resolve.SHA, sha)
	}

	// Tree at main lists the pushed file.
	var tree struct {
		Entries []apiTreeEntry `json:"entries"`
	}
	getJSON(t, lane(s, repo, "tree/main"), &tree)
	found = false
	for _, e := range tree.Entries {
		if e.Name == "hello.txt" && e.Type == "blob" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tree/main entries = %+v, want hello.txt blob", tree.Entries)
	}

	// Blob content matches the push.
	var blob struct {
		Contents string `json:"contents"`
	}
	getJSON(t, lane(s, repo, "blob/main/hello.txt"), &blob)
	if blob.Contents != content {
		t.Fatalf("blob contents = %q, want %q", blob.Contents, content)
	}

	// Commits page and single commit.
	var commits struct {
		SHA     string      `json:"sha"`
		Commits []apiCommit `json:"commits"`
	}
	getJSON(t, lane(s, repo, "commits?ref=main"), &commits)
	if len(commits.Commits) == 0 || commits.Commits[0].SHA != sha {
		t.Fatalf("commits[0] = %+v, want head sha %q", commits.Commits, sha)
	}
	var detail struct {
		Commit apiCommit `json:"commit"`
	}
	getJSON(t, lane(s, repo, "commit/"+sha), &detail)
	if detail.Commit.SHA != sha {
		t.Fatalf("commit/%s detail sha = %q", sha, detail.Commit.SHA)
	}
}

// lane builds a repo-lane API URL; empty sub selects the summary endpoint.
func lane(s *e2eServer, repo, sub string) string {
	base := s.base + "/" + testOwner + "/" + repo + "/api"
	if sub == "" {
		return base
	}
	return base + "/" + sub
}

// ---- (c) no-config boot defaults ------------------------------------------------

func TestE2E_BootDefaults(t *testing.T) {
	s := newServer(t)

	var health struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	getJSON(t, s.base+"/healthz", &health)
	if health.Status != "ok" {
		t.Fatalf("healthz status = %q, want ok", health.Status)
	}

	var ready map[string]any
	getJSON(t, s.base+"/readyz", &ready)
	if ready["status"] != "ready" {
		t.Fatalf("readyz status = %v, want ready", ready["status"])
	}
	if ready["config"] != "defaults" {
		t.Fatalf("readyz config = %v, want \"defaults\"", ready["config"])
	}

	if _, err := os.Stat(filepath.Join(s.dataDir, "walhub.toml")); !os.IsNotExist(err) {
		t.Fatalf("walhub.toml exists after defaults boot (err=%v)", err)
	}
}

// ---- (d) invalid config → setup-only mode ---------------------------------------

func TestE2E_InvalidConfigSetupOnly(t *testing.T) {
	bin := buildWALHub(t)
	dataDir := t.TempDir()
	writeWALHubConfig(t, dataDir, "bogus_key = 1\n")
	s := startServer(t, bin, dataDir, freePort(t))
	t.Cleanup(s.stop)

	// healthz stays 200 (up but unconfigured).
	var health map[string]any
	getJSON(t, s.base+"/healthz", &health)

	// /readyz reports setup_required with 503.
	status, body := httpGet(s.base+"/readyz", 5*time.Second)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503 (body %s)", status, body)
	}
	if !jsonHasKey(body, "status", "setup_required") {
		t.Fatalf("readyz body = %s, want status setup_required", body)
	}

	// /setup serves (200); everything else 503.
	if status, body = httpGet(s.base+"/setup", 5*time.Second); status != http.StatusOK {
		t.Fatalf("GET /setup = %d, want 200 (body %s)", status, body)
	}
	if status, body = httpGet(s.base+"/api/v1/owners", 5*time.Second); status != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/v1/owners = %d, want 503 (body %s)", status, body)
	}
	infoRefs := s.gitURL(testOwner, uniqueRepo("blocked")) + "/info/refs?service=git-upload-pack"
	if status, body = httpGet(infoRefs, 5*time.Second); status != http.StatusServiceUnavailable {
		t.Fatalf("GET git info/refs = %d, want 503 (body %s)", status, body)
	}

	// GET /api/v1/setup reports the file state and the validation errors.
	var setup struct {
		FileState string `json:"file_state"`
		Errors    []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	getJSON(t, s.base+"/api/v1/setup", &setup)
	if setup.FileState != "invalid" {
		t.Fatalf("setup file_state = %q, want \"invalid\"", setup.FileState)
	}
	if len(setup.Errors) == 0 {
		t.Fatalf("setup errors empty, want the unknown-key validation error")
	}
}

// jsonHasKey reports whether body is JSON with obj[key] == want.
func jsonHasKey(body []byte, key, want string) bool {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	v, _ := m[key].(string)
	return v == want
}

// ---- (e) setup save → restart → normal ------------------------------------------

func TestE2E_SetupSaveRestart(t *testing.T) {
	bin := buildWALHub(t)
	dataDir := t.TempDir()
	writeWALHubConfig(t, dataDir, "bogus_key = 1\n")
	port := freePort(t)
	s := startServer(t, bin, dataDir, port)
	t.Cleanup(s.stop)

	// The setup API is open in setup-only mode and reports the bad file.
	var setup struct {
		FileState string `json:"file_state"`
	}
	getJSON(t, s.base+"/api/v1/setup", &setup)
	if setup.FileState != "invalid" {
		t.Fatalf("file_state = %q, want \"invalid\"", setup.FileState)
	}

	// PUT a valid config: server.listen/store/cache rooted in this data dir,
	// filesystem backend, auto-create on push (first-run default we want kept).
	overrides := map[string]any{
		"server.listen":              fmt.Sprintf("0.0.0.0:%d", port),
		"server.auto_create_on_push": true,
		"store.backend":              "filesystem",
		"store.root":                 filepath.Join(dataDir, "store"),
		"cache.dir":                  filepath.Join(dataDir, "cache"),
	}
	payload, _ := json.Marshal(map[string]any{"overrides": overrides})
	status, body := httpRequest(t, http.MethodPut, s.base+"/api/v1/setup", payload)
	if status < 200 || status > 299 {
		t.Fatalf("PUT /api/v1/setup = %d, want 2xx (body %s)", status, body)
	}

	// The file now exists and is byte-valid TOML.
	cfgPath := filepath.Join(dataDir, "walhub.toml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("saved config missing: %v", err)
	}
	var decoded map[string]any
	if _, perr := toml.Decode(string(raw), &decoded); perr != nil {
		t.Fatalf("saved config is not valid TOML: %v\n%s", perr, raw)
	}

	// Restart on the same data dir and port: normal mode, push works.
	s.stop()
	s2 := startServer(t, bin, dataDir, port)
	t.Cleanup(s2.stop)

	var ready map[string]any
	getJSON(t, s2.base+"/readyz", &ready)
	if ready["status"] != "ready" {
		t.Fatalf("readyz after restart = %v, want \"ready\" (not setup_required)", ready["status"])
	}

	g := newGitClient(t)
	repo := uniqueRepo("postrestart")
	_, sha := pushFreshRepo(t, g, s2, repo, "hello.txt", "after restart\n")
	refs := g.lsRemote(s2.gitURL(testOwner, repo))
	if got := refs["refs/heads/main"]; got != sha {
		t.Fatalf("after restart ls-remote refs/heads/main = %q, want %q", got, sha)
	}
}

// ---- (f) events webhook delivery --------------------------------------------------

// delivery is one webhook POST captured by the test sink.
type delivery struct {
	Body    []byte
	Sig     string
	DelivID string
}

func TestE2E_EventsWebhookDelivery(t *testing.T) {
	requireModernGit(t)
	g := newGitClient(t)
	bin := buildWALHub(t)
	dataDir := t.TempDir()

	deliveries := make(chan delivery, 16)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readLimited(r)
		d := delivery{Body: body}
		if v := r.Header.Get("X-Walgit-Signature"); v != "" {
			d.Sig = v
		}
		if v := r.Header.Get("X-Walgit-Delivery"); v != "" {
			d.DelivID = v
		}
		w.WriteHeader(http.StatusOK)
		select {
		case deliveries <- d:
		default:
		}
	}))
	defer sink.Close()

	const secret = "e2e-webhook-secret"
	writeWALHubConfig(t, dataDir, fmt.Sprintf(`
[server]
auto_create_on_push = true

[store]
backend = "filesystem"
root = %q

[cache]
dir = %q

[events]
webhook_url = %q
webhook_secret = %q
sweep_interval = "1s"
`, filepath.Join(dataDir, "store"), filepath.Join(dataDir, "cache"), sink.URL, secret))

	s := startServer(t, bin, dataDir, freePort(t))
	t.Cleanup(s.stop)

	// Push: create event for refs/heads/main must reach the sink within ~10s
	// (1s sweep backstop) even without a notify wake.
	repo := uniqueRepo("events")
	url := s.gitURL(testOwner, repo)
	dir := filepath.Join(t.TempDir(), "seed")
	g.initRepo(dir)
	sha := g.commitFile(dir, "hello.txt", "trigger events\n", "seed commit")
	g.run(dir, "remote", "add", "origin", url)
	g.run(dir, "push", "-u", "origin", "main")

	d := awaitDelivery(t, deliveries, 10*time.Second, "push of main")
	verifySignature(t, d, secret)
	var events []refEvent
	if err := json.Unmarshal(d.Body, &events); err != nil {
		t.Fatalf("webhook body is not a JSON array: %v\nbody: %s", err, d.Body)
	}
	zero := zeroOID(40)
	wantRef := "refs/heads/main"
	var create *refEvent
	for i := range events {
		e := events[i]
		if e.RefName == wantRef && e.Action == "create" {
			create = &events[i]
		}
	}
	if create == nil {
		t.Fatalf("no create event for %s in %s", wantRef, d.Body)
	}
	if create.RefType != "branch" {
		t.Fatalf("ref_type = %q, want \"branch\"", create.RefType)
	}
	if create.New != sha {
		t.Fatalf("event new = %q, want pushed sha %q", create.New, sha)
	}
	if create.Old != zero {
		t.Fatalf("event old = %q, want zero OID %q", create.Old, zero)
	}
	if create.Repo != testOwner+"/"+repo {
		t.Fatalf("event repo = %q, want %q", create.Repo, testOwner+"/"+repo)
	}
	if create.Walgit.Seq == "" {
		t.Fatalf("_walgit.seq = %q, want a decimal string", create.Walgit.Seq)
	}

	// Second push, woken immediately via POST /_events/notify.
	sha2 := g.commitFile(dir, "hello.txt", "second revision\n", "update commit")
	g.run(dir, "push", "origin", "main")
	status, body := httpPost(t, s.base+"/_events/notify", []byte(`{"repo":"`+testOwner+`/`+repo+`"}`))
	if status < 200 || status > 299 {
		t.Fatalf("POST /_events/notify = %d, want 2xx (body %s)", status, body)
	}

	d = awaitDelivery(t, deliveries, 10*time.Second, "update of main")
	var batch []refEvent
	if err := json.Unmarshal(d.Body, &batch); err != nil {
		t.Fatalf("webhook body is not a JSON array: %v\nbody: %s", err, d.Body)
	}
	var update *refEvent
	for i := range batch {
		if batch[i].RefName == wantRef && batch[i].Action == "update" && batch[i].New == sha2 {
			update = &batch[i]
		}
	}
	if update == nil {
		t.Fatalf("no update event for %s at %q in %s", wantRef, sha2, d.Body)
	}
}

// refEvent mirrors the internal/events.RefEvent wire shape (09 §3); kept
// locally so this package stays decoupled from internal/.
type refEvent struct {
	Action        string  `json:"action"`
	RefType       string  `json:"ref_type"`
	RefName       string  `json:"ref_name"`
	Old           string  `json:"old"`
	New           string  `json:"new"`
	Pusher        string  `json:"pusher"`
	CorrelationID string  `json:"correlation_id"`
	Repo          string  `json:"repo"`
	Walgit        walgitE `json:"_walgit"`
}

type walgitE struct {
	SchemaVersion int    `json:"schema_version"`
	Seq           string `json:"seq"`
	EntryKind     string `json:"entry_kind"`
	RequestID     string `json:"request_id"`
}

// zeroOID returns the all-zero hex OID of the given length.
func zeroOID(hexLen int) string {
	return hex.EncodeToString(make([]byte, hexLen/2))
}

func awaitDelivery(t *testing.T, ch <-chan delivery, timeout time.Duration, what string) delivery {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case d := <-ch:
			return d
		case <-deadline:
			t.Fatalf("no webhook delivery for %s within %v", what, timeout)
		}
	}
}

// verifySignature checks X-Walgit-Signature ("sha256=" + hex HMAC-SHA256 of
// the body) and X-Walgit-Delivery (hex SHA-1 of the body).
func verifySignature(t *testing.T, d delivery, secret string) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(d.Body)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); d.Sig != want {
		t.Fatalf("X-Walgit-Signature = %q, want %q", d.Sig, want)
	}
	sum := sha1.Sum(d.Body)
	if want := hex.EncodeToString(sum[:]); d.DelivID != want {
		t.Fatalf("X-Walgit-Delivery = %q, want %q", d.DelivID, want)
	}
}

// writeWALHubConfig writes <dataDir>/walhub.toml with the given content.
func writeWALHubConfig(t *testing.T, dataDir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dataDir, "walhub.toml"), []byte(content), 0o600); err != nil {
		t.Fatalf("write walhub.toml: %v", err)
	}
}

func readLimited(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 1<<20))
}
