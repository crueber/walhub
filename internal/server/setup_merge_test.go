package server

// Setup editing semantics (§3.4): PUT/POST /api/v1/setup edit the CURRENT
// file-visible configuration — untouched keys keep their values, the
// zero-config first-run shape is the base when no file exists, and the
// standalone setup surface stays open whenever auth mode is "none".

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/web"
)

func setupMergeServer(t *testing.T, dataDir string) (*Server, http.Handler) {
	t.Helper()
	paths := []string{filepath.Join(dataDir, config.ConfigFileName)}
	// The effective config mirrors a real boot: defaults ⊕ file (first-run
	// shape when no file exists).
	cfg, err := config.LoadSetupBase(dataDir, paths)
	if err != nil {
		t.Fatal(err)
	}
	s, h := newTestServer(t, func(o *Options) {
		o.Config = cfg
	})
	s.cfg.DataDir = dataDir
	s.boot.ConfigPaths = paths
	s.boot.Mode = "normal"
	return s, h
}

func putSetup(t *testing.T, s *Server, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("PUT", "/api/v1/setup", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.setupPut(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// A save must EDIT the current file: untouched keys keep their values.
func TestSetupPutEditsCurrentFile(t *testing.T) {
	dataDir := t.TempDir()
	base := config.FirstRunDefaults(dataDir)
	base.Server.Listen = "127.0.0.1:9443"
	base.Server.Auth.Mode = "none"
	base.Server.Auth.AnonymousRead = true
	if err := config.SaveSetup(base, dataDir); err != nil {
		t.Fatal(err)
	}
	s, _ := setupMergeServer(t, dataDir)

	code, body := putSetup(t, s, `{"overrides": {"server.request_timeout": "2h"}}`)
	if code != http.StatusOK {
		t.Fatalf("put = %d %v", code, body)
	}
	after, err := config.LoadSetupBase(dataDir, s.boot.ConfigPaths)
	if err != nil {
		t.Fatal(err)
	}
	if after.Server.Listen != "127.0.0.1:9443" {
		t.Fatalf("untouched listen reset: %q", after.Server.Listen)
	}
	if after.Server.Auth.Mode != "none" {
		t.Fatalf("untouched auth mode reset: %q", after.Server.Auth.Mode)
	}
	if time.Duration(after.Server.RequestTimeout) != 2*time.Hour {
		t.Fatalf("override not applied: %v", after.Server.RequestTimeout)
	}
	rr, _ := body["requires_restart"].([]any)
	if len(rr) != 1 || rr[0] != "server.request_timeout" {
		t.Fatalf("requires_restart = %v, want exactly the edited key", body["requires_restart"])
	}
}

// With no config file the base is the first-run shape, NOT the compiled-in
// defaults — a first save must not swap the filesystem store for s3 or move
// the listener off 0.0.0.0.
func TestSetupPutFirstRunKeepsFirstRunShape(t *testing.T) {
	dataDir := t.TempDir()
	s, _ := setupMergeServer(t, dataDir)
	s.boot.Mode = "defaults"
	if err := os.Remove(filepath.Join(dataDir, config.ConfigFileName)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	code, body := putSetup(t, s, `{"overrides": {"server.request_timeout": "30m"}}`)
	if code != http.StatusOK {
		t.Fatalf("put = %d %v", code, body)
	}
	after, err := config.LoadSetupBase(dataDir, s.boot.ConfigPaths)
	if err != nil {
		t.Fatal(err)
	}
	if after.Store.Backend != "filesystem" || after.Server.Listen != "0.0.0.0:8080" || !after.Server.AutoCreateOnPush {
		t.Fatalf("first-run shape lost: backend=%q listen=%q auto=%t",
			after.Store.Backend, after.Server.Listen, after.Server.AutoCreateOnPush)
	}
	if time.Duration(after.Server.RequestTimeout) != 30*time.Minute {
		t.Fatalf("override not applied: %v", after.Server.RequestTimeout)
	}
}

// The merged config is what gets validated: a request that is only valid
// against the CURRENT file must pass, and one that breaks the merge must 422.
func TestSetupTestValidatesMergedConfig(t *testing.T) {
	dataDir := t.TempDir()
	base := config.FirstRunDefaults(dataDir)
	base.Server.Auth.Mode = "oidc"
	base.Server.Auth.AnonymousRead = false
	base.Server.Auth.Issuer = "https://id.example.com"
	base.Server.Auth.AllowedDomains = []string{"example.com"}
	base.Server.Auth.OAuthClientID = "walhub"
	base.Server.Auth.OAuthClientSecret = "s3cret"
	if err := config.SaveSetup(base, dataDir); err != nil {
		t.Fatal(err)
	}
	s, _ := setupMergeServer(t, dataDir)
	t.Setenv("WALHUB_SETUP_TOKEN", "setup-tok")

	// mode=oidc alone is only valid because the FILE supplies issuer/secret.
	req := httptest.NewRequest("POST", "/api/v1/setup/test?token=setup-tok",
		strings.NewReader(`{"overrides": {"server.auth.mode": "oidc"}}`))
	rec := httptest.NewRecorder()
	s.setupTest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("merged validate = %d %s (file keys must satisfy the request)", rec.Code, rec.Body.String())
	}

	// Breaking the merge: clearing the file's oauth_client_secret leaves
	// client_id set — "both set or both unset" fails only against the merge.
	req = httptest.NewRequest("POST", "/api/v1/setup/test?token=setup-tok",
		strings.NewReader(`{"overrides": {"server.auth.oauth_client_secret": ""}}`))
	rec = httptest.NewRecorder()
	s.setupTest(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid merge = %d %s", rec.Code, rec.Body.String())
	}
}

// Browsers attach Origin to every state-changing request, same-origin ones
// included; the CORS gate must allow an Origin that names the request's own
// host (regression: the setup Save button 403'd from the SPA at zero-config).
func TestCORSAllowsSameOriginWrites(t *testing.T) {
	if !sameOriginHost("http://walgit.localhost:8080", "walgit.localhost:8080") {
		t.Fatal("same origin must match")
	}
	if !sameOriginHost("https://hub.test/hub", "hub.test") {
		t.Fatal("origin with a path must still match its host")
	}
	if sameOriginHost("http://evil.test", "hub.test") {
		t.Fatal("foreign origin must not match")
	}
	if sameOriginHost("hub.test", "hub.test") {
		t.Fatal("origin without a scheme is malformed, never same-origin")
	}

	dataDir := t.TempDir()
	s, h := setupMergeServer(t, dataDir)
	s.cfg.Server.Auth.Mode = "none"
	s.cfg.Server.Auth.AnonymousRead = true
	s.boot.Mode = "normal"
	req := httptest.NewRequest("PUT", "http://walgit.localhost:8080/api/v1/setup",
		strings.NewReader(`{"overrides": {"server.request_timeout": "2h"}}`))
	req.Header.Set("Origin", "http://walgit.localhost:8080")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin PUT = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// Coverage for the setup save coercion and merge error paths (the gate is
// ≥95% per package; these branches guard against silent truncation).
func TestSetupCoerceErrorPaths(t *testing.T) {
	c := config.Defaults()
	rv := reflect.ValueOf(c).Elem()

	server := rv.FieldByName("Server")

	// Duration: bare integer = seconds (§2 parser rule); garbage → error.
	f := server.FieldByName("RequestTimeout")
	if err := configCoerce(f, "45", "server.request_timeout"); err != nil || time.Duration(f.Int()) != 45*time.Second {
		t.Fatalf("bare int duration = %v err=%v", time.Duration(f.Int()), err)
	}
	if err := configCoerce(f, "not-a-time", "server.request_timeout"); err == nil {
		t.Fatal("garbage duration must fail")
	}
	// ByteSize: scientific notation from JSON numbers; non-integral → error.
	b := server.FieldByName("MaxPushBytes")
	if err := configCoerce(b, "1.073741824e+10", "server.max_push_bytes"); err != nil || b.Int() != 10737418240 {
		t.Fatalf("byte size = %d err=%v", b.Int(), err)
	}
	if err := configCoerce(b, "1.5", "server.max_push_bytes"); err == nil {
		t.Fatal("fractional size must fail")
	}
	// Bool: false is a real value; garbage fails.
	bv := server.FieldByName("AutoCreateOnPush")
	if err := configCoerce(bv, "false", "server.auto_create_on_push"); err != nil || bv.Bool() {
		t.Fatalf("bool false = %v err=%v", bv.Bool(), err)
	}
	if err := configCoerce(bv, "maybe", "server.auto_create_on_push"); err == nil {
		t.Fatal("garbage bool must fail")
	}
	// A slice of non-strings is unsupported; a struct is not a value.
	tokens := server.FieldByName("Auth").FieldByName("Tokens")
	if err := configCoerce(tokens, "x", "server.auth.tokens"); err == nil {
		t.Fatal("struct slice must be unsupported")
	}
	authStruct := server.FieldByName("Auth")
	if err := configCoerce(authStruct, "x", "server.auth"); err == nil {
		t.Fatal("struct coercion must fail")
	}
	// Path walking: a section used as a value; a scalar used as a section.
	if err := configSetPath(c, []string{"server", "auth"}, "x"); err == nil {
		t.Fatal("section-as-value must fail")
	}
	if err := configSetPath(c, []string{"server", "http2", "deep"}, "x"); err == nil {
		t.Fatal("scalar-as-section must fail")
	}
	if err := configSetPath(c, []string{"server"}, "x"); err == nil {
		t.Fatal("single segment must fail")
	}
	// parseSettingInt edge: fractional input.
	if _, err := parseSettingInt("1.5"); err == nil {
		t.Fatal("fractional must fail")
	}
	// effectiveValue with a malformed key → nil.
	if v := effectiveValue(c, "nodot"); v != nil {
		t.Fatalf("malformed key = %v, want nil", v)
	}
}

// The TOML decode failure must surface as an error (a nil error with a nil
// config would crash the caller).
func TestParseSetupBodyTOMLDecodeError(t *testing.T) {
	req := httptest.NewRequest("PUT", "/api/v1/setup", strings.NewReader("[server\nbroken"))
	if _, err := parseSetupBody(req, config.Defaults()); err == nil {
		t.Fatal("broken TOML must error")
	}
	// JSON list values take the default formatting branch.
	req = httptest.NewRequest("PUT", "/api/v1/setup",
		strings.NewReader(`{"overrides": {"server.cors_origins": ["a.test", "b.test"]}}`))
	c, err := parseSetupBody(req, config.Defaults())
	if err != nil || len(c.Server.CorsOrigins) != 2 {
		t.Fatalf("list override = %v err=%v", c, err)
	}
}

// setupBaseConfig falls back to the first-run shape when the candidate file
// cannot be parsed (a directory in the candidate list forces a decode error).
func TestSetupBaseConfigFallsBackOnBadFile(t *testing.T) {
	dataDir := t.TempDir()
	s, _ := setupMergeServer(t, dataDir)
	s.boot.ConfigPaths = []string{dataDir} // a directory: stat ok, decode fails
	base := s.setupBaseConfig()
	if base.Store.Backend != "filesystem" || base.DataDir != dataDir {
		t.Fatalf("fallback base = backend %q dir %q", base.Store.Backend, base.DataDir)
	}
}

// Duration and byte-size values round-trip: the schema serves "1h0m0s" /
// plain integers, and Sscanf-style parsing would truncate both.
// The setup UI sends the same spellings the TOML file accepts: sizes as
// "64MiB", durations with the d/w suffixes, floats and unsigned counters as
// plain numbers, and struct slices (bundles.strategy) as a [[strategy]]
// fragment. Every one must round-trip through the overrides channel — these
// previously failed with "not an int"/"unsupported type", so the page could
// not save its own edits.
func TestSetupPutAcceptsUISpellings(t *testing.T) {
	dataDir := t.TempDir()
	s, _ := setupMergeServer(t, dataDir)

	fragment := "[[strategy]]\nname = \"weekly\"\nkind = \"full\"\nschedule = \"0 0 23 * * 0\"\nkeep = 2"
	body, err := json.Marshal(map[string]any{"overrides": map[string]any{
		"server.max_push_bytes":      "2GiB",
		"maintenance.fsck_interval":  "24h",
		"wal.snapshot_every_entries": 1000,
		"cache.disk_high_watermark":  "0.9",
		"bundles.strategy":           fragment,
	}})
	if err != nil {
		t.Fatal(err)
	}
	code, resp := putSetup(t, s, string(body))
	if code != http.StatusOK {
		t.Fatalf("put = %d %v", code, resp)
	}
	after, err := config.LoadSetupBase(dataDir, s.boot.ConfigPaths)
	if err != nil {
		t.Fatal(err)
	}
	if int64(after.Server.MaxPushBytes) != 2<<30 {
		t.Fatalf("size coerced wrong: %d", after.Server.MaxPushBytes)
	}
	if time.Duration(after.Maintenance.FsckInterval) != 24*time.Hour {
		t.Fatalf("d/w duration coerced wrong: %v", after.Maintenance.FsckInterval)
	}
	if after.WAL.SnapshotEveryEntries != 1000 {
		t.Fatalf("uint coerced wrong: %d", after.WAL.SnapshotEveryEntries)
	}
	if after.Cache.DiskHighWatermark != 0.9 {
		t.Fatalf("float coerced wrong: %v", after.Cache.DiskHighWatermark)
	}
	if len(after.Bundles.Strategy) != 1 || after.Bundles.Strategy[0].Name != "weekly" ||
		after.Bundles.Strategy[0].Kind != "full" || after.Bundles.Strategy[0].Keep != 2 {
		t.Fatalf("strategy fragment decoded wrong: %+v", after.Bundles.Strategy)
	}
}

// plain integers, and Sscanf-style parsing would truncate both.
func TestSetupCoerceDurationsAndSizes(t *testing.T) {
	dataDir := t.TempDir()
	s, _ := setupMergeServer(t, dataDir)

	code, body := putSetup(t, s,
		`{"overrides": {"server.request_timeout": "90m", "server.max_push_bytes": 68719476736}}`)
	if code != http.StatusOK {
		t.Fatalf("put = %d %v", code, body)
	}
	after, err := config.LoadSetupBase(dataDir, s.boot.ConfigPaths)
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(after.Server.RequestTimeout) != 90*time.Minute {
		t.Fatalf("duration coerced wrong: %v", after.Server.RequestTimeout)
	}
	if int64(after.Server.MaxPushBytes) != 68719476736 {
		t.Fatalf("byte size coerced wrong: %d", after.Server.MaxPushBytes)
	}
}

// The setup surface is open at ANY time while auth mode is "none" — first
// run, normal running mode, and setup-only mode alike — with no credential.
func TestSetupOpenWheneverAuthNone(t *testing.T) {
	for _, mode := range []string{"defaults", "normal", "setup_only"} {
		t.Run(mode, func(t *testing.T) {
			dataDir := t.TempDir()
			s, h := setupMergeServer(t, dataDir)
			s.cfg.Server.Auth.Mode = "none"
			s.cfg.Server.Auth.AnonymousRead = true
			s.boot.Mode = mode

			for _, path := range []string{"/setup", "/api/v1/setup"} {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x"+path, nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("GET %s in %s mode = %d, want open (body %.80s)", path, mode, rec.Code, rec.Body.String())
				}
			}
			// And the save path answers too (a valid override, no credential).
			code, body := putSetup(t, s, `{"overrides": {"server.request_timeout": "45m"}}`)
			if code != http.StatusOK {
				t.Fatalf("PUT in %s mode = %d %v", mode, code, body)
			}
		})
	}
}

// The built SPA assets must be served with the right caching class: hashed
// vite bundles immutable, the shell no-cache (D-WEB-6). Regression for the
// coverage gap after the SolidJS cutover.
func TestServeUIAssetsCaching(t *testing.T) {
	s, h := setupMergeServer(t, t.TempDir())
	s.cfg.Server.Auth.Mode = "none"
	s.cfg.Server.Auth.AnonymousRead = true
	s.boot.Mode = "normal"

	// find a real hashed bundle in the embed (make web ran before tests)
	entries, err := webFilesGlob("dist/assets")
	if err != nil || len(entries) == 0 {
		t.Skipf("no built assets (run make web): %v", err)
	}
	jsName := ""
	for _, e := range entries {
		if strings.HasSuffix(e, ".js") {
			jsName = strings.TrimPrefix(e, "dist/")
			break
		}
	}
	if jsName == "" {
		t.Skip("no js asset built")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/_ui/"+jsName, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("asset = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("hashed asset cache-control = %q, want immutable", cc)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Fatalf("hashed asset content-type = %q", ct)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/_ui/index.html", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("index = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("shell cache-control = %q, want no-cache", cc)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/_ui/nope.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown asset = %d", rec.Code)
	}
}

func webFilesGlob(dir string) ([]string, error) {
	var out []string
	err := fs.WalkDir(web.FilesFS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(p, dir+"/") {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}
