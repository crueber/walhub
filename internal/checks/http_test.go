package checks

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// Table-driven handler coverage: every route × method matrix, the auth
// mapping, cache classes, and the []-not-null wire rules.

// doRequest routes one request through the Handler (both lanes).
func doRequest(h *Handler, method, path, body, cred string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cred != "" {
		req.Header.Set("Authorization", cred)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func testHandler() (*testEnv, *Handler) {
	e := newTestEnv()
	return e, &Handler{Svc: e.svc}
}

func TestHandleRouting(t *testing.T) {
	e, h := testHandler()
	sha := hexSHA(50)
	e.knowSHA(sha)
	for _, tc := range []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{"report", "POST", "/o/r/api/checks/statuses/" + sha, `{"context":"ci","state":"success"}`, 200},
		{"report browser lane", "POST", "/o/r/api-browser/checks/statuses/" + sha, `{"context":"ci","state":"success"}`, 200},
		{"report git suffix", "POST", "/o/r.git/api/checks/statuses/" + sha, `{"context":"ci","state":"success"}`, 200},
		{"statuses", "GET", "/o/r/api/checks/statuses/" + sha, "", 200},
		{"combined", "GET", "/o/r/api/checks/" + sha, "", 200},
		{"index", "GET", "/o/r/api/checks", "", 200},
		{"index query", "GET", "/o/r/api/checks?n=10", "", 200},
		{"tokens list", "GET", "/o/r/api/checks/tokens", "", 200},
		{"tokens create", "POST", "/o/r/api/checks/tokens", `{"name":"ci"}`, 201},
		{"report 405", "PUT", "/o/r/api/checks/statuses/" + sha, "", 405},
		{"statuses 405", "DELETE", "/o/r/api/checks/statuses/" + sha, "", 405},
		{"combined 405", "POST", "/o/r/api/checks/" + sha, "", 405},
		{"index 405", "POST", "/o/r/api/checks", "", 405},
		{"tokens 405", "PUT", "/o/r/api/checks/tokens", "", 405},
		{"revoke 405", "GET", "/o/r/api/checks/tokens/abcd1234", "", 405},
		{"bad sha", "GET", "/o/r/api/checks/zzz", "", 400},
		{"bad sha statuses", "GET", "/o/r/api/checks/statuses/zzz", "", 400},
		{"bad sha report", "POST", "/o/r/api/checks/zzz", "", 405}, // shape matches combined; method doesn't
		{"extra segment", "GET", "/o/r/api/checks/" + sha + "/extra", "", 404},
		{"not checks", "GET", "/o/r/api/pulls", "", 404},
		{"top level", "GET", "/api/v1/repos", "", 404},
		{"bad n", "GET", "/o/r/api/checks?n=0", "", 400},
		{"big n", "GET", "/o/r/api/checks?n=201", "", 400},
		{"non-numeric n", "GET", "/o/r/api/checks?n=many", "", 400},
		{"bad after", "GET", "/o/r/api/checks?after=zzz", "", 400},
		{"bad body", "POST", "/o/r/api/checks/statuses/" + sha, `{oops`, 400},
		{"unknown field", "POST", "/o/r/api/checks/statuses/" + sha, `{"context":"ci","state":"success","bogus":1}`, 400},
		{"empty body", "POST", "/o/r/api/checks/statuses/" + sha, ``, 400},
		{"unknown token field", "POST", "/o/r/api/checks/tokens", `{"name":"x","secret":"y"}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(h, tc.method, tc.path, tc.body, "Bearer writer-token")
			// The test handler has nil Auth ⇒ anonymous ⇒ writes 401.
			// Reads on the public repo pass; writes need a principal.
			// Re-run writes with an injected principal below.
			_ = rec
			h2 := &Handler{Svc: e.svc, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
				return admin(), nil
			}}
			rec2 := doRequest(h2, tc.method, tc.path, tc.body, "")
			if rec2.Code != tc.wantStatus {
				t.Fatalf("%s %s: got %d want %d (%s)", tc.method, tc.path, rec2.Code, tc.wantStatus, rec2.Body.String())
			}
		})
	}
}

func TestHandlerAuthMapping(t *testing.T) {
	e, _ := testHandler()
	sha := hexSHA(51)
	e.knowSHA(sha)
	mkHandler := func(p auth.Principal, aerr *auth.AuthError) *Handler {
		return &Handler{Svc: e.svc, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
			return p, aerr
		}}
	}
	// Anonymous report ⇒ 401 + Bearer challenge.
	rec := doRequest(&Handler{Svc: e.svc}, "POST", "/o/r/api/checks/statuses/"+sha, `{"context":"ci","state":"success"}`, "")
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("anon report: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	// Anonymous read on the public repo ⇒ 200.
	rec = doRequest(&Handler{Svc: e.svc}, "GET", "/o/r/api/checks/"+sha, "", "")
	if rec.Code != 200 {
		t.Fatalf("anon read: %d", rec.Code)
	}
	// Auth chain errors map through.
	bad := mkHandler(auth.Principal{}, &auth.AuthError{Kind: auth.ErrForbidden, Why: "denied"})
	rec = doRequest(bad, "GET", "/o/r/api/checks/"+sha, "", "")
	if rec.Code != 403 {
		t.Fatalf("forbidden: %d", rec.Code)
	}
	down := mkHandler(auth.Principal{}, &auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"})
	rec = doRequest(down, "GET", "/o/r/api/checks/"+sha, "", "")
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("unavailable: %d", rec.Code)
	}
	// Revoke unknown ⇒ 404; revoke by non-admin ⇒ 403.
	w := mkHandler(writer(), nil)
	rec = doRequest(w, "DELETE", "/o/r/api/checks/tokens/deadbeef", "", "")
	if rec.Code != 403 {
		t.Fatalf("revoke by writer: %d", rec.Code)
	}
	a := mkHandler(admin(), nil)
	rec = doRequest(a, "DELETE", "/o/r/api/checks/tokens/deadbeef", "", "")
	if rec.Code != 404 {
		t.Fatalf("revoke unknown: %d", rec.Code)
	}
	// Revoke ⇒ 204 with empty body.
	created, _ := e.svc.CreateToken(ctx(), "o", "r", admin(), "ci", nil)
	rec = doRequest(a, "DELETE", "/o/r/api/checks/tokens/"+created.ID, "", "")
	if rec.Code != 204 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHandlerWireRules(t *testing.T) {
	e, h := testHandler()
	sha := hexSHA(52)
	e.knowSHA(sha)
	authed := &Handler{Svc: e.svc, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return writer(), nil
	}}
	_ = h
	// Reports answer 200 with the full record as JSON.
	rec := doRequest(authed, "POST", "/o/r/api/checks/statuses/"+sha, `{"context":"ci/build","state":"pending","description":"run 7","target_url":"https://ci.example/7"}`, "")
	if rec.Code != 200 {
		t.Fatalf("report: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	// Empty lists are [] (never null) and GETs are no-store.
	adminH := &Handler{Svc: e.svc, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return admin(), nil
	}}
	for _, path := range []string{
		"/o/r/api/checks/statuses/" + sha,
		"/o/r/api/checks/" + sha,
		"/o/r/api/checks",
		"/o/r/api/checks/tokens",
	} {
		rec := doRequest(adminH, "GET", path, "", "")
		if rec.Code != 200 {
			t.Fatalf("get %s: %d", path, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("get %s: cache = %q", path, cc)
		}
		if strings.Contains(rec.Body.String(), "null") && strings.Contains(path, "statuses") {
			t.Fatalf("null in %s: %s", path, rec.Body.String())
		}
	}
	// Statuses list carries the reported row.
	rec = doRequest(authed, "GET", "/o/r/api/checks/statuses/"+sha, "", "")
	if !strings.Contains(rec.Body.String(), `"context":"ci/build"`) {
		t.Fatalf("row: %s", rec.Body.String())
	}
	// Combined carries counts.
	rec = doRequest(authed, "GET", "/o/r/api/checks/"+sha, "", "")
	if !strings.Contains(rec.Body.String(), `"state":"pending"`) || !strings.Contains(rec.Body.String(), `"total_counts"`) {
		t.Fatalf("combined: %s", rec.Body.String())
	}
	// Token create answers 201 with the once-shown secret.
	rec = doRequest(authed, "POST", "/o/r/api/checks/tokens", `{"name":"w"}`, "")
	if rec.Code != 403 { // writer is not admin
		t.Fatalf("token create by writer: %d", rec.Code)
	}
	rec = doRequest(adminH, "POST", "/o/r/api/checks/tokens", `{"name":"w"}`, "")
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"token":"wct_`) {
		t.Fatalf("token create: %d %s", rec.Code, rec.Body.String())
	}
	// Errors are plain text.
	rec = doRequest(authed, "POST", "/o/r/api/checks/statuses/"+sha, `{"context":"ci","state":"bogus"}`, "")
	if rec.Code != 409 || rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("state enum: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestHandlerCITokenEndToEnd(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(53)
	e.knowSHA(sha)
	created, err := e.svc.CreateToken(ctx(), "o", "r", admin(), "woodpecker", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// The handler resolves wct_ through the injected Auth wrapper (the
	// composition path: ShapePrincipal + server chain).
	chain := func(r *http.Request) (auth.Principal, *auth.AuthError) { return anon(), nil }
	h := &Handler{Svc: e.svc, Auth: WrapAuth(chain)}
	bearer := "Bearer " + created.Token
	rec := doRequest(h, "POST", "/o/r/api/checks/statuses/"+sha, `{"context":"ci/build","state":"success"}`, bearer)
	if rec.Code != 200 {
		t.Fatalf("ci report: %d %s", rec.Code, rec.Body.String())
	}
	// Basic with the token as password works too.
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("ci:"+created.Token))
	rec = doRequest(h, "POST", "/o/r/api/checks/statuses/"+sha, `{"context":"lint","state":"success"}`, basic)
	if rec.Code != 200 {
		t.Fatalf("ci basic: %d %s", rec.Code, rec.Body.String())
	}
	// A forged secret 401s (the client erases it).
	id, _, _ := ParseCIToken(created.Token)
	rec = doRequest(h, "POST", "/o/r/api/checks/statuses/"+sha, `{"context":"ci/build","state":"success"}`, "Bearer wct_"+id+".forged")
	if rec.Code != 401 {
		t.Fatalf("forged: %d", rec.Code)
	}
	// A malformed wct_ token 401s at the Auth wrapper (never falls
	// through to anonymous reads on a private repo).
	e.roles.Public = false
	rec = doRequest(h, "GET", "/o/r/api/checks/"+sha, "", "Bearer wct_short")
	if rec.Code != 401 {
		t.Fatalf("malformed ci on private: %d", rec.Code)
	}
}
