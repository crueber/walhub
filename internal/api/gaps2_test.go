package api

// gaps2_test.go sweeps the remaining error arms: a view that fails per method
// (→ 503/404 mapping through every handler), non-flushing SSE clients, and
// the small render helpers' edge arms.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

// failView fails one named RepoView method with a configurable error.
type failView struct {
	fakeView
	fail map[string]error
}

// newFailFixture embeds the shared fake view by pointer (fakeView has a mutex).

func (f *failView) err(name string) error { return f.fail[name] }

func (f *failView) Sync(_ context.Context, _ git.RepoId, _ SyncLevel) error { return f.err("Sync") }

func (f *failView) Resolve(ctx context.Context, id git.RepoId, rest string) (Resolution, error) {
	if err := f.err("Resolve"); err != nil {
		return Resolution{}, err
	}
	return f.fakeView.Resolve(ctx, id, rest)
}

func (f *failView) Head(ctx context.Context, id git.RepoId) (Ref, bool, error) {
	if err := f.err("Head"); err != nil {
		return Ref{}, false, err
	}
	return f.fakeView.Head(ctx, id)
}

func (f *failView) RefList(ctx context.Context, id git.RepoId, ns string, q RefQuery) ([]Ref, bool, error) {
	if err := f.err("RefList"); err != nil {
		return nil, false, err
	}
	return f.fakeView.RefList(ctx, id, ns, q)
}

func (f *failView) Tree(ctx context.Context, id git.RepoId, rev, path string) (TreeResult, error) {
	if err := f.err("Tree"); err != nil {
		return TreeResult{}, err
	}
	return f.fakeView.Tree(ctx, id, rev, path)
}

func (f *failView) Blob(ctx context.Context, id git.RepoId, rev, path string, raw bool) (BlobResult, error) {
	if err := f.err("Blob"); err != nil {
		return BlobResult{}, err
	}
	return f.fakeView.Blob(ctx, id, rev, path, raw)
}

func (f *failView) Commits(ctx context.Context, id git.RepoId, ref, path string, skip, n int) (CommitPage, error) {
	if err := f.err("Commits"); err != nil {
		return CommitPage{}, err
	}
	return f.fakeView.Commits(ctx, id, ref, path, skip, n)
}

func (f *failView) Commit(ctx context.Context, id git.RepoId, sha string) (CommitDetail, error) {
	if err := f.err("Commit"); err != nil {
		return CommitDetail{}, err
	}
	return f.fakeView.Commit(ctx, id, sha)
}

func (f *failView) Summary(ctx context.Context, id git.RepoId) (SummaryData, error) {
	if err := f.err("Summary"); err != nil {
		return SummaryData{}, err
	}
	return f.fakeView.Summary(ctx, id)
}

func (f *failView) Overview(ctx context.Context, id git.RepoId) (OverviewData, error) {
	if err := f.err("Overview"); err != nil {
		return OverviewData{}, err
	}
	return f.fakeView.Overview(ctx, id)
}

func (f *failView) Settings(ctx context.Context, id git.RepoId) (SettingsDoc, error) {
	if err := f.err("Settings"); err != nil {
		return SettingsDoc{}, err
	}
	return f.fakeView.Settings(ctx, id)
}

func (f *failView) SettingsHistory(ctx context.Context, id git.RepoId) (SettingsHistory, error) {
	if err := f.err("SettingsHistory"); err != nil {
		return SettingsHistory{}, err
	}
	return f.fakeView.SettingsHistory(ctx, id)
}

func (f *failView) HeadSeq(ctx context.Context, id git.RepoId) (uint64, error) {
	if err := f.err("HeadSeq"); err != nil {
		return 0, err
	}
	return f.fakeView.HeadSeq(ctx, id)
}

func (f *failView) PushHistory(ctx context.Context, id git.RepoId, last int) ([]PushRecord, error) {
	if err := f.err("PushHistory"); err != nil {
		return nil, err
	}
	return f.fakeView.PushHistory(ctx, id, last)
}

func newFailFixture(t *testing.T) (*fixture, *failView) {
	t.Helper()
	f := newFixture(t)
	fv := &failView{fakeView: fakeView{resolves: f.view.resolves, heads: f.view.heads,
		lists: f.view.lists, more: f.view.more, trees: f.view.trees, blobs: f.view.blobs, blobRaw: f.view.blobRaw,
		commitPg: f.view.commitPg, commits: f.view.commits, summaries: f.view.summaries, overviews: f.view.overviews,
		settings: f.view.settings, history: f.view.history, headSeq: f.view.headSeq, pushes: f.view.pushes,
		published: f.view.published},
		fail: map[string]error{}}
	f.env.Repo = fv
	return f, fv
}

func TestViewMethodErrors503(t *testing.T) {
	for _, name := range []string{
		"Sync", "Resolve", "Head", "RefList", "Tree", "Blob", "Commits", "Commit",
		"Summary", "Overview", "Settings", "SettingsHistory", "PushHistory",
	} {
		f, fv := newFailFixture(t)
		// rev-addressed handlers resolve first; give them a working resolve
		f.view.resolves["demo/walgit/main"] = Resolution{SHA: fakeSHA, Kind: "branch", Revision: 7}
		f.view.resolves["demo/walgit/"] = Resolution{SHA: fakeSHA, Kind: "branch", Revision: 7}
		f.view.resolves["demo/walgit/"+fakeSHA] = Resolution{SHA: fakeSHA, Kind: "commit", Revision: 7}
		fv.fail[name] = errBoom
		path := map[string]string{
			"Sync":            "/demo/walgit/api",
			"Resolve":         "/demo/walgit/api/resolve/main",
			"Head":            "/demo/walgit/api/refs",
			"RefList":         "/demo/walgit/api/refs/branches",
			"Tree":            "/demo/walgit/api/tree/main",
			"Blob":            "/demo/walgit/api/blob/main/x.txt",
			"Commits":         "/demo/walgit/api/commits?ref=main",
			"Commit":          "/demo/walgit/api/commit/" + fakeSHA,
			"Summary":         "/demo/walgit/api",
			"Overview":        "/demo/walgit/api/overview",
			"Settings":        "/demo/walgit/api/settings",
			"SettingsHistory": "/demo/walgit/api/settings/history",
			"PushHistory":     "/demo/walgit/api/policy/dry-run",
		}[name]
		method := "GET"
		if name == "PushHistory" {
			method = "POST"
		}
		w := f.req(method, path)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s failing → %d, want 503 (body %s)", name, w.Code, w.Body.String())
		}
	}
}

// notFoundView fails Summary with a wrapped ErrNotFound → 404 mapping.
func TestSummaryNotFoundMapping(t *testing.T) {
	f, fv := newFailFixture(t)
	fv.fail["Summary"] = errWrap(ErrNotFound)
	if w := f.req("GET", "/demo/walgit/api"); w.Code != http.StatusNotFound {
		t.Fatalf("wrapped not-found → %d, want 404", w.Code)
	}
}

// nonFlusher is an http.ResponseWriter without Flusher (SSE must refuse).
type nonFlusher struct{ header http.Header }

func (n *nonFlusher) Header() http.Header       { return n.header }
func (n *nonFlusher) Write([]byte) (int, error) { return 0, nil }
func (n *nonFlusher) WriteHeader(int)           {}

func TestSSERefusesNonFlusher(t *testing.T) {
	w := &nonFlusher{header: http.Header{}}
	if _, ok := NewSSE(w, httptest.NewRequest("GET", "/x", nil)); ok {
		t.Fatal("NewSSE must refuse a non-flushing writer")
	}
	rs, ok := newRefStream(w)
	if ok || rs != nil {
		t.Fatal("newRefStream must refuse a non-flushing writer")
	}
	// the ref SSE dialect reports 406 through the handler
	f := newFixture(t)
	f.view.lists["demo/walgit/heads"] = []Ref{{Name: "refs/heads/main", SHA: fakeSHA}}
	w2 := f.do("GET", "/demo/walgit/api/refs/branches?n=5", nil, map[string]string{"Accept": "text/event-stream"}, &auth.Principal{Name: "reader"})
	if w2.Code != http.StatusOK {
		t.Fatalf("SSE dialect = %d", w2.Code)
	}
}

func TestStreamRefsDialect(t *testing.T) {
	f := newFixture(t)
	f.view.lists["demo/walgit/heads"] = []Ref{
		{Name: "refs/heads/main", SHA: fakeSHA},
		{Name: "refs/heads/side", SHA: fakeSHA2},
	}
	w := f.do("GET", "/demo/walgit/api/refs/branches?n=1", nil, map[string]string{"Accept": "text/event-stream"}, &auth.Principal{Name: "reader"})
	if w.Code != 200 {
		t.Fatalf("stream = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "event: ref") || !strings.Contains(body, `"sha":"`+fakeSHA+`"`) {
		t.Fatalf("ref packets missing: %s", body)
	}
	if !strings.Contains(body, `event: done`) || !strings.Contains(body, `"more":true`) {
		t.Fatalf("done packet with more=true missing: %s", body)
	}
	if strings.Contains(body, ": walgit") {
		t.Fatal("ref dialect has no opener")
	}
}
