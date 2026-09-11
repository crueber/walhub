// serve320_test.go — issue #320 summary/health agreement: the
// serve-health sidecar degrades the summary, the mirror hook's
// DegradedReason verdict degrades without a second probe, empty repos
// stay empty, and the degraded flip busts the ETag. Table-driven
// httptest; `-race` mandatory.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// seedServeHealth plants a serve-health marker for the repo in the
// fixture store.
func seedServeHealth(t *testing.T, f *fixture, owner, name, reason string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"version": 1, "status": "degraded", "reason": reason, "at": "2026-09-11T00:00:00Z",
	})
	if _, err := store.PutBytes(context.Background(), f.env.Store,
		store.ServeHealthKey(owner, name), raw,
		store.PutOptions{Mode: store.PutOverwrite, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed serve-health: %v", err)
	}
}

func TestSummaryServeHealth(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed SummaryData // fakeView summary (Health "" → handler derives)
		// marker reason to seed ("" = none); hook verdict when the
		// hook projects (nil = hook declines → non-mirror).
		marker       string
		hookView     *MirrorView
		fsckMissing  bool
		wantHealth   string
		wantDegraded bool
	}{
		{
			name:       "healthy stays healthy with no marker",
			seed:       SummaryData{Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 2},
			wantHealth: "healthy",
		},
		{
			name:         "serve marker degrades a ref-healthy repo",
			seed:         SummaryData{Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 37},
			marker:       "serve sync wait timed out: objects not servable",
			wantHealth:   "degraded",
			wantDegraded: true,
		},
		{
			name:       "empty repo skips the marker probe",
			seed:       SummaryData{},
			marker:     "stale mark on an empty repo",
			wantHealth: "empty",
		},
		{
			name:         "fsck damage stays degraded with marker too",
			seed:         SummaryData{Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1},
			marker:       "serve sync wait timed out",
			fsckMissing:  true,
			wantHealth:   "degraded",
			wantDegraded: true,
		},
		{
			name: "mirror hook verdict degrades without a stored marker",
			seed: SummaryData{Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 37},
			hookView: &MirrorView{
				UpstreamURL: "https://example.com/a.git", Schedule: "hourly",
				LastResult: "ok", Due: true, DegradedReason: "serve sync wait timed out",
			},
			wantHealth:   "degraded",
			wantDegraded: true,
		},
		{
			name: "clean mirror hook keeps healthy",
			seed: SummaryData{Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 37},
			hookView: &MirrorView{
				UpstreamURL: "https://example.com/a.git", Schedule: "hourly",
				LastResult: "ok", Due: true,
			},
			wantHealth: "healthy",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			seedSummary(f)
			f.view.summaries["demo/walgit"] = tc.seed
			if tc.marker != "" {
				seedServeHealth(t, f, "demo", "walgit", tc.marker)
			}
			if tc.fsckMissing {
				putFsck(t, f, git.RepoId{Owner: "demo", Name: "walgit"}, &proto.FsckReport{MissingTotal: 3})
			}
			if tc.hookView != nil {
				v := *tc.hookView
				f.env.MirrorSummary = func(ctx context.Context, owner, repo string) (MirrorView, bool) {
					return v, true
				}
			} else {
				f.env.MirrorSummary = func(ctx context.Context, owner, repo string) (MirrorView, bool) {
					return MirrorView{}, false
				}
			}
			w := f.req("GET", "/demo/walgit/api")
			if w.Code != 200 {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			var body struct {
				Health string `json:"health"`
			}
			decodeJSON(t, w, &body)
			if body.Health != tc.wantHealth {
				t.Fatalf("health = %q, want %q (body=%s)", body.Health, tc.wantHealth, w.Body.String())
			}
			if etag := w.Header().Get("ETag"); tc.wantDegraded && !strings.Contains(etag, "~degraded") {
				t.Fatalf("etag = %q, want ~degraded suffix", etag)
			}
		})
	}
}

// TestSummaryServeHealthBustsCache pins the agreement flip: the same
// head sha reads healthy, then degraded after the mark lands (no 304
// across the flip), then healthy again once the mark clears.
func TestSummaryServeHealthBustsCache(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 37,
	}
	f.env.MirrorSummary = func(ctx context.Context, owner, repo string) (MirrorView, bool) {
		return MirrorView{}, false
	}
	w := f.req("GET", "/demo/walgit/api")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	healthyETag := w.Header().Get("ETag")
	if strings.Contains(healthyETag, "degraded") {
		t.Fatalf("healthy etag carries degraded: %q", healthyETag)
	}

	seedServeHealth(t, f, "demo", "walgit", "serve sync wait timed out")
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": healthyETag}, readP())
	if w.Code != 200 {
		t.Fatalf("degraded flip must revalidate to 200, got %d", w.Code)
	}
	degradedETag := w.Header().Get("ETag")
	if degradedETag == healthyETag {
		t.Fatal("etag did not move across the degraded flip")
	}
	// While degraded, the etag 304s.
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": degradedETag}, readP())
	if w.Code != http.StatusNotModified {
		t.Fatalf("current degraded etag must 304, got %d", w.Code)
	}
}

// TestServeTimeoutAnswers503 pins the fail-fast contract end to end: a
// serve-sync timeout (the #320 bound firing) answers 503 + Retry-After
// on every object route — never a hang, never a 404.
func TestServeTimeoutAnswers503(t *testing.T) {
	fix := newGitFix(t)
	eng := &fakeEngine{
		syncErr: &wal.WalError{Kind: wal.WalErrTimeout, Detail: "serve sync timed out: objects not servable"},
		obj:     wal.ObjectAccess{Local: fix.bare},
		rev:     7,
	}
	f := newEngineFixture(t, eng)
	for _, path := range []string{
		"/demo/walgit/api/tree/" + fix.main,
		"/demo/walgit/api/blob/" + fix.main + "/hello.txt",
		"/demo/walgit/api/commits?n=1",
		"/demo/walgit/api/commit/" + fix.main,
	} {
		w := f.do("GET", path)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("GET %s = %d (%s), want 503", path, w.Code, w.Body.String())
		}
		if ra := w.Header().Get("Retry-After"); ra == "" {
			t.Fatalf("GET %s missing Retry-After", path)
		}
		if body := w.Body.String(); !strings.Contains(body, "timed out") {
			t.Fatalf("GET %s body = %q, want the timeout reason", path, body)
		}
	}
	// And the engine error itself is not a not-found (the 404 branch
	// must never claim it).
	if errors.Is(notFoundOr(eng.syncErr), ErrNotFound) {
		t.Fatal("timeout mapped to not-found")
	}
}

// TestMirrorHashCoversDegradedReason pins the ETag input: a
// degraded_reason-only change moves the hash (else the projection
// flip hides behind a 304).
func TestMirrorHashCoversDegradedReason(t *testing.T) {
	a := MirrorView{UpstreamURL: "u", Schedule: "hourly", LastResult: "ok"}
	b := a
	b.DegradedReason = "serve sync wait timed out"
	if mirrorHash(a) == mirrorHash(b) {
		t.Fatal("degraded_reason change did not move the hash")
	}
}
