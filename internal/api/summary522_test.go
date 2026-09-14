// summary522_test.go — Forgejo #522 per-repo feature flags: the summary
// wire field (always present, six enabled booleans — the #319 badge
// discipline), the ETag story for settings-only flips (no new commit, no
// ref move — the #235/#240/#319/#505 suffix precedent, unconditional per
// the #513 ruling), and the walView fold from the manifest-inline
// settings TOML (zero new store round trips — the Description precedent).
package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

func featuresPtr(f config.ResolvedFeatures) *config.ResolvedFeatures { return &f }

func TestSummaryFeaturesWire(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	allOff := config.ResolvedFeatures{}
	cases := []struct {
		name       string
		seed       *config.ResolvedFeatures // nil = unpopulated view (fail-open)
		want       config.ResolvedFeatures
		wantSuffix string
	}{
		{"nil view fails open to all-on", nil, config.AllFeatures(), "~t111111"},
		{"all-on projects all true", featuresPtr(config.AllFeatures()), config.AllFeatures(), "~t111111"},
		{"all-off projects all false", featuresPtr(allOff), allOff, "~t000000"},
		{"mixed projects per flag", featuresPtr(config.ResolvedFeatures{
			Issues: false, Pulls: true, Releases: false, Forks: true, Watch: true, Star: false,
		}), config.ResolvedFeatures{
			Issues: false, Pulls: true, Releases: false, Forks: true, Watch: true, Star: false,
		}, "~t010110"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.view.summaries["demo/walgit"] = SummaryData{
				Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
				Features: c.seed,
			}
			w := f.req("GET", "/demo/walgit/api")
			if w.Code != 200 {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			var body struct {
				Features config.ResolvedFeatures `json:"features"`
			}
			decodeJSON(t, w, &body)
			if body.Features != c.want {
				t.Fatalf("features = %+v, want %+v", body.Features, c.want)
			}
			// The object is always present (never null — the #319 badge
			// discipline), even when every flag is off.
			if !strings.Contains(w.Body.String(), `"features":{`) {
				t.Fatalf("wire missing features object: %s", w.Body.String())
			}
			for _, k := range []string{`"issues":`, `"pulls":`, `"releases":`, `"forks":`, `"watch":`, `"star":`} {
				if !strings.Contains(w.Body.String(), k) {
					t.Fatalf("wire missing %s: %s", k, w.Body.String())
				}
			}
			etag := w.Header().Get("ETag")
			if want := `"` + fakeSHA + `~k0` + c.wantSuffix + `"`; etag != want {
				t.Fatalf("etag = %q, want %q", etag, want)
			}
		})
	}
}

func TestSummaryFeaturesRevalidate(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
		Features: featuresPtr(config.AllFeatures()),
	}
	w := f.req("GET", "/demo/walgit/api")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	current := w.Header().Get("ETag")

	// Forgejo #513 regression for the new field: a client holding a
	// pre-#522 cached summary (no features object; head-sha + ~k0 ETag)
	// revalidating against the current all-on state must NOT 304 — the
	// stale field-less body would keep every tab visible via the
	// missing-field fail-open.
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": `"` + fakeSHA + `~k0"`}, readP())
	if w.Code != 200 {
		t.Fatalf("pre-#522 etag must revalidate to 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"features":{`) {
		t.Fatalf("repopulated body missing features: %s", w.Body.String())
	}

	// A settings-only flip (issues off, same head sha, no ref move)
	// must NOT 304 against the previous etag, or the tab bar keeps
	// showing Issues behind a 304.
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
		Features: featuresPtr(config.ResolvedFeatures{
			Issues: false, Pulls: true, Releases: true, Forks: true, Watch: true, Star: true,
		}),
	}
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": current}, readP())
	if w.Code != 200 {
		t.Fatalf("settings-only flip must revalidate to 200, got %d", w.Code)
	}
	var body struct {
		Features config.ResolvedFeatures `json:"features"`
	}
	decodeJSON(t, w, &body)
	if body.Features.Issues {
		t.Fatal("issues flag must project off after the flip")
	}
	flipped := w.Header().Get("ETag")
	if !strings.HasSuffix(flipped, `~t011111"`) {
		t.Fatalf("etag = %q, want ~t011111 suffix", flipped)
	}
	// The flipped etag 304s while the flags are current.
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": flipped}, readP())
	if w.Code != http.StatusNotModified {
		t.Fatalf("current features etag must 304, got %d", w.Code)
	}

	// Re-enabling restores the all-on suffix with counts intact (the
	// body still carries the same head/branches — only flags move).
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
		Features: featuresPtr(config.AllFeatures()),
	}
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": flipped}, readP())
	if w.Code != 200 {
		t.Fatalf("re-enable must revalidate to 200, got %d", w.Code)
	}
	if etag := w.Header().Get("ETag"); etag != current {
		t.Fatalf("re-enabled etag = %q, want the original %q", etag, current)
	}
}

// --- walView fold: manifest-inline settings TOML → SummaryData.Features ---

func TestWalSummaryFeatures(t *testing.T) {
	ctx := context.Background()
	id := git.RepoId{Owner: "demo", Name: "walgit"}
	fix := newGitFix(t)
	man := &proto.Manifest{HeadSeq: 3,
		Settings: &proto.RepoSettings{Toml: "[features]\nissues = false\nstar = false\n"}}
	eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 1, man: man}
	v := newEngineFixture(t, eng).view()
	s, err := v.Summary(ctx, id)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if s.Features == nil {
		t.Fatal("features must always be populated by the walView fold")
	}
	want := config.ResolvedFeatures{Issues: false, Pulls: true, Releases: true, Forks: true, Watch: true, Star: false}
	if *s.Features != want {
		t.Fatalf("features = %+v, want %+v", *s.Features, want)
	}
	// No settings on the manifest → all-on, never an error (zero
	// migration for existing repos).
	eng.man = &proto.Manifest{HeadSeq: 3}
	s, err = v.Summary(ctx, id)
	if err != nil || s.Features == nil || *s.Features != config.AllFeatures() {
		t.Fatalf("no-settings summary = %+v, %v", s.Features, err)
	}
	// Corrupt settings TOML fails open to all-on (display metadata must
	// never fail the summary — the DescriptionOf precedent).
	eng.man = &proto.Manifest{HeadSeq: 3,
		Settings: &proto.RepoSettings{Toml: "[broken\n"}}
	s, err = v.Summary(ctx, id)
	if err != nil || s.Features == nil || *s.Features != config.AllFeatures() {
		t.Fatalf("corrupt-settings summary = %+v, %v", s.Features, err)
	}
	// Nil engine fails open to all-on (never a nil view field).
	if got := (&walView{}).repoFeatures(ctx, id); got != config.AllFeatures() {
		t.Fatalf("nil engine = %+v", got)
	}
}
