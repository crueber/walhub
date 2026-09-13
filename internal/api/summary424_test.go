// summary424_test.go — issue #424 fork projection: the summary wire
// fields (fork_parent omitempty + always-present forks count) and the
// ETag story for ref-less fork mutations (a fork landing moves no parent
// ref, so the ETag covers the index version + count + parent — the
// #235/#240/#319 suffix precedent, mutable-collab class per #382).
package api

import (
	"context"
	"strings"
	"testing"
)

func TestSummaryForkWire(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
	}
	cases := []struct {
		name       string
		hook       func(ctx context.Context, owner, repo string) (ForkSummary, bool)
		wantParent string
		wantForks  int
		wantSuffix string // "" = bare head-sha etag
	}{
		{"nil hook", nil, "", 0, ""},
		{"declined hook", func(ctx context.Context, owner, repo string) (ForkSummary, bool) {
			return ForkSummary{}, false
		}, "", 0, ""},
		{"childless root", func(ctx context.Context, owner, repo string) (ForkSummary, bool) {
			return ForkSummary{Version: 1}, true
		}, "", 0, "~f1.0.0"},
		{"fork with siblings", func(ctx context.Context, owner, repo string) (ForkSummary, bool) {
			return ForkSummary{Parent: "demo/upstream", Count: 2, Version: 3}, true
		}, "demo/upstream", 2, "~f3.2."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.env.ForkInfo = c.hook
			w := f.req("GET", "/demo/walgit/api")
			if w.Code != 200 {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			var body struct {
				ForkParent string `json:"fork_parent"`
				Forks      int    `json:"forks"`
			}
			decodeJSON(t, w, &body)
			if body.ForkParent != c.wantParent || body.Forks != c.wantForks {
				t.Fatalf("fork = %q/%d, want %q/%d", body.ForkParent, body.Forks, c.wantParent, c.wantForks)
			}
			// forks is always present (0 = none); fork_parent rides
			// omitempty (absent when not a fork).
			if !strings.Contains(w.Body.String(), `"forks":`) {
				t.Fatalf("wire missing forks: %s", w.Body.String())
			}
			if c.wantParent == "" && strings.Contains(w.Body.String(), `"fork_parent"`) {
				t.Fatalf("wire must omit fork_parent: %s", w.Body.String())
			}
			etag := w.Header().Get("ETag")
			if c.wantSuffix == "" {
				if want := `"` + fakeSHA + `"`; etag != want {
					t.Fatalf("etag = %q, want %q", etag, want)
				}
			} else {
				if !strings.Contains(etag, c.wantSuffix) {
					t.Fatalf("etag = %q, want suffix %q", etag, c.wantSuffix)
				}
			}
			if cc := w.Header().Get("Cache-Control"); cc != ccMutable {
				t.Fatalf("summary cache = %q, want %q", cc, ccMutable)
			}
		})
	}
}

func TestSummaryForkRevalidate(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
	}
	version := 1
	f.env.ForkInfo = func(ctx context.Context, owner, repo string) (ForkSummary, bool) {
		return ForkSummary{Version: version}, true
	}
	w := f.req("GET", "/demo/walgit/api")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	before := w.Header().Get("ETag")

	// A fork landing moves no ref (same head sha, version 1→2): the old
	// ETag must NOT revalidate to 304.
	version = 2
	w2 := f.req("GET", "/demo/walgit/api")
	if w2.Code != 200 {
		t.Fatalf("status = %d", w2.Code)
	}
	after := w2.Header().Get("ETag")
	if after == before {
		t.Fatalf("etag did not move on fork: %q", after)
	}
	// The stale token 304s nothing now.
	stale := f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": before}, readP())
	if stale.Code != 200 {
		t.Fatalf("stale etag must not 304, got %d", stale.Code)
	}
}
