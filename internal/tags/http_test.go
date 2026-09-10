package tags

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

func doReq(t *testing.T, x *harness, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	x.handler.ServeHTTP(w, req)
	return w
}

func writerHeaders() map[string]string {
	return map[string]string{"X-Test-Principal": "jane"}
}

func TestHandleTable(t *testing.T) {
	newBody := func() string { return `{"name":"v1","sha":"` + testSHA + `"}` }
	cases := []struct {
		name       string
		setup      func(x *harness)
		method     string
		target     string
		body       func() string
		headers    map[string]string
		wantStatus int
		wantBody   string // substring; "" skips
	}{
		{"happy 201", grantWrite, "POST", "/o/r/api/tags", newBody, writerHeaders(), 201, `"ref":"refs/tags/v1"`},
		{"browser lane 201", grantWrite, "POST", "/o/r/api-browser/tags", newBody, writerHeaders(), 201, `"name":"v1"`},
		{"git suffix repo 201", grantWrite, "POST", "/o/r.git/api/tags", newBody, writerHeaders(), 201, `"sha":"` + testSHA + `"`},
		{"get 405", grantWrite, "GET", "/o/r/api/tags", nil, writerHeaders(), 405, ""},
		{"put 405", grantWrite, "PUT", "/o/r/api/tags", newBody, writerHeaders(), 405, ""},
		{"delete 405", grantWrite, "DELETE", "/o/r/api/tags", nil, writerHeaders(), 405, ""},
		{"anonymous 401", nil, "POST", "/o/r/api/tags", newBody, nil, 401, ""},
		{"reader 403", func(x *harness) { x.roles.grant("o", "r", "jane", "read") }, "POST", "/o/r/api/tags", newBody, writerHeaders(), 403, ""},
		{"bad name 400", grantWrite, "POST", "/o/r/api/tags", func() string { return `{"name":"bad name","sha":"` + testSHA + `"}` }, writerHeaders(), 400, "invalid ref name"},
		{"empty name 400", grantWrite, "POST", "/o/r/api/tags", func() string { return `{"sha":"` + testSHA + `"}` }, writerHeaders(), 400, "must not be empty"},
		{"unknown sha 404", grantWrite, "POST", "/o/r/api/tags", func() string { return `{"name":"v1","sha":"ffffffffffffffffffffffffffffffffffffffff"}` }, writerHeaders(), 404, "unknown revision"},
		{"empty sha 400", grantWrite, "POST", "/o/r/api/tags", func() string { return `{"name":"v1","sha":""}` }, writerHeaders(), 400, "must not be empty"},
		{"annotated message 201", grantWrite, "POST", "/o/r/api/tags", func() string { return `{"name":"v1","sha":"` + testSHA + `","message":"release one"}` }, writerHeaders(), 201, `"sha":"` + testTagOid + `"`},
		{"annotated oversize 400", grantWrite, "POST", "/o/r/api/tags", func() string {
			return `{"name":"v1","sha":"` + testSHA + `","message":"` + strings.Repeat("x", MaxTagMessageLen+1) + `"}`
		}, writerHeaders(), 400, "exceeds"},
		{"annotated nul 400", grantWrite, "POST", "/o/r/api/tags", func() string {
			return "{\"name\":\"v1\",\"sha\":\"" + testSHA + "\",\"message\":\"hi\\u0000there\"}"
		}, writerHeaders(), 400, "NUL"},
		{"unknown field 400", grantWrite, "POST", "/o/r/api/tags", func() string { return `{"name":"v1","sha":"` + testSHA + `","bogus":1}` }, writerHeaders(), 400, "unknown field"},
		{"invalid json 400", grantWrite, "POST", "/o/r/api/tags", func() string { return `{"name":` }, writerHeaders(), 400, "invalid JSON"},
		{"null body 400", grantWrite, "POST", "/o/r/api/tags", func() string { return `null` }, writerHeaders(), 400, "expected an object"},
		{"not tags 404", grantWrite, "GET", "/o/r/api/nope", nil, writerHeaders(), 404, ""},
		{"top-level api 404", grantWrite, "GET", "/api/tags", nil, writerHeaders(), 404, ""},
		{"extra segment 404", grantWrite, "POST", "/o/r/api/tags/extra", newBody, writerHeaders(), 404, ""},
		{"bad repo id 404", grantWrite, "POST", "/BAD%20OWNER/r/api/tags", newBody, writerHeaders(), 404, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := newHarness(t)
			if tc.setup != nil {
				tc.setup(x)
			}
			var body string
			if tc.body != nil {
				body = tc.body()
			}
			w := doReq(t, x, tc.method, tc.target, body, tc.headers)
			if w.Code != tc.wantStatus {
				t.Fatalf("%s %s = %d (%q), want %d", tc.method, tc.target, w.Code, w.Body.String(), tc.wantStatus)
			}
			if tc.wantBody != "" && !strings.Contains(w.Body.String(), tc.wantBody) {
				t.Fatalf("body %q lacks %q", w.Body.String(), tc.wantBody)
			}
			if tc.wantStatus == 201 {
				if ct := w.Header().Get("Content-Type"); ct != "application/json" {
					t.Fatalf("content-type = %q", ct)
				}
				if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
					t.Fatalf("cache-control = %q", cc)
				}
			}
		})
	}
}

func TestHandleConflict(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.refs.existing["v1"] = testSHA
	w := doReq(t, x, "POST", "/o/r/api/tags", `{"name":"v1","sha":"`+testSHA+`"}`, writerHeaders())
	if w.Code != 409 {
		t.Fatalf("= %d (%q), want 409", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "already exists") {
		t.Fatalf("body %q lacks conflict wording", w.Body.String())
	}
}

// TestHandleAnnotatedRecords is the wire proof for #263: a POST with a
// message takes the annotated path end to end (201 with the tag object oid),
// records the PUSH-shaped publish (tag oid + peeled commit + pack), and the
// mktag body carries the tagger line and message.
func TestHandleAnnotatedRecords(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	w := doReq(t, x, "POST", "/o/r/api/tags", `{"name":"v2","sha":"`+testSHA+`","message":"release two"}`, writerHeaders())
	if w.Code != 201 {
		t.Fatalf("= %d (%q), want 201", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"sha":"`+testTagOid+`"`) {
		t.Fatalf("body %q lacks tag object oid", w.Body.String())
	}
	if len(x.refs.annotated) != 1 {
		t.Fatalf("annotated = %d, want 1", len(x.refs.annotated))
	}
	a := x.refs.annotated[0]
	if a.name != "v2" || a.tagOid != testTagOid || a.peeled != testSHA || len(a.pack) == 0 {
		t.Fatalf("annotated = %+v", a)
	}
	if len(x.git.tagBodies) != 1 {
		t.Fatalf("mktag bodies = %d, want 1", len(x.git.tagBodies))
	}
	body := string(x.git.tagBodies[0])
	for _, want := range []string{"object " + testSHA, "type commit", "tag v2", "tagger jane <jane@walhub.local>", "release two"} {
		if !strings.Contains(body, want) {
			t.Fatalf("mktag body %q lacks %q", body, want)
		}
	}
}

// TestHandleAnnotatedConflict proves the annotated path races like the
// lightweight one: create-against-present is a CAS 409, never a move.
func TestHandleAnnotatedConflict(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.refs.existing["v2"] = testSHA
	w := doReq(t, x, "POST", "/o/r/api/tags", `{"name":"v2","sha":"`+testSHA+`","message":"release two"}`, writerHeaders())
	if w.Code != 409 {
		t.Fatalf("= %d (%q), want 409", w.Code, w.Body.String())
	}
}

func TestHandleAuthErrors(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	kinds := []struct {
		kind auth.AuthErrorKind
		want int
	}{
		{auth.ErrInvalid, 401},
		{auth.ErrForbidden, 403},
		{auth.ErrUnavailable, 503},
	}
	for _, k := range kinds {
		x.handler.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
			return auth.Principal{}, &auth.AuthError{Kind: k.kind, Why: "test gate"}
		}
		w := doReq(t, x, "POST", "/o/r/api/tags", `{"name":"v1","sha":"`+testSHA+`"}`, nil)
		if w.Code != k.want {
			t.Fatalf("kind %v = %d, want %d", k.kind, w.Code, k.want)
		}
	}
	// 401 carries the Bearer challenge git needs to erase the credential.
	x.handler.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "nope"}
	}
	w := doReq(t, x, "POST", "/o/r/api/tags", `{"name":"v1","sha":"`+testSHA+`"}`, nil)
	if got := w.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("401 must carry WWW-Authenticate")
	}
}

func TestHandleNonTagsPaths(t *testing.T) {
	x := newHarness(t)
	// Short path (no repo scope) and single-segment owner never claim.
	for _, target := range []string{"/o/r/api", "/o/api/tags", "/o"} {
		req := httptest.NewRequest("GET", target, nil)
		if x.handler.Handle(httptest.NewRecorder(), req) {
			t.Fatalf("Handle(%q) = true, want false", target)
		}
	}
}
