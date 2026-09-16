// http_test.go — the Seam 1 surface: authz, validation, write-only
// secrets, scrubbed errors, both lanes, discovery templates.
package pushmirror

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

func testHandler(t *testing.T, p auth.Principal) (*Handler, *Service, context.Context) {
	t.Helper()
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	h := &Handler{Svc: svc, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return p, nil
	}}
	return h, svc, ctx
}

var adminPrincipal = auth.Principal{Name: "ada", Write: true, Admin: true}
var userPrincipal = auth.Principal{Name: "bob", Write: true}
var anonPrincipal = auth.Anonymous()

func doReq(h *Handler, method, path, body string) *httptest.ResponseRecorder {
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rdr)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func createRepoForHTTP(t *testing.T, svc *Service, ctx context.Context, owner, name string) {
	t.Helper()
	if _, err := svc.reg.Create(ctx, owner+"/"+name, git.Sha1); err != nil {
		t.Fatalf("create repo: %v", err)
	}
}

func TestGetNotConfigured404(t *testing.T) {
	h, svc, ctx := testHandler(t, adminPrincipal)
	createRepoForHTTP(t, svc, ctx, "o", "r")
	w := doReq(h, "GET", "/o/r/api/pushmirror", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("GET absent = %d, want 404", w.Code)
	}
}

func TestPutRequiresAdmin(t *testing.T) {
	for _, tc := range []struct {
		p    auth.Principal
		want int
	}{
		{anonPrincipal, http.StatusUnauthorized},
		{userPrincipal, http.StatusForbidden},
	} {
		h, svc, ctx := testHandler(t, tc.p)
		createRepoForHTTP(t, svc, ctx, "o", "r")
		w := doReq(h, "PUT", "/o/r/api/pushmirror", `{"upstream_url":"file:///x"}`)
		if w.Code != tc.want {
			t.Errorf("PUT as %+v = %d, want %d", tc.p, w.Code, tc.want)
		}
	}
}

func TestPutCreateFileNoneAndUpdate(t *testing.T) {
	h, svc, ctx := testHandler(t, adminPrincipal)
	createRepoForHTTP(t, svc, ctx, "o", "r")
	w := doReq(h, "PUT", "/o/r/api/pushmirror", `{"upstream_url":"file:///tmp/up.git"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT create = %d %q", w.Code, w.Body.String())
	}
	var v View
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.AuthKind != AuthNone || v.UpstreamURL != "file:///tmp/up.git" {
		t.Errorf("created view = %+v", v)
	}
	// Browser lane twins the api lane.
	w = doReq(h, "GET", "/o/r/api-browser/pushmirror", "")
	if w.Code != http.StatusOK {
		t.Errorf("browser-lane GET = %d", w.Code)
	}
	// Secrets never echo: create a second repo on https, set a token,
	// the response carries the hint only.
	createRepoForHTTP(t, svc, ctx, "o", "t")
	w = doReq(h, "PUT", "/o/t/api/pushmirror", `{"upstream_url":"https://github.com/o/t.git","auth_kind":"token","token":"tok-secret-9999"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT token create = %d %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "tok-secret-9999") {
		t.Errorf("stored token echoed: %q", body)
	}
	var v2 View
	if err := json.Unmarshal(w.Body.Bytes(), &v2); err != nil {
		t.Fatal(err)
	}
	if !v2.HasSecret || v2.SecretHint != "••••9999" {
		t.Errorf("hint = %+v", v2)
	}
	if v2.AuthKind != AuthToken {
		t.Errorf("kind = %+v", v2)
	}
	// Upstream change is 409 (back on the file:// repo).
	w = doReq(h, "PUT", "/o/r/api/pushmirror", `{"upstream_url":"file:///tmp/other.git"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("upstream change = %d, want 409", w.Code)
	}
	// Schedule on then off.
	w = doReq(h, "PUT", "/o/r/api/pushmirror", `{"schedule":"daily"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("schedule on = %d %q", w.Code, w.Body.String())
	}
	w = doReq(h, "PUT", "/o/r/api/pushmirror", `{"schedule":""}`)
	if w.Code != http.StatusOK {
		t.Fatalf("schedule off = %d %q", w.Code, w.Body.String())
	}
	var v3 View
	if err := json.Unmarshal(w.Body.Bytes(), &v3); err != nil {
		t.Fatal(err)
	}
	if v3.Schedule != "" {
		t.Errorf("schedule not off: %+v", v3)
	}
	// Unknown schedule fails closed.
	w = doReq(h, "PUT", "/o/r/api/pushmirror", `{"schedule":"cron"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad schedule = %d, want 400", w.Code)
	}
	// Unknown field fails closed.
	w = doReq(h, "PUT", "/o/r/api/pushmirror", `{"frobnicate":1}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown field = %d, want 400", w.Code)
	}
	// DELETE stops everything.
	w = doReq(h, "DELETE", "/o/r/api/pushmirror", "")
	if w.Code != http.StatusNoContent {
		t.Errorf("DELETE = %d", w.Code)
	}
	w = doReq(h, "GET", "/o/r/api/pushmirror", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", w.Code)
	}
}

func TestPutUnbornRepo404(t *testing.T) {
	h, _, _ := testHandler(t, adminPrincipal)
	w := doReq(h, "PUT", "/o/ghost/api/pushmirror", `{"upstream_url":"file:///x"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("PUT unborn = %d, want 404", w.Code)
	}
}

func TestPutTransportValidation(t *testing.T) {
	h, svc, ctx := testHandler(t, adminPrincipal)
	createRepoForHTTP(t, svc, ctx, "o", "r")
	// token over plaintext http: refused.
	w := doReq(h, "PUT", "/o/r/api/pushmirror", `{"upstream_url":"http://example.com/o/r.git","auth_kind":"token","token":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("token-over-http = %d, want 400", w.Code)
	}
	// ssh key for https: refused.
	w = doReq(h, "PUT", "/o/r/api/pushmirror", `{"upstream_url":"https://example.com/o/r.git","auth_kind":"ssh","ssh_private_key":"k"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("ssh-for-https = %d, want 400", w.Code)
	}
}

func TestKeygenRoundTrip(t *testing.T) {
	h, svc, ctx := testHandler(t, adminPrincipal)
	createRepoForHTTP(t, svc, ctx, "o", "r")
	// Keygen needs a config first.
	w := doReq(h, "POST", "/o/r/api-browser/pushmirror/keygen", `{}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("keygen without config = %d, want 404", w.Code)
	}
	w = doReq(h, "PUT", "/o/r/api/pushmirror", `{"upstream_url":"git@example.com:o/r.git","auth_kind":"ssh","ssh_private_key":"placeholder-key","dangerous":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("ssh create = %d %q", w.Code, w.Body.String())
	}
	w = doReq(h, "POST", "/o/r/api/pushmirror/keygen", `{"known_hosts":"example.com ssh-ed25519 AAAA\n"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("keygen = %d %q", w.Code, w.Body.String())
	}
	var out struct {
		PublicKey      string `json:"public_key"`
		KeyFingerprint string `json:"key_fingerprint"`
		HasSecret      bool   `json:"has_secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.PublicKey, "ssh-ed25519 ") || !strings.HasPrefix(out.KeyFingerprint, "SHA256:") || !out.HasSecret {
		t.Errorf("keygen response = %+v", out)
	}
	if strings.Contains(w.Body.String(), "BEGIN OPENSSH PRIVATE KEY") {
		t.Error("private key echoed in keygen response")
	}
	// Config now shows the public key + ssh kind; secret has material.
	w = doReq(h, "GET", "/o/r/api/pushmirror", "")
	var v View
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.AuthKind != AuthSSH || v.PublicKey != out.PublicKey || !v.HasSecret {
		t.Errorf("post-keygen view = %+v", v)
	}
	_ = ctx
}

func TestSyncNowRequiresConfig(t *testing.T) {
	h, svc, ctx := testHandler(t, adminPrincipal)
	createRepoForHTTP(t, svc, ctx, "o", "r")
	w := doReq(h, "POST", "/o/r/api/pushmirror/sync", `{}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("sync without config = %d, want 404", w.Code)
	}
	w = doReq(h, "GET", "/o/r/api/pushmirror/sync", "")
	if w.Code != http.StatusOK {
		t.Fatalf("sync status = %d", w.Code)
	}
}

func TestNonMirrorPathsFallThrough(t *testing.T) {
	h, _, _ := testHandler(t, adminPrincipal)
	if h.Handle(httptest.NewRecorder(), httptest.NewRequest("GET", "/o/r/api/refs", nil)) {
		t.Error("refs path claimed by pushmirror handler")
	}
	w := doReq(h, "GET", "/o/r/api/pushmirror", "")
	_ = w
}

func TestHandleCoversExposedRoutes(t *testing.T) {
	h, _, _ := testHandler(t, adminPrincipal)
	// Every served route shape (both lanes) is claimed; the canonical
	// template list covers exactly these shapes (no phantoms, nothing
	// missing — the #272 two-way contract, minimal form).
	claimed := []struct{ method, path string }{
		{"GET", "/o/r/api/pushmirror"},
		{"PUT", "/o/r/api/pushmirror"},
		{"DELETE", "/o/r/api/pushmirror"},
		{"POST", "/o/r/api/pushmirror/sync"},
		{"GET", "/o/r/api/pushmirror/sync"},
		{"POST", "/o/r/api/pushmirror/keygen"},
		{"GET", "/o/r/api-browser/pushmirror"},
		{"PUT", "/o/r/api-browser/pushmirror"},
		{"DELETE", "/o/r/api-browser/pushmirror"},
		{"POST", "/o/r/api-browser/pushmirror/sync"},
		{"GET", "/o/r/api-browser/pushmirror/sync"},
		{"POST", "/o/r/api-browser/pushmirror/keygen"},
	}
	for _, c := range claimed {
		w := httptest.NewRecorder()
		if !h.Handle(w, httptest.NewRequest(c.method, c.path, nil)) {
			t.Errorf("%s %s not claimed", c.method, c.path)
		}
	}
	// Non-shapes fall through to the core mux.
	for _, path := range []string{"/o/r/api/pushmirror/extra", "/o/r/api/other", "/api/v1/repos/mirrors"} {
		if h.Handle(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil)) {
			t.Errorf("%s claimed", path)
		}
	}
}

func TestExposedTemplatesExact(t *testing.T) {
	want := []string{
		"/{owner}/{repo}/api/pushmirror",
		"/{owner}/{repo}/api/pushmirror/sync",
		"/{owner}/{repo}/api/pushmirror/keygen",
	}
	if len(ExposedTemplates) != len(want) {
		t.Fatalf("ExposedTemplates = %v", ExposedTemplates)
	}
	for i := range want {
		if ExposedTemplates[i] != want[i] {
			t.Errorf("ExposedTemplates[%d] = %q, want %q", i, ExposedTemplates[i], want[i])
		}
	}
}

func TestBodyHasKey(t *testing.T) {
	if !bodyHasKey([]byte(`{"schedule":""}`), "schedule") {
		t.Error("present empty key missed")
	}
	if bodyHasKey([]byte(`{"other":1}`), "schedule") {
		t.Error("absent key found")
	}
	if bodyHasKey(nil, "schedule") {
		t.Error("nil body has key")
	}
}
