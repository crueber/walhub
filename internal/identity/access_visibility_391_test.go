package identity

// Forgejo #391: visibility "bounces" on the settings page — after saving,
// a refresh shows the old value. Candidate causes ranked in the issue:
// (1) the save silently failed (403/409) with no reseed, (2) stale CAS
// version, (3) store conditional-GET staleness behind the access LRU,
// (4) multi-path/multi-instance disagreement.
//
// These tests run the REAL HTTP handler (routeAccess) against REAL
// backends (memory + filesystem) and pin the contract the UI fix relies
// on: a fresh-version PUT sticks across LRU-cached re-reads, a stale
// version 409s (the retry trigger), a non-admin PUT 403s (the loud-error
// trigger), and a second instance sharing the store converges on the
// next conditional revalidation (ruling out candidates 3+4 on these
// backend classes — the store contract suite pins the If-None-Match
// mapping they depend on).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store"
)

// services391 builds one Service per backend class sharing nothing:
// memory (fresh) and filesystem (fresh temp dir).
func services391(t *testing.T) map[string]*Service {
	t.Helper()
	fs, err := store.NewFilesystemRoot(t.TempDir(), 8)
	if err != nil {
		t.Fatalf("filesystem backend: %v", err)
	}
	out := map[string]*Service{}
	for name, st := range map[string]store.ObjectStore{"memory": store.NewMemory(), "filesystem": fs} {
		s := New(st, config.Defaults())
		s.Now = testClock
		out[name] = s
	}
	return out
}

func getAccessDoc391(t *testing.T, h *Handler, target string) (int, string) {
	t.Helper()
	w := doReq(h, "GET", target, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, w.Code, w.Body.String())
	}
	var doc struct {
		Version    int    `json:"version"`
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode access doc: %v", err)
	}
	return doc.Version, doc.Visibility
}

func putAccess391(t *testing.T, h *Handler, target string, version int, vis string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(h, "PUT", target, `{"version":`+strconv.Itoa(version)+`,"visibility":`+strconv.Quote(vis)+`,"role_bindings":[]}`)
}

// A fresh-version PUT sticks across refreshes, including the LRU
// NotModified re-read path; a stale version 409s (the client's
// retry trigger — without retry+reseed the select shows a value the
// server disagrees with, i.e. the #391 bounce).
func TestAccessVisibility391_SaveSticksAcrossRefresh(t *testing.T) {
	for name, s := range services391(t) {
		t.Run(name, func(t *testing.T) {
			h := testHandler(s, admin)
			target := "/acme/repo/api/access"

			v0, _ := getAccessDoc391(t, h, target)
			if w := putAccess391(t, h, target, v0, "private"); w.Code != http.StatusOK {
				t.Fatalf("PUT private = %d: %s", w.Code, w.Body.String())
			}
			// Refresh 1: server truth is private.
			if v, vis := getAccessDoc391(t, h, target); vis != "private" || v != v0+1 {
				t.Fatalf("after save: version=%d visibility=%q, want %d/private", v, vis, v0+1)
			}
			// Refresh 2 rides the LRU NotModified path (conditional GET
			// against the cached store version) — must not regress.
			if v, vis := getAccessDoc391(t, h, target); vis != "private" || v != v0+1 {
				t.Fatalf("LRU re-read: version=%d visibility=%q, want %d/private", v, vis, v0+1)
			}
			// A stale integer version 409s — the failure the UI must
			// retry (once, fresh) or reseed loudly, never swallow.
			if w := putAccess391(t, h, target, v0, "public"); w.Code != http.StatusConflict {
				t.Fatalf("stale PUT = %d, want 409", w.Code)
			}
			// The retry with the fresh version sticks.
			v1, _ := getAccessDoc391(t, h, target)
			if w := putAccess391(t, h, target, v1, "authenticated"); w.Code != http.StatusOK {
				t.Fatalf("retry PUT = %d: %s", w.Code, w.Body.String())
			}
			if _, vis := getAccessDoc391(t, h, target); vis != "authenticated" {
				t.Fatalf("after retry: visibility=%q, want authenticated", vis)
			}
		})
	}
}

// A non-admin visibility PUT fails closed with 403 (candidate #1:
// the screenshot user's save never reached the bucket — the UI
// showed the chosen value anyway until refresh reseeded public).
func TestAccessVisibility391_NonAdminPutForbidden(t *testing.T) {
	for name, s := range services391(t) {
		t.Run(name, func(t *testing.T) {
			h := testHandler(s, stranger)
			if w := putAccess391(t, h, "/acme/repo/api/access", 0, "private"); w.Code != http.StatusForbidden {
				t.Fatalf("stranger PUT = %d, want 403", w.Code)
			}
		})
	}
}

// Two instances sharing one store converge: B primes its LRU on the
// old doc, A writes, B's next GET revalidates and serves the new
// visibility. A failure here would be genuine multi-instance
// staleness (candidates #3/#4) and would extend #382's contract
// test instead of patching the settings page.
func TestAccessVisibility391_TwoInstancesConverge(t *testing.T) {
	stores := map[string]store.ObjectStore{"memory": store.NewMemory()}
	if fs, err := store.NewFilesystemRoot(t.TempDir(), 8); err != nil {
		t.Fatalf("filesystem backend: %v", err)
	} else {
		stores["filesystem"] = fs
	}
	for name, st := range stores {
		t.Run(name, func(t *testing.T) {
			newSvc := func() *Service {
				s := New(st, config.Defaults())
				s.Now = testClock
				return s
			}
			a, b := newSvc(), newSvc()
			ha, hb := testHandler(a, admin), testHandler(b, admin)
			target := "/acme/repo/api/access"

			// B primes its LRU on the synthesized/public doc.
			if _, vis := getAccessDoc391(t, hb, target); vis != "public" {
				t.Fatalf("primed visibility=%q, want public", vis)
			}
			// A saves private through the full HTTP path.
			v0, _ := getAccessDoc391(t, ha, target)
			if w := putAccess391(t, ha, target, v0, "private"); w.Code != http.StatusOK {
				t.Fatalf("A PUT = %d: %s", w.Code, w.Body.String())
			}
			// B converges on its very next GET — staleness is bounded
			// by one conditional revalidation, never indefinite.
			if _, vis := getAccessDoc391(t, hb, target); vis != "private" {
				t.Fatalf("B after A write: visibility=%q, want private", vis)
			}
		})
	}
}
