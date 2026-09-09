// summary235_test.go — issue #235 repo description: the summary wire field,
// the ETag/cache story for description-only changes, and the walView fold
// from the manifest-inline settings TOML.
package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

func TestSummaryDescriptionWire(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.summaries["demo/walgit"] = SummaryData{
		Head:        &Ref{Name: "refs/heads/main", SHA: fakeSHA},
		Branches:    2,
		Tags:        1,
		Description: "short line",
	}
	w := f.req("GET", "/demo/walgit/api")
	if w.Code != 200 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Description string `json:"description"`
	}
	decodeJSON(t, w, &body)
	if body.Description != "short line" {
		t.Fatalf("description = %q", body.Description)
	}
	// The ETag covers the description (same head sha, new field).
	etag := w.Header().Get("ETag")
	if !strings.HasPrefix(etag, `"`+fakeSHA+`~d`) {
		t.Fatalf("etag = %q, want head sha + ~d suffix", etag)
	}

	// Unset renders as "" (field always present), with the bare head etag.
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 2, Tags: 1,
	}
	w = f.req("GET", "/demo/walgit/api")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"description":""`) {
		t.Fatalf("unset description must serialize as empty string: %s", w.Body.String())
	}
	if etag := w.Header().Get("ETag"); etag != `"`+fakeSHA+`"` {
		t.Fatalf("etag = %q, want bare head sha", etag)
	}
}

func TestSummaryDescription304(t *testing.T) {
	f := newFixture(t)
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
		Description: "v1 text",
	}
	w := f.req("GET", "/demo/walgit/api")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	current := w.Header().Get("ETag")

	// A description-only change must NOT 304 against the previous etag.
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
		Description: "v2 text",
	}
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": current}, readP())
	if w.Code != 200 {
		t.Fatalf("stale description etag must revalidate to 200, got %d", w.Code)
	}
	// The new etag 304s while the text is current.
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": w.Header().Get("ETag")}, readP())
	if w.Code != http.StatusNotModified {
		t.Fatalf("current description etag must 304, got %d", w.Code)
	}
	// Clearing the description busts the cache too (suffix drops off).
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
	}
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": current}, readP())
	if w.Code != 200 {
		t.Fatalf("cleared description must revalidate to 200, got %d", w.Code)
	}
}

// --- walView fold: manifest-inline settings TOML → SummaryData.Description ---

func TestWalSummaryDescription(t *testing.T) {
	ctx := context.Background()
	id := git.RepoId{Owner: "demo", Name: "walgit"}
	fix := newGitFix(t)
	man := &proto.Manifest{HeadSeq: 3,
		Settings: &proto.RepoSettings{Toml: "description = \"from the wal\"\n"}}
	eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 1, man: man}
	v := newEngineFixture(t, eng).view()
	s, err := v.Summary(ctx, id)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if s.Description != "from the wal" {
		t.Fatalf("description = %q", s.Description)
	}
	// No settings on the manifest → unset, never an error.
	eng.man = &proto.Manifest{HeadSeq: 3}
	s, err = v.Summary(ctx, id)
	if err != nil || s.Description != "" {
		t.Fatalf("no-settings summary = %+v, %v", s, err)
	}
	// Corrupt settings TOML fails open to "" (display metadata).
	eng.man = &proto.Manifest{HeadSeq: 3,
		Settings: &proto.RepoSettings{Toml: "[broken\n"}}
	s, err = v.Summary(ctx, id)
	if err != nil || s.Description != "" {
		t.Fatalf("corrupt-settings summary = %+v, %v", s, err)
	}
	// Nil engine fails closed to "".
	if got := (&walView{}).repoDescription(ctx, id); got != "" {
		t.Fatalf("nil engine = %q", got)
	}
}
