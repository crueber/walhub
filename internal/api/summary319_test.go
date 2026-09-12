// summary319_test.go — issue #319 open-count badges: the summary wire
// fields (always present, 0 = none), and the ETag/cache story for
// ref-less collab mutations (a close/reopen moves no ref, so the ETag
// covers the shared index version — the #235/#240 suffix precedent).
// The class is the #280 mutable-collab no-cache class (Forgejo #381
// moved the summary off SWR: the version-covering ETag makes
// revalidation correct, but SWR's stale-serve window still licensed the
// browser to paint the pre-mutation body on the next refresh).
package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestSummaryCollabCountsWire(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
	}
	cases := []struct {
		name       string
		hook       func(ctx context.Context, owner, repo string) (CollabCounts, bool)
		wantIssues int
		wantPulls  int
		wantSuffix string // "" = bare head-sha etag
	}{
		{"nil hook", nil, 0, 0, ""},
		{"declined hook", func(ctx context.Context, owner, repo string) (CollabCounts, bool) {
			return CollabCounts{}, false
		}, 0, 0, ""},
		{"counts", func(ctx context.Context, owner, repo string) (CollabCounts, bool) {
			if owner != "demo" || repo != "walgit" {
				return CollabCounts{}, false
			}
			return CollabCounts{OpenIssues: 3, OpenPulls: 1, Version: 7}, true
		}, 3, 1, "~c7"},
		{"all closed still versioned", func(ctx context.Context, owner, repo string) (CollabCounts, bool) {
			return CollabCounts{OpenIssues: 0, OpenPulls: 0, Version: 9}, true
		}, 0, 0, "~c9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.env.CollabCounts = c.hook
			w := f.req("GET", "/demo/walgit/api")
			if w.Code != 200 {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			var body struct {
				OpenIssues int `json:"open_issues"`
				OpenPulls  int `json:"open_pulls"`
			}
			decodeJSON(t, w, &body)
			if body.OpenIssues != c.wantIssues || body.OpenPulls != c.wantPulls {
				t.Fatalf("counts = %d/%d, want %d/%d", body.OpenIssues, body.OpenPulls, c.wantIssues, c.wantPulls)
			}
			// The fields are always present (0 = none — the badge hides
			// at 0 client-side), never omitted.
			for _, k := range []string{`"open_issues":`, `"open_pulls":`} {
				if !strings.Contains(w.Body.String(), k) {
					t.Fatalf("wire missing %s: %s", k, w.Body.String())
				}
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

func TestSummaryCollabCountsRevalidate(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
	}
	version := 7
	f.env.CollabCounts = func(ctx context.Context, owner, repo string) (CollabCounts, bool) {
		return CollabCounts{OpenIssues: 2, OpenPulls: 0, Version: version}, true
	}
	w := f.req("GET", "/demo/walgit/api")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	current := w.Header().Get("ETag")
	if !strings.HasSuffix(current, `~c7"`) {
		t.Fatalf("etag = %q, want ~c7 suffix", current)
	}

	// A close with no ref move (same head sha, version 7→8, count 2→1)
	// must NOT 304 against the previous etag.
	version = 8
	f.env.CollabCounts = func(ctx context.Context, owner, repo string) (CollabCounts, bool) {
		return CollabCounts{OpenIssues: 1, OpenPulls: 0, Version: version}, true
	}
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": current}, readP())
	if w.Code != 200 {
		t.Fatalf("stale counts etag must revalidate to 200, got %d", w.Code)
	}
	var body struct {
		OpenIssues int `json:"open_issues"`
	}
	decodeJSON(t, w, &body)
	if body.OpenIssues != 1 {
		t.Fatalf("open_issues = %d, want 1", body.OpenIssues)
	}
	// The new etag 304s while the counts are current.
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": w.Header().Get("ETag")}, readP())
	if w.Code != http.StatusNotModified {
		t.Fatalf("current counts etag must 304, got %d", w.Code)
	}
}
