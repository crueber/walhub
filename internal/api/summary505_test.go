// summary505_test.go — issue #505 checks-existence flag: the summary wire
// field (always present, false = none — the #319 badge discipline), and
// the ETag/cache story for the first report (creating the checks index
// moves no ref, so the ETag covers the index version — the
// #235/#240/#319 suffix precedent). The class is the #280
// mutable-collab no-cache class (Forgejo #381).
package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestSummaryChecksWire(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
	}
	cases := []struct {
		name       string
		hook       func(ctx context.Context, owner, repo string) (ChecksSummary, bool)
		wantHas    bool
		wantSuffix string // "~k0" = absent probe (Forgejo #513: never bare)
	}{
		{"nil hook", nil, false, "~k0"},
		{"declined hook", func(ctx context.Context, owner, repo string) (ChecksSummary, bool) {
			return ChecksSummary{}, false
		}, false, "~k0"},
		{"empty index still versioned", func(ctx context.Context, owner, repo string) (ChecksSummary, bool) {
			return ChecksSummary{HasChecks: false, Version: 1}, true
		}, false, "~k1"},
		{"checks reported", func(ctx context.Context, owner, repo string) (ChecksSummary, bool) {
			if owner != "demo" || repo != "walgit" {
				return ChecksSummary{}, false
			}
			return ChecksSummary{HasChecks: true, Version: 3}, true
		}, true, "~k3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.env.ChecksSummary = c.hook
			w := f.req("GET", "/demo/walgit/api")
			if w.Code != 200 {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			var body struct {
				HasChecks bool `json:"has_checks"`
			}
			decodeJSON(t, w, &body)
			if body.HasChecks != c.wantHas {
				t.Fatalf("has_checks = %v, want %v", body.HasChecks, c.wantHas)
			}
			// The field is always present (false = none — the Checks
			// tab hides client-side), never omitted.
			if !strings.Contains(w.Body.String(), `"has_checks":`) {
				t.Fatalf("wire missing has_checks: %s", w.Body.String())
			}
			etag := w.Header().Get("ETag")
			if want := `"` + fakeSHA + c.wantSuffix + `"`; etag != want {
				t.Fatalf("etag = %q, want %q", etag, want)
			}
			// The class is the #280 mutable-collab no-cache class (Forgejo
			// #381): revalidation stays ETag-cheap, the stale-serve window
			// is gone.
			if cc := w.Header().Get("Cache-Control"); cc != ccMutable {
				t.Fatalf("summary cache = %q, want %q", cc, ccMutable)
			}
		})
	}
}

func TestSummaryChecksRevalidate(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
	}
	// No checks yet: the hook declines (absent index). The etag still
	// carries ~k0 (Forgejo #513) — never bare.
	f.env.ChecksSummary = func(ctx context.Context, owner, repo string) (ChecksSummary, bool) {
		return ChecksSummary{}, false
	}
	w := f.req("GET", "/demo/walgit/api")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	absent := w.Header().Get("ETag")
	if want := `"` + fakeSHA + `~k0"`; absent != want {
		t.Fatalf("etag = %q, want %q", absent, want)
	}

	// Forgejo #513 regression: a client holding a pre-#505 cached
	// summary (no has_checks field; bare head-sha ETag) revalidating
	// against the current absent-index state must NOT 304 — the stale
	// field-less body would keep the Checks tab visible via the
	// missing-field fail-open.
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": `"` + fakeSHA + `"`}, readP())
	if w.Code != 200 {
		t.Fatalf("pre-#505 bare etag must revalidate to 200, got %d", w.Code)
	}
	var preBody struct {
		HasChecks bool `json:"has_checks"`
	}
	decodeJSON(t, w, &preBody)
	if preBody.HasChecks {
		t.Fatalf("has_checks = true on a check-less repo")
	}
	if !strings.Contains(w.Body.String(), `"has_checks":`) {
		t.Fatalf("repopulated body missing has_checks: %s", w.Body.String())
	}

	// The first report creates the index and moves no ref (same head
	// sha, version 1) — revalidation against the absent etag must NOT
	// 304, or the Checks tab stays hidden after CI reports.
	version := 1
	f.env.ChecksSummary = func(ctx context.Context, owner, repo string) (ChecksSummary, bool) {
		return ChecksSummary{HasChecks: true, Version: version}, true
	}
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": absent}, readP())
	if w.Code != 200 {
		t.Fatalf("first-report etag must revalidate to 200, got %d", w.Code)
	}
	var body struct {
		HasChecks bool `json:"has_checks"`
	}
	decodeJSON(t, w, &body)
	if !body.HasChecks {
		t.Fatalf("has_checks = false after the first report")
	}
	flipped := w.Header().Get("ETag")
	if !strings.HasSuffix(flipped, `~k1"`) {
		t.Fatalf("etag = %q, want ~k1 suffix", flipped)
	}

	// A second report (version 1→2, same head sha) must bust again.
	version = 2
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": flipped}, readP())
	if w.Code != 200 {
		t.Fatalf("second-report etag must revalidate to 200, got %d", w.Code)
	}
	// The new etag 304s while the index is current.
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": w.Header().Get("ETag")}, readP())
	if w.Code != http.StatusNotModified {
		t.Fatalf("current checks etag must 304, got %d", w.Code)
	}
}
