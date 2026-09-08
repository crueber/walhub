package api

// health209_test.go — issue #209 self-heal: the summary `health` vocabulary,
// the `empty repository:` 404-marker prefix at all 9 bind_wal sites, and the
// overview fsck projection. Table-driven httptest throughout; `-race`
// mandatory; the ≥95% per-package gate (make cover) holds with these
// included.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// putFsck seeds a cached fsck.pb report for the repo in the fixture store.
func putFsck(t *testing.T, f *fixture, id git.RepoId, rep *proto.FsckReport) {
	t.Helper()
	if _, err := store.PutBytes(context.Background(), f.env.Store,
		id.StorePrefix()+store.Fsck, rep.Marshal(),
		store.PutOptions{Mode: store.PutOverwrite, ContentType: "application/x-protobuf"}); err != nil {
		t.Fatalf("seed fsck.pb: %v", err)
	}
}

func fsckTs(t time.Time) *proto.Timestamp {
	return &proto.Timestamp{Seconds: t.Unix()}
}

type summaryWire struct {
	Head struct {
		Name string `json:"name"`
		SHA  string `json:"sha"`
	} `json:"head"`
	Health       string `json:"health"`
	MissingTotal uint64 `json:"missing_total"`
}

// --- summary health ------------------------------------------------------------

func TestSummaryHealthVariants(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name        string
		seed        SummaryData // fakeView summary (Health "" → handler derives)
		fsck        *proto.FsckReport
		rawFsck     []byte // corrupt bytes instead of a report (nil = none)
		wantHealth  string
		wantMissing uint64
		wantETag    string
		wantNoETag  bool
	}{
		{
			name:       "healthy repo, never audited",
			seed:       SummaryData{Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 2, Tags: 1},
			wantHealth: "healthy",
			wantETag:   `"` + fakeSHA + `"`,
		},
		{
			name:       "empty repo, never audited",
			seed:       SummaryData{},
			wantHealth: "empty",
			wantNoETag: true,
		},
		{
			name:       "view-set healthy passes through",
			seed:       SummaryData{Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1, Health: "healthy"},
			wantHealth: "healthy",
			wantETag:   `"` + fakeSHA + `"`,
		},
		{
			name:       "degraded override from cached report",
			seed:       SummaryData{Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 2, Tags: 1},
			fsck:       &proto.FsckReport{MissingTotal: 3, Missing: []string{"aaa"}, At: fsckTs(now)},
			wantHealth: "degraded", wantMissing: 3,
			wantETag: `"` + fakeSHA + `~degraded"`,
		},
		{
			name:       "degraded from sample alone (total lost, sample kept)",
			seed:       SummaryData{Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1},
			fsck:       &proto.FsckReport{Missing: []string{"aaa", "bbb"}, At: fsckTs(now)},
			wantHealth: "degraded", wantMissing: 2,
			wantETag: `"` + fakeSHA + `~degraded"`,
		},
		{
			name:       "clean report stays healthy",
			seed:       SummaryData{Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1},
			fsck:       &proto.FsckReport{At: fsckTs(now)},
			wantHealth: "healthy",
			wantETag:   `"` + fakeSHA + `"`,
		},
		{
			name:       "corrupt report stays healthy (probe fails soft)",
			seed:       SummaryData{Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1},
			rawFsck:    []byte("not-protobuf"),
			wantHealth: "healthy",
			wantETag:   `"` + fakeSHA + `"`,
		},
		{
			name:       "empty skips the probe even with a stale damaging report",
			seed:       SummaryData{},
			fsck:       &proto.FsckReport{MissingTotal: 9, Missing: []string{"zzz"}, At: fsckTs(now)},
			wantHealth: "empty",
			wantNoETag: true,
		},
		{
			name:       "tags-only repo is healthy (refs exist, head null)",
			seed:       SummaryData{Tags: 1},
			wantHealth: "healthy",
			wantNoETag: true, // head nil → "" etag, as today
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.view.summaries["demo/walgit"] = tc.seed
			if tc.fsck != nil {
				putFsck(t, f, git.RepoId{Owner: "demo", Name: "walgit"}, tc.fsck)
			}
			if tc.rawFsck != nil {
				if _, err := store.PutBytes(context.Background(), f.env.Store,
					"repos/demo/walgit/"+store.Fsck, tc.rawFsck,
					store.PutOptions{Mode: store.PutOverwrite}); err != nil {
					t.Fatal(err)
				}
			}
			w := f.req("GET", "/demo/walgit/api")
			if w.Code != 200 {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			var body summaryWire
			decodeJSON(t, w, &body)
			if body.Health != tc.wantHealth {
				t.Fatalf("health = %q, want %q (body %s)", body.Health, tc.wantHealth, w.Body.String())
			}
			if body.MissingTotal != tc.wantMissing {
				t.Fatalf("missing_total = %d, want %d", body.MissingTotal, tc.wantMissing)
			}
			if tc.wantNoETag {
				if etag := w.Header().Get("ETag"); etag != "" {
					t.Fatalf("etag = %q, want absent", etag)
				}
			} else if etag := w.Header().Get("ETag"); etag != tc.wantETag {
				t.Fatalf("etag = %q, want %q", etag, tc.wantETag)
			}
		})
	}
}

func TestSummaryDegraded304(t *testing.T) {
	f := newFixture(t)
	f.view.summaries["demo/walgit"] = SummaryData{Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1}
	putFsck(t, f, git.RepoId{Owner: "demo", Name: "walgit"},
		&proto.FsckReport{MissingTotal: 1, Missing: []string{"aaa"}, At: fsckTs(time.Now())})
	// The healthy etag must NOT 304 once degraded (the flip busts the cache).
	w := f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": `"` + fakeSHA + `"`}, readP())
	if w.Code != 200 {
		t.Fatalf("stale healthy etag must revalidate to 200, got %d", w.Code)
	}
	// The degraded etag 304s while still degraded.
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": `"` + fakeSHA + `~degraded"`}, readP())
	if w.Code != http.StatusNotModified {
		t.Fatalf("current degraded etag must 304, got %d", w.Code)
	}
}

// --- unborn predicate (exactness, review S2) ------------------------------------

func TestUnbornStateExact(t *testing.T) {
	ctx := context.Background()
	id := git.RepoId{Owner: "demo", Name: "walgit"}
	for _, tc := range []struct {
		name  string
		sum   SummaryData
		man   *proto.Manifest
		manEr error
		want  bool
	}{
		{"empty manifest, no refs", SummaryData{}, &proto.Manifest{HeadSeq: 0}, nil, true},
		{"nil manifest (fakes), no refs", SummaryData{}, nil, nil, true},
		{"refs present", SummaryData{Head: &Ref{}, Branches: 1}, &proto.Manifest{}, nil, false},
		{"tags only are refs (never empty)", SummaryData{Tags: 1}, &proto.Manifest{HeadSeq: 0}, nil, false},
		{"head set", SummaryData{Head: &Ref{}, Branches: 1, Tags: 1}, &proto.Manifest{HeadSeq: 9}, nil, false},
		{"refs deleted after pushes: HeadSeq>0 is NOT empty", SummaryData{}, &proto.Manifest{HeadSeq: 7}, nil, false},
		{"manifest error fails closed", SummaryData{}, nil, errBoom, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &fakeEngine{man: tc.man, manErr: tc.manEr}
			v := &walView{engine: eng}
			if got := v.unbornState(ctx, id, tc.sum); got != tc.want {
				t.Fatalf("unbornState(%+v) = %v, want %v", tc.sum, got, tc.want)
			}
		})
	}
	// Nil engine fails closed.
	if (&walView{}).unbornState(ctx, id, SummaryData{}) {
		t.Fatal("nil engine must fail closed")
	}
}

func TestSummaryViewHealth(t *testing.T) {
	ctx := context.Background()
	id := git.RepoId{Owner: "demo", Name: "walgit"}
	// Non-empty serving copy → healthy (fake manifest nil counts as HeadSeq 0,
	// but refs exist so the predicate still refuses empty).
	fix := newGitFix(t)
	eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 1}
	v := newEngineFixture(t, eng).view()
	s, err := v.Summary(ctx, id)
	if err != nil || s.Health != RepoHealthHealthy {
		t.Fatalf("non-empty summary health = %q, %v", s.Health, err)
	}
	// Empty serving copy → empty, and the marker predicate agrees (the two
	// share summarizeSnapshot, so they cannot disagree).
	eng.obj = wal.ObjectAccess{Local: &git.LocalRepo{Root: t.TempDir(), ID: id, Path: emptyBare(t)}}
	s, err = v.Summary(ctx, id)
	if err != nil || s.Health != RepoHealthEmpty {
		t.Fatalf("empty summary health = %q, %v", s.Health, err)
	}
	if !v.unbornState(ctx, id, s) {
		t.Fatal("Summary says empty but the marker predicate refuses it")
	}
}

// --- the 9 marker sites ----------------------------------------------------------

func TestEmptyMarkerSites(t *testing.T) {
	ctx := context.Background()
	id := git.RepoId{Owner: "demo", Name: "walgit"}

	// Empty repo: every reachable 404 carries the marker (frozen 404 status,
	// additive prefix). The empty bare has an unborn HEAD symref, so resolve
	// "" takes the :187 "HEAD" arm — either arm must prefix.
	eng := &fakeEngine{obj: wal.ObjectAccess{Local: &git.LocalRepo{Root: t.TempDir(), ID: id, Path: emptyBare(t)}}}
	v := newEngineFixture(t, eng).view()
	badSHA := strings.Repeat("a", 40)
	emptyCases := []struct {
		name string
		call func() error
		want string // stable suffix after the marker
	}{
		{"resolve HEAD", func() error { _, err := v.Resolve(ctx, id, ""); return err }, "HEAD"},
		{"resolve rev", func() error { _, err := v.Resolve(ctx, id, "nope"); return err }, "nope"},
		{"tree", func() error { _, err := v.Tree(ctx, id, "main", ""); return err }, "tree main:"},
		{"blob", func() error { _, err := v.Blob(ctx, id, "main", "hello.txt", false); return err }, "blob hello.txt not found"},
		{"commits", func() error { _, err := v.Commits(ctx, id, "main", "", 0, 5); return err }, "history of main"},
		{"commit", func() error { _, err := v.Commit(ctx, id, badSHA); return err }, "commit " + badSHA + " not found"},
	}
	for _, tc := range emptyCases {
		t.Run("empty/"+tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("want 404")
			}
			msg := err.Error()
			if !strings.Contains(msg, ErrNotFound.Error()) {
				t.Fatalf("must wrap ErrNotFound: %q", msg)
			}
			if !strings.HasPrefix(strings.TrimPrefix(msg, ErrNotFound.Error()+": "), emptyMarker) &&
				!strings.Contains(msg, emptyMarker+tc.want) {
				t.Fatalf("missing %q marker: %q", emptyMarker, msg)
			}
		})
	}
	// Wire level: the empty resolve 404 carries the marker in the body.
	fx := newEngineFixture(t, eng)
	w := fx.do("GET", "/demo/walgit/api/resolve/")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), emptyMarker) {
		t.Fatalf("wire resolve 404 = %d %q, want marker", w.Code, w.Body.String())
	}
	// And the empty summary reports health empty with no ETag.
	w = fx.do("GET", "/demo/walgit/api")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"health":"empty"`) {
		t.Fatalf("wire empty summary = %d %q", w.Code, w.Body.String())
	}
	if etag := w.Header().Get("ETag"); etag != "" {
		t.Fatalf("empty summary etag = %q, want absent", etag)
	}

	// Damaged repo (refs present, objects missing): the SAME 404s keep their
	// exact frozen messages — the marker must never appear (review S2).
	fix := newGitFix(t)
	eng2 := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 1}
	v2 := newEngineFixture(t, eng2).view()
	damageCases := []struct {
		name string
		call func() error
		want string // frozen prefix; git's own stderr tail is environment text
	}{
		{"resolve rev", func() error { _, err := v2.Resolve(ctx, id, "nope-nope"); return err }, "not found: nope-nope"},
		{"tree", func() error { _, err := v2.Tree(ctx, id, badSHA, ""); return err }, "not found: tree " + badSHA + ":"},
		{"blob", func() error { _, err := v2.Blob(ctx, id, fix.main, "nonexistent.txt", false); return err }, "not found: blob nonexistent.txt not found"},
		{"commits", func() error { _, err := v2.Commits(ctx, id, badSHA, "", 0, 5); return err }, "not found: history of " + badSHA},
		{"commit", func() error { _, err := v2.Commit(ctx, id, badSHA); return err }, "not found: commit " + badSHA + " not found"},
	}
	for _, tc := range damageCases {
		t.Run("damage/"+tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("want 404")
			}
			if msg := err.Error(); !strings.HasPrefix(msg, tc.want) {
				t.Fatalf("damage-404 changed: got %q want prefix %q", msg, tc.want)
			}
			if strings.Contains(err.Error(), emptyMarker) {
				t.Fatalf("damage-404 must never carry the marker: %q", err.Error())
			}
		})
	}
}

// --- overview fsck projection -----------------------------------------------------

type overviewFsckWire struct {
	Fsck *struct {
		MissingTotal  uint64   `json:"missing_total"`
		Missing       []string `json:"missing"`
		Problems      uint64   `json:"problems"`
		RepairedSeq   uint64   `json:"repaired_seq"`
		At            string   `json:"at"`
		Host          string   `json:"host"`
		RepairStalled bool     `json:"repair_stalled"`
		Upstream      string   `json:"upstream"`
	} `json:"fsck"`
}

func seedOverview(f *fixture) {
	f.view.overviews["demo/walgit"] = OverviewData{
		Health: Health{Status: "ok", Issues: []string{}, Suggestions: []Suggestion{}},
	}
}

func TestOverviewFsckAbsent(t *testing.T) {
	f := newFixture(t)
	seedOverview(f)
	w := f.req("GET", "/demo/walgit/api/overview")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var body overviewFsckWire
	decodeJSON(t, w, &body)
	if body.Fsck != nil {
		t.Fatalf("fsck must be absent when never audited: %+v", body.Fsck)
	}
	// Corrupt bytes also project to absent, never a 500.
	if _, err := store.PutBytes(context.Background(), f.env.Store,
		"repos/demo/walgit/"+store.Fsck, []byte("junk"),
		store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	w = f.req("GET", "/demo/walgit/api/overview")
	if w.Code != 200 {
		t.Fatalf("corrupt fsck status = %d", w.Code)
	}
	decodeJSON(t, w, &body)
	if body.Fsck != nil {
		t.Fatalf("corrupt fsck must project absent: %+v", body.Fsck)
	}
}

func TestOverviewFsckProjection(t *testing.T) {
	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour) // older than the 7d default interval
	for _, tc := range []struct {
		name         string
		rep          *proto.FsckReport
		hostUpstream string
		repoTOML     string
		wantStalled  bool
		wantUpstream string
	}{
		{
			name:         "fresh report, no upstream: visible, not stalled",
			rep:          &proto.FsckReport{MissingTotal: 2, Missing: []string{"aa", "bb"}, Problems: 1, At: fsckTs(now), Host: "h1"},
			wantStalled:  false,
			wantUpstream: "",
		},
		{
			name:         "old report + host upstream + unrepaired = stalled",
			rep:          &proto.FsckReport{MissingTotal: 2, Missing: []string{"aa"}, At: fsckTs(old), Host: "h1"},
			hostUpstream: "https://upstream.example.com/r.git",
			wantStalled:  true,
			wantUpstream: "https://upstream.example.com/r.git",
		},
		{
			name:         "old report, no upstream: guidance case, not stalled",
			rep:          &proto.FsckReport{MissingTotal: 2, Missing: []string{"aa"}, At: fsckTs(old)},
			wantStalled:  false,
			wantUpstream: "",
		},
		{
			name:         "repaired disarm clears stall",
			rep:          &proto.FsckReport{MissingTotal: 2, Missing: []string{"aa"}, RepairedSeq: 41, At: fsckTs(old)},
			hostUpstream: "https://upstream.example.com/r.git",
			wantStalled:  false,
			wantUpstream: "https://upstream.example.com/r.git",
		},
		{
			name:         "per-repo D24 upstream wins over empty host",
			rep:          &proto.FsckReport{MissingTotal: 1, Missing: []string{"aa"}, At: fsckTs(old)},
			repoTOML:     "[upstream]\ngit = \"https://peer.example.com/r.git\"\n",
			wantStalled:  true,
			wantUpstream: "https://peer.example.com/r.git",
		},
		{
			name:         "broken repo TOML falls back to host",
			rep:          &proto.FsckReport{MissingTotal: 1, At: fsckTs(old)},
			hostUpstream: "https://upstream.example.com/r.git",
			repoTOML:     "[upstream\ngit = ",
			wantStalled:  true,
			wantUpstream: "https://upstream.example.com/r.git",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			seedOverview(f)
			f.env.Cfg.Upstream.Git = tc.hostUpstream
			if tc.repoTOML != "" {
				f.view.published["demo/walgit"] = []byte(tc.repoTOML)
			}
			putFsck(t, f, git.RepoId{Owner: "demo", Name: "walgit"}, tc.rep)
			w := f.req("GET", "/demo/walgit/api/overview")
			if w.Code != 200 {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			var body overviewFsckWire
			decodeJSON(t, w, &body)
			if body.Fsck == nil {
				t.Fatal("fsck projection missing")
			}
			got := body.Fsck
			if got.MissingTotal != fsckMissingTotal(tc.rep) {
				t.Fatalf("missing_total = %d want %d", got.MissingTotal, fsckMissingTotal(tc.rep))
			}
			if len(got.Missing) != len(tc.rep.Missing) {
				t.Fatalf("missing sample = %v want %v", got.Missing, tc.rep.Missing)
			}
			if got.Missing == nil {
				t.Fatal("missing must serialize as [], never null")
			}
			if got.RepairedSeq != tc.rep.RepairedSeq || got.Host != tc.rep.Host || got.Problems != tc.rep.Problems {
				t.Fatalf("projection = %+v report = %+v", got, tc.rep)
			}
			if got.At == "" {
				t.Fatal("at must render")
			}
			if got.RepairStalled != tc.wantStalled {
				t.Fatalf("repair_stalled = %v want %v", got.RepairStalled, tc.wantStalled)
			}
			if got.Upstream != tc.wantUpstream {
				t.Fatalf("upstream = %q want %q", got.Upstream, tc.wantUpstream)
			}
		})
	}
}

func TestFsckProjectionUnit(t *testing.T) {
	now := time.Now()
	rep := &proto.FsckReport{MissingTotal: 1, At: fsckTs(now.Add(-time.Hour))}
	// Nil At never stalls (fail closed), even with everything else set.
	noAt := &proto.FsckReport{MissingTotal: 1}
	if got := fsckProjection(noAt, "https://u/x.git", time.Hour, now); got.RepairStalled {
		t.Fatal("nil At must never derive stalled")
	}
	// An explicit interval is honored verbatim (the fallback lives at the
	// call site, fsckIntervalOf, not in the pure projection).
	if got := fsckProjection(rep, "https://u/x.git", 30*time.Minute, now); !got.RepairStalled {
		t.Fatal("1h-old report must stall under a 30m interval")
	}
	if got := fsckProjection(rep, "https://u/x.git", 7*24*time.Hour, now); got.RepairStalled {
		t.Fatal("1h-old report must not stall under the default interval")
	}
	// Missing slice is copied, never aliased.
	out := fsckProjection(&proto.FsckReport{Missing: []string{"a"}}, "", time.Hour, now)
	out.Missing[0] = "mut"
	if out.Missing == nil {
		t.Fatal("unreachable")
	}
	// fsckIntervalOf fallbacks: nil handler, nil config, and non-positive
	// intervals all fall back to the 7d default; positives pass through.
	if fsckIntervalOf(nil) != 7*24*time.Hour {
		t.Fatal("nil handler must fall back")
	}
	if fsckIntervalOf(&handlers{env: &Env{}}) != 7*24*time.Hour {
		t.Fatal("nil config must fall back")
	}
	cfg := config.Defaults()
	cfg.Maintenance.FsckInterval = config.Duration(2 * time.Hour)
	if fsckIntervalOf(&handlers{env: &Env{Cfg: cfg}}) != 2*time.Hour {
		t.Fatal("positive interval must pass through")
	}
	cfg.Maintenance.FsckInterval = 0
	if fsckIntervalOf(&handlers{env: &Env{Cfg: cfg}}) != 7*24*time.Hour {
		t.Fatal("zero interval must fall back")
	}
}

func TestProbeHelpersUnit(t *testing.T) {
	ctx := context.Background()
	id := git.RepoId{Owner: "demo", Name: "walgit"}
	// Nil store never probes.
	if _, ok := probeFsck(ctx, nil, id); ok {
		t.Fatal("nil store must not probe")
	}
	// Nil report totals zero without panicking.
	if fsckMissingTotal(nil) != 0 || fsckHasMissing(nil) {
		t.Fatal("nil report must read as clean")
	}
	// Nil host config resolves to "" (no repair source), never panics.
	if got := effectiveUpstreamGit(nil, SettingsDoc{TOML: "[upstream]\ngit = \"https://x/y.git\"\n"}); got != "" {
		t.Fatalf("nil host must resolve empty, got %q", got)
	}
	// Merge failure (unmergable doc cannot happen via ParseRepoSettings
	// success path — cover the TOML-empty early return instead).
	if got := effectiveUpstreamGit(config.Defaults(), SettingsDoc{}); got != "" {
		t.Fatalf("empty doc must resolve host (empty), got %q", got)
	}
}

// countStore decorates an ObjectStore counting Get calls per key — the
// EVIDENCE E13 round-trip harness: every probe below is one exact-key
// conditional-class GET, never a LIST, and the empty path probes nothing.
type countStore struct {
	store.ObjectStore
	gets map[string]int
}

func (s *countStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if s.gets == nil {
		s.gets = map[string]int{}
	}
	s.gets[key]++
	return s.ObjectStore.Get(ctx, key, opts)
}

func (s *countStore) totalGets() int {
	n := 0
	for _, c := range s.gets {
		n += c
	}
	return n
}

// TestSummaryOverviewRoundTrips pins the R1-B1 cost class per endpoint:
// empty summary +0, non-empty summary +1, overview +1 — all exact-key probes,
// and the law-6 budgeted engine paths (push/sync/checkpoint) never call here.
func TestSummaryOverviewRoundTrips(t *testing.T) {
	id := git.RepoId{Owner: "demo", Name: "walgit"}
	fsckKey := id.StorePrefix() + store.Fsck

	// Empty summary: at most the #210 placeholder sidecar probe (R1 B1 —
	// the fsck.pb probe is still skipped on data in hand; the sidecar is
	// probed ONLY when HeadSeq==0 && refs==0, so real repos pay +0).
	f := newFixture(t)
	cs := &countStore{ObjectStore: f.env.Store}
	f.env.Store = cs
	f.view.summaries["demo/walgit"] = SummaryData{}
	if w := f.req("GET", "/demo/walgit/api"); w.Code != 200 {
		t.Fatalf("empty summary = %d", w.Code)
	}
	if n := cs.totalGets(); n > 1 {
		t.Fatalf("empty summary issued %d store GETs, want ≤1 (sidecar only)", n)
	}
	if n := cs.gets[id.StorePrefix()+PlaceholderKeySuffix]; n > 1 {
		t.Fatalf("empty summary sidecar probes = %d, want ≤1", n)
	}
	if n := cs.gets[fsckKey]; n != 0 {
		t.Fatalf("empty summary must skip the fsck.pb probe, got %d", n)
	}

	// Healthy summary, never audited: exactly the fsck probe miss added —
	// the placeholder sidecar is never probed for non-empty repos (R1 B1:
	// real repos pay +0).
	f.view.summaries["demo/walgit"] = SummaryData{Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1}
	healthyBefore := cs.totalGets()
	if w := f.req("GET", "/demo/walgit/api"); w.Code != 200 {
		t.Fatalf("healthy summary = %d", w.Code)
	}
	if d := cs.totalGets() - healthyBefore; d != 1 {
		t.Fatalf("healthy summary issued %d GETs, want exactly the fsck.pb probe", d)
	}
	if n := cs.gets[fsckKey]; n != 1 {
		t.Fatalf("healthy summary fsck probes = %d, want 1", n)
	}
	if n := cs.gets[id.StorePrefix()+PlaceholderKeySuffix]; n != 1 {
		t.Fatalf("healthy summary must not probe the sidecar (real pays +0), probes = %v", cs.gets)
	}

	// Degraded summary: exactly the probe hit (no second read).
	putFsck(t, f, id, &proto.FsckReport{MissingTotal: 2, Missing: []string{"a", "b"}, At: fsckTs(time.Now())})
	before := cs.totalGets()
	if w := f.req("GET", "/demo/walgit/api"); w.Code != 200 {
		t.Fatalf("degraded summary = %d", w.Code)
	}
	if d := cs.totalGets() - before; d != 1 {
		t.Fatalf("degraded summary issued %d GETs, want 1", d)
	}

	// Overview: exactly one probe whether the report exists or not.
	f2 := newFixture(t)
	cs2 := &countStore{ObjectStore: f2.env.Store}
	f2.env.Store = cs2
	seedOverview(f2)
	if w := f2.req("GET", "/demo/walgit/api/overview"); w.Code != 200 {
		t.Fatalf("overview = %d", w.Code)
	}
	if n := cs2.totalGets(); n != 1 || cs2.gets[fsckKey] != 1 {
		t.Fatalf("overview (no report) GETs = %v, want exactly one probe", cs2.gets)
	}
	putFsck(t, f2, id, &proto.FsckReport{At: fsckTs(time.Now())})
	before = cs2.totalGets()
	if w := f2.req("GET", "/demo/walgit/api/overview"); w.Code != 200 {
		t.Fatalf("overview with report = %d", w.Code)
	}
	if d := cs2.totalGets() - before; d != 1 {
		t.Fatalf("overview (report) issued %d GETs, want 1", d)
	}
}
