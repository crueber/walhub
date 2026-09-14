package api

// anonwrite502_test.go — Forgejo #502: anonymous visitors execute zero
// writes. Resolution-matrix grid over (principal × inventoried write route):
// every write refuses an anonymous principal with a consistent 401
// (WWW-Authenticate: Bearer), and authenticated rows are unchanged.
//
// Defect A pin: POST/DELETE /api/v1/ssh-keys from anonymous must 401 WITHOUT
// touching the registry. Defect B pin: gate() answers 401 (never 403) for
// anonymous on AuthWrite/AuthAdmin even with anonymous_read on.

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// anonWriteRoutes is the inventoried write surface (the #502 table): every
// entry must refuse anonymous with 401.
var anonWriteRoutes502 = []struct {
	name   string
	method string
	path   string
	body   string
}{
	{"repo PUT", "PUT", "/demo/walgit/api", ""},
	{"repo DELETE", "DELETE", "/demo/walgit/api", ""},
	{"settings PUT", "PUT", "/demo/walgit/api/settings", "[bundles]\n"},
	{"settings DELETE", "DELETE", "/demo/walgit/api/settings", ""},
	{"policy PUT", "PUT", "/demo/walgit/api/policy", "{}"},
	{"policy DELETE", "DELETE", "/demo/walgit/api/policy", ""},
	{"ops POST", "POST", "/demo/walgit/api/ops/sync", ""},
	{"owner profile PUT", "PUT", "/api/v1/owners/demo/profile", "{}"},
	{"ssh-keys POST", "POST", "/api/v1/ssh-keys", `{"key": "` + validKeyLine + `"}`},
	{"ssh-keys DELETE", "DELETE", "/api/v1/ssh-keys/fp-1", ""},
}

func TestAnonWriteMatrix502(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.env.Cfg.Server.Auth.AnonymousRead = true // the #345 OIDC anonymous-read mode
	stub := &stubSSHKeys{recs: map[string]SshKeyRecord{}}
	f.env.SSHKeys = stub

	for _, tc := range anonWriteRoutes502 {
		w := f.do(tc.method, tc.path, bodyOf(tc.body), nil, nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: anonymous status = %d, want 401", tc.name, w.Code)
		}
		if h := w.Header().Get("WWW-Authenticate"); h != `Bearer realm="walgit"` {
			t.Errorf("%s: www-authenticate = %q, want Bearer realm", tc.name, h)
		}
	}
	if len(stub.recs) != 0 {
		t.Fatalf("anonymous ssh-keys calls wrote %d records", len(stub.recs))
	}
}

// bodyOf renders the matrix body ("" = no body, never a nil-typed reader).
func bodyOf(s string) io.Reader {
	if s == "" {
		return nil
	}
	return strings.NewReader(s)
}

// TestAnonWriteGateStatus502 pins defect B: anonymous on AuthWrite/AuthAdmin
// answers 401 even with anonymous_read on — never 403 "insufficient role".
func TestAnonWriteGateStatus502(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.env.Cfg.Server.Auth.AnonymousRead = true
	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"AuthWrite", "POST", "/demo/walgit/api/ops/sync"},
		{"AuthAdmin", "PUT", "/demo/walgit/api/settings"},
	} {
		w := f.do(tc.method, tc.path, nil, nil, nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: anonymous status = %d, want 401 (not 403)", tc.name, w.Code)
		}
		if strings.Contains(w.Body.String(), "required") && w.Code != http.StatusUnauthorized {
			t.Errorf("%s: body = %q", tc.name, w.Body.String())
		}
	}
}

// TestAuthedWriteRowsUnchanged502 pins the other matrix dimension:
// authenticated principals see exactly the old verdicts.
func TestAuthedWriteRowsUnchanged502(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.env.Cfg.Server.Auth.AnonymousRead = true
	stub := &stubSSHKeys{recs: map[string]SshKeyRecord{}}
	f.env.SSHKeys = stub

	// Read-only principal: writes still 403 (authenticated-but-insufficient).
	if w := f.do("POST", "/demo/walgit/api/ops/sync", nil, nil, readP()); w.Code != http.StatusForbidden {
		t.Errorf("read-only ops POST = %d, want 403", w.Code)
	}
	// Read-only principal keeps ssh-keys self-service (AuthRead + explicit
	// anonymous check only).
	if w := f.do("POST", "/api/v1/ssh-keys", strings.NewReader(`{"key": "`+validKeyLine+`"}`), nil, readP()); w.Code != http.StatusCreated {
		t.Errorf("read-only ssh-keys POST = %d, want 201", w.Code)
	}
	// Full-access principal: writes proceed past the gate (unknown op →
	// 404 from the handler, not 401/403 from the gate; the sync op itself
	// would open a task stream the fake never closes).
	admin := &auth.Principal{Name: "root", Write: true, Admin: true}
	if w := f.do("POST", "/demo/walgit/api/ops/bogus-op", nil, nil, admin); w.Code != http.StatusNotFound {
		t.Errorf("admin ops POST = %d, want 404 past the gate", w.Code)
	}
	if w := f.do("DELETE", "/demo/walgit/api", nil, nil, admin); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
		t.Errorf("admin repo DELETE = %d, want past the gate", w.Code)
	}
}

// TestSSHKeysAnonLeakClosed502 is the defect-A pin: the anonymous POST that
// used to write a key record for the synthetic "anonymous" principal now
// 401s and the registry is untouched; anonymous DELETE 401s too.
func TestSSHKeysAnonLeakClosed502(t *testing.T) {
	f := newFixture(t)
	f.env.Cfg.Server.Auth.AnonymousRead = true
	stub := &stubSSHKeys{recs: map[string]SshKeyRecord{}}
	f.env.SSHKeys = stub

	w := f.do("POST", "/api/v1/ssh-keys", strings.NewReader(`{"key": "`+validKeyLine+`", "title": "anon"}`), nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous ssh-keys POST = %d, want 401", w.Code)
	}
	if len(stub.recs) != 0 {
		t.Fatalf("anonymous POST wrote %d key records", len(stub.recs))
	}
	w = f.do("DELETE", "/api/v1/ssh-keys/fp-1", nil, nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous ssh-keys DELETE = %d, want 401", w.Code)
	}

	// Auth-none keeps working: its principal is auth.None(), never anonymous.
	f.env.Cfg.Server.Auth.Mode = "none"
	w = f.do("POST", "/api/v1/ssh-keys", strings.NewReader(`{"key": "`+validKeyLine+`", "title": "laptop"}`), nil, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("none-mode ssh-keys POST = %d, want 201", w.Code)
	}
}
