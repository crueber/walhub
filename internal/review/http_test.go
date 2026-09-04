package review

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// testHandler builds a Handler over a seeded service; auth resolves
// "Bearer <name>" (admin: prefix grants host admin), empty = anonymous.
func testHandler(t *testing.T) (*Handler, *Service) {
	t.Helper()
	svc, _ := testSvc()
	seedPR(t, svc)
	h := &Handler{Svc: svc, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		v := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		v = strings.TrimSpace(v)
		if v == "" {
			return auth.Anonymous(), nil
		}
		if v == "broken" {
			return auth.Anonymous(), &auth.AuthError{Kind: auth.ErrUnavailable, Why: "auth down"}
		}
		if strings.HasPrefix(v, "admin:") {
			return auth.Principal{Name: strings.TrimPrefix(v, "admin:"), Admin: true}, nil
		}
		return auth.Principal{Name: v}, nil
	}}
	return h, svc
}

func doReq(h *Handler, method, path, token, body string) *httptest.ResponseRecorder {
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func mustJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("non-JSON 200: %q", w.Body.String())
	}
	return m
}

func TestHTTPReviews(t *testing.T) {
	submit := `{"state":"APPROVED","commit_sha":"` + testHead + `","body":"lgtm"}`
	for _, lane := range []string{"api", "api-browser"} {
		base := "/o/r/" + lane + "/pulls/7"
		t.Run("lane "+lane, func(t *testing.T) {
			h, _ := testHandler(t)
			// GET empty list → 200 {reviews:[],more:false}.
			w := doReq(h, "GET", base+"/reviews", "carol", "")
			if w.Code != 200 {
				t.Fatalf("list: %d %s", w.Code, w.Body.String())
			}
			m := mustJSON(t, w)
			if len(m["reviews"].([]any)) != 0 || m["more"].(bool) {
				t.Fatalf("%v", m)
			}
			// POST approve → 201.
			w = doReq(h, "POST", base+"/reviews", "bob", submit)
			if w.Code != 201 {
				t.Fatalf("submit: %d %s", w.Code, w.Body.String())
			}
			m = mustJSON(t, w)
			if m["review"].(map[string]any)["seq"].(float64) != 1 {
				t.Fatalf("%v", m)
			}
			if m["summary"].(map[string]any)["decision"] != "APPROVED" {
				t.Fatalf("%v", m)
			}
			// GET one → 200; unknown → 404.
			w = doReq(h, "GET", base+"/reviews/1", "carol", "")
			if w.Code != 200 {
				t.Fatalf("get: %d", w.Code)
			}
			w = doReq(h, "GET", base+"/reviews/9", "carol", "")
			if w.Code != 404 {
				t.Fatalf("unknown: %d", w.Code)
			}
			// Self-approve → 422; stale sha → 409.
			w = doReq(h, "POST", base+"/reviews", "alice", submit)
			if w.Code != 422 {
				t.Fatalf("self: %d", w.Code)
			}
			w = doReq(h, "POST", base+"/reviews", "dave",
				`{"state":"APPROVED","commit_sha":"`+testHead2+`"}`)
			if w.Code != 409 || !strings.Contains(w.Body.String(), "reviewed commit is not the pull request head") {
				t.Fatalf("stale: %d %s", w.Code, w.Body.String())
			}
			// Dismiss → 200 (maintain); non-maintain → 403.
			w = doReq(h, "POST", base+"/reviews/1/dismiss", "carol", `{"reason":"x"}`)
			if w.Code != 403 {
				t.Fatalf("dismiss non-maintain: %d", w.Code)
			}
			w = doReq(h, "POST", base+"/reviews/1/dismiss", "bob", `{"reason":"stale"}`)
			if w.Code != 200 {
				t.Fatalf("dismiss: %d %s", w.Code, w.Body.String())
			}
			if mustJSON(t, w)["summary"].(map[string]any)["decision"] != "REVIEW_REQUIRED" {
				t.Fatalf("dismissed summary wrong")
			}
			// Anonymous list → 401 with Bearer challenge.
			w = doReq(h, "GET", base+"/reviews", "", "")
			if w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
				t.Fatalf("anon: %d", w.Code)
			}
			// Unknown field → 400; bad method → 405.
			w = doReq(h, "POST", base+"/reviews", "bob", `{"state":"APPROVED","commit_sha":"`+testHead+`","zzz":1}`)
			if w.Code != 400 {
				t.Fatalf("unknown field: %d", w.Code)
			}
			w = doReq(h, "PUT", base+"/reviews", "bob", "")
			if w.Code != 405 {
				t.Fatalf("method: %d", w.Code)
			}
		})
	}
}

func TestHTTPThreads(t *testing.T) {
	anchor := `{"path":"src/main.go","side":"NEW","new_start":120,"new_lines":3,` +
		`"commit_sha":"` + testHead + `",` +
		`"context_sha":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`
	for _, lane := range []string{"api", "api-browser"} {
		base := "/o/r/" + lane + "/pulls/7"
		t.Run("lane "+lane, func(t *testing.T) {
			h, _ := testHandler(t)
			// Open → 201 with tid.
			w := doReq(h, "POST", base+"/threads", "carol", `{"anchor":`+anchor+`,"body":"nit"}`)
			if w.Code != 201 {
				t.Fatalf("open: %d %s", w.Code, w.Body.String())
			}
			tid := mustJSON(t, w)["thread"].(map[string]any)["tid"].(string)
			if tid != "00000001" {
				t.Fatalf("tid %s", tid)
			}
			// List → 200; resolved=false filter hides after resolve.
			w = doReq(h, "GET", base+"/threads", "carol", "")
			if w.Code != 200 || len(mustJSON(t, w)["threads"].([]any)) != 1 {
				t.Fatalf("list: %d %s", w.Code, w.Body.String())
			}
			// Comment → 201.
			w = doReq(h, "POST", base+"/threads/"+tid+"/comments", "bob", `{"body":"ack"}`)
			if w.Code != 201 {
				t.Fatalf("comment: %d %s", w.Code, w.Body.String())
			}
			// Get → thread + 2 comments newest-first.
			w = doReq(h, "GET", base+"/threads/"+tid, "carol", "")
			if w.Code != 200 {
				t.Fatalf("get: %d", w.Code)
			}
			m := mustJSON(t, w)
			if len(m["comments"].([]any)) != 2 {
				t.Fatalf("%v", m)
			}
			// Resolve by participant → 200.
			w = doReq(h, "POST", base+"/threads/"+tid+"/resolve", "bob", "")
			if w.Code != 200 {
				t.Fatalf("resolve: %d %s", w.Code, w.Body.String())
			}
			if !mustJSON(t, w)["thread"].(map[string]any)["resolved"].(bool) {
				t.Fatalf("not resolved")
			}
			w = doReq(h, "GET", base+"/threads?resolved=false", "carol", "")
			if w.Code != 200 || len(mustJSON(t, w)["threads"].([]any)) != 0 {
				t.Fatalf("filter: %d %s", w.Code, w.Body.String())
			}
			// Unresolve → 200.
			w = doReq(h, "POST", base+"/threads/"+tid+"/unresolve", "bob", "")
			if w.Code != 200 {
				t.Fatalf("unresolve: %d", w.Code)
			}
			// Bad tid → 404; bad resolved → 400.
			w = doReq(h, "GET", base+"/threads/zzz", "carol", "")
			if w.Code != 404 {
				t.Fatalf("bad tid: %d", w.Code)
			}
			w = doReq(h, "GET", base+"/threads?resolved=maybe", "carol", "")
			if w.Code != 400 {
				t.Fatalf("bad filter: %d", w.Code)
			}
		})
	}
}

func TestHTTPRequestsSuggest(t *testing.T) {
	for _, lane := range []string{"api", "api-browser"} {
		base := "/o/r/" + lane + "/pulls/7"
		t.Run("lane "+lane, func(t *testing.T) {
			h, _ := testHandler(t)
			w := doReq(h, "GET", base+"/review-requests", "carol", "")
			if w.Code != 200 || len(mustJSON(t, w)["reviewers"].([]any)) != 0 {
				t.Fatalf("empty: %d %s", w.Code, w.Body.String())
			}
			w = doReq(h, "POST", base+"/review-requests", "carol", `{"reviewers":["bob"]}`)
			if w.Code != 403 {
				t.Fatalf("non-author add: %d", w.Code)
			}
			w = doReq(h, "POST", base+"/review-requests", "alice", `{"reviewers":["bob","carol"]}`)
			if w.Code != 200 || len(mustJSON(t, w)["reviewers"].([]any)) != 2 {
				t.Fatalf("add: %d %s", w.Code, w.Body.String())
			}
			w = doReq(h, "DELETE", base+"/review-requests", "carol", `{"reviewers":["carol"]}`)
			if w.Code != 200 || len(mustJSON(t, w)["reviewers"].([]any)) != 1 {
				t.Fatalf("self-remove: %d %s", w.Code, w.Body.String())
			}
			w = doReq(h, "GET", base+"/review-suggest?q=b", "carol", "")
			if w.Code != 200 {
				t.Fatalf("suggest: %d %s", w.Code, w.Body.String())
			}
			w = doReq(h, "GET", base+"/review-suggest", "", "")
			if w.Code != 401 {
				t.Fatalf("anon suggest: %d", w.Code)
			}
		})
	}
}

func TestHTTPRouting(t *testing.T) {
	h, _ := testHandler(t)
	for _, tc := range []struct {
		method, path string
		token        string
		want         int
	}{
		{"GET", "/o/r/api/pulls/7/nope", "carol", 404},
		{"GET", "/o/r/api/pulls/0/reviews", "carol", 404},
		{"GET", "/o/r/api/pulls/7/reviews/x/dismiss", "bob", 404},
		{"POST", "/o/r/api/pulls/7/reviews/1/bogus", "bob", 404},
		{"GET", "/o/r/api/issues", "carol", 404}, // not ours — core answers
		{"GET", "/nope/api/pulls/7/reviews", "carol", 404},
		{"DELETE", "/o/r/api/pulls/7/reviews", "bob", 405},
		{"PUT", "/o/r/api/pulls/7/threads", "bob", 405},
		{"DELETE", "/o/r/api/pulls/7/review-suggest", "carol", 405},
		{"POST", "/o/r/api/pulls/7/review-requests/extra", "alice", 404},
	} {
		w := doReq(h, tc.method, tc.path, tc.token, "")
		if w.Code != tc.want {
			t.Errorf("%s %s = %d, want %d (%s)", tc.method, tc.path, w.Code, tc.want, w.Body.String())
		}
	}
	// Errors are plain text.
	w := doReq(h, "GET", "/o/r/api/pulls/7/reviews/9", "carol", "")
	if w.Code != 404 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("content-type %q", w.Header().Get("Content-Type"))
	}
	// No-store on JSON success.
	w = doReq(h, "GET", "/o/r/api/pulls/7/reviews", "carol", "")
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache %q", w.Header().Get("Cache-Control"))
	}
}

func TestHTTPStreamEmit(t *testing.T) {
	h, svc := testHandler(t)
	var streamed []StreamEvent
	var notified []NotifyEvent
	svc.Stream = func(_ context.Context, ev StreamEvent) { streamed = append(streamed, ev) }
	svc.Notify = func(_ context.Context, ev NotifyEvent) { notified = append(notified, ev) }
	base := "/o/r/api/pulls/7"
	w := doReq(h, "POST", base+"/reviews", "bob",
		`{"state":"APPROVED","commit_sha":"`+testHead+`","threads":[]}`)
	if w.Code != 201 {
		t.Fatalf("submit: %d", w.Code)
	}
	if len(streamed) != 1 || streamed[0].Name != "review" || streamed[0].Summary == nil {
		t.Fatalf("%+v", streamed)
	}
	if len(notified) != 1 || notified[0].Class != "review_submitted" {
		t.Fatalf("%+v", notified)
	}
}
