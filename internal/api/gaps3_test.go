package api

// gaps3_test.go: last error arms — publish failures through the settings
// handlers, render-cache miss arms, dispatch conventions, and the pure
// render helpers' edge branches.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

// failingPublish fails PublishSettings on demand.
type failingPublish struct {
	failView
}

// publishFixture reuses the fail fixture without copying the embedded mutex.
func publishFixture(t *testing.T) (*fixture, *failingPublish) {
	t.Helper()
	f, fv := newFailFixture(t)
	fp := &failingPublish{failView: failView{fail: fv.fail}}
	fp.fakeView.resolves = f.view.resolves
	fp.fakeView.heads = f.view.heads
	fp.fakeView.lists = f.view.lists
	fp.fakeView.more = f.view.more
	fp.fakeView.trees = f.view.trees
	fp.fakeView.blobs = f.view.blobs
	fp.fakeView.blobRaw = f.view.blobRaw
	fp.fakeView.commitPg = f.view.commitPg
	fp.fakeView.commits = f.view.commits
	fp.fakeView.summaries = f.view.summaries
	fp.fakeView.overviews = f.view.overviews
	fp.fakeView.settings = f.view.settings
	fp.fakeView.history = f.view.history
	fp.fakeView.headSeq = f.view.headSeq
	fp.fakeView.pushes = f.view.pushes
	fp.fakeView.published = f.view.published
	fp.fakeView.synced = f.view.synced
	f.env.Repo = fp
	return f, fp
}

func (f *failingPublish) PublishSettings(ctx context.Context, id git.RepoId, body []byte, msg, author string) (uint64, error) {
	if err := f.err("PublishSettings"); err != nil {
		return 0, err
	}
	return f.fakeView.PublishSettings(ctx, id, body, msg, author)
}

func TestSettingsPublishFailures(t *testing.T) {
	// PUT settings with a failing publish → 503
	f, fp := publishFixture(t)
	fp.fail["PublishSettings"] = errBoom
	if w := f.req("PUT", "/demo/walgit/api/settings", strings.NewReader("[bundles]\nmain_only = false\n")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT publish fail = %d", w.Code)
	}
	// DELETE settings with a failing publish → 503
	if w := f.req("DELETE", "/demo/walgit/api/settings"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("DELETE publish fail = %d", w.Code)
	}
}

func TestRenderMissArms(t *testing.T) {
	f := newFixture(t)
	f.view.resolves["demo/walgit/main"] = Resolution{Ref: "refs/heads/main", SHA: fakeSHA, Kind: "branch", Revision: 7}
	// JSON blob render with a missing blob → 404 (render error arm)
	if w := f.req("GET", "/demo/walgit/api/blob/main/absent.txt"); w.Code != http.StatusNotFound {
		t.Fatalf("missing blob render = %d", w.Code)
	}
	// raw blob with a missing blob → 404 (raw error arm)
	if w := f.req("GET", "/demo/walgit/api/blob/main/absent.txt?raw"); w.Code != http.StatusNotFound {
		t.Fatalf("missing raw = %d", w.Code)
	}
	// commits query guards
	if w := f.req("GET", "/demo/walgit/api/commits?skip=-1"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad skip = %d", w.Code)
	}
	if w := f.req("GET", "/demo/walgit/api/commits?n=0"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad n = %d", w.Code)
	}
	// tree render miss
	if w := f.req("GET", "/demo/walgit/api/tree/main"); w.Code != http.StatusNotFound {
		t.Fatalf("tree miss = %d", w.Code)
	}
}

func TestDispatchConventions(t *testing.T) {
	f := newFixture(t)
	// wrong method on a repo lane → 405
	if w := f.req("POST", "/demo/walgit/api"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST lane root = %d, want 405", w.Code)
	}
	// .git suffix is stripped
	f.view.summaries["demo/walgit"] = SummaryData{Branches: 1}
	if w := f.req("GET", "/demo/walgit.git/api"); w.Code != 200 {
		t.Fatalf(".git strip = %d", w.Code)
	}
	// junk repo id → 404 (invalid owner characters)
	if w := f.req("GET", "/a~b/repo/api"); w.Code != http.StatusNotFound {
		t.Fatalf("bad id = %d", w.Code)
	}
}

func TestGateAnonymousWriteArms(t *testing.T) {
	f := newFixture(t)
	anon := &auth.Principal{Anonymous: true}
	// anonymous read allowed (anonRead=true)
	if w := f.do("GET", "/demo/walgit/api", nil, nil, anon); w.Code == http.StatusUnauthorized {
		t.Fatal("anonymous read must pass with anonRead on")
	}
	// anonymous write → 403 (never 401, anonRead is on)
	if w := f.do("PUT", "/demo/walgit/api", nil, nil, anon); w.Code != http.StatusForbidden {
		t.Fatalf("anon write = %d, want 403", w.Code)
	}
	// anonymous admin → 403
	if w := f.do("DELETE", "/demo/walgit/api", nil, nil, anon); w.Code != http.StatusForbidden {
		t.Fatalf("anon admin = %d, want 403", w.Code)
	}
	// with anonRead off, anonymous write/admin → 401
	f.env.Cfg.Server.Auth.AnonymousRead = false
	if w := f.do("PUT", "/demo/walgit/api", nil, nil, anon); w.Code != http.StatusUnauthorized {
		t.Fatalf("anon write 401 = %d", w.Code)
	}
	if w := f.do("DELETE", "/demo/walgit/api", nil, nil, anon); w.Code != http.StatusUnauthorized {
		t.Fatalf("anon admin 401 = %d", w.Code)
	}
}

func TestRenderHelperEdgeArms(t *testing.T) {
	if !parseRFC3339("").IsZero() || parseRFC3339("2026-09-01T00:00:00Z").IsZero() {
		t.Fatal("parseRFC3339 broken")
	}
	if dowName(0) == "" || dowName(6) == "" {
		t.Fatal("dowName broken")
	}
	// malformed log records are skipped, not fatal
	if recs := parseLogRecords([]byte("junk\x00\x00\x00\x00\x00\x00\x00\x00\x1e")); len(recs) != 0 {
		t.Fatalf("junk records = %+v", recs)
	}
	if isTrailerLine("not a trailer") || !isTrailerLine("Signed-off-by: a <a@b>") {
		t.Fatal("isTrailerLine broken")
	}
	if isNumField("-") != true || isNumField("x") || numOrNeg1("x") != -1 {
		t.Fatal("num arms broken")
	}
	// cron guards: a short spec fails, garbage never schedules
	if _, ok := cronField("* * *", 0, 6, map[string]int{"sun": 0}); ok {
		t.Fatal("short cron must fail")
	}
	if _, ok := cronNext("garbage", time.Now()); ok {
		t.Fatal("garbage cron must not schedule")
	}
	// malformed show record → not ok
	if _, ok := parseShowRecord([]byte("nonsense")); ok {
		t.Fatal("junk show record must not parse")
	}
}

func TestPacketJSONTaskShape(t *testing.T) {
	rec := TaskRecord{ID: "t", Kind: "gc"}
	got := packetJSON(Progress{Kind: "task", Task: &rec})
	if !strings.Contains(got, `"id":"t"`) {
		t.Fatalf("task packet = %s", got)
	}
}

func TestGitCmdEmptyBinaryFallsBack(t *testing.T) {
	v := &walView{}
	repo := &git.LocalRepo{Path: t.TempDir()}
	out, err := v.gitCmd(t.Context(), repo, "version")
	if err != nil || !strings.Contains(string(out), "git version") {
		t.Fatalf("gitCmd fallback = %q, %v", out, err)
	}
	if _, err := v.gitCmd(t.Context(), repo, "rev-parse", "--verify", "nope^{commit}"); err == nil {
		t.Fatal("failing git command must error")
	}
}

func TestBundleStrategiesNilCfg(t *testing.T) {
	f := newFixture(t)
	f.env.Cfg = nil
	if got := bundleStrategies(&handlers{env: f.env}, httptest.NewRequest("GET", "/x", nil)); got == nil || len(got) != 0 {
		t.Fatalf("nil cfg strategies = %v", got)
	}
}

func TestSettingsValidateNoBodyArms(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	// no body + broken stored TOML → ok:false
	f.view.published["demo/walgit"] = []byte("[compaction]\ntrigger_packs = \"x\"\n")
	w := f.req("POST", "/demo/walgit/api/settings/validate")
	var bad struct {
		OK     bool     `json:"ok"`
		Errors []string `json:"errors"`
	}
	decodeJSON(t, w, &bad)
	if bad.OK || len(bad.Errors) == 0 {
		t.Fatalf("broken stored validate = %+v", bad)
	}
	// validate against an invalid override value → ok:false
	w = f.req("POST", "/demo/walgit/api/settings/validate", strings.NewReader("[compaction]\ntrigger_packs = \"x\"\n"))
	decodeJSON(t, w, &bad)
	if bad.OK || len(bad.Errors) == 0 {
		t.Fatalf("invalid value validate = %+v", bad)
	}
}

func TestEffectiveTokenEnvDropped(t *testing.T) {
	f := newFixture(t)
	f.env.Cfg.Upstream.TokenEnv = "SECRET_VAR"
	f.view.published["demo/walgit"] = []byte("[compaction]\ntrigger_packs = 7\n")
	w := f.req("GET", "/demo/walgit/api/settings/effective")
	if w.Code != 200 {
		t.Fatalf("effective = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "SECRET_VAR") {
		t.Fatalf("token_env leaked: %s", w.Body.String())
	}
}
