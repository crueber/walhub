package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func tmpDataDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "walhub-data-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func writeConfig(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadExplicitMissingFileIsFatal(t *testing.T) {
	_, _, err := LoadExplicit("/nonexistent/walgit.toml", func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "config file not found: /nonexistent/walgit.toml") {
		t.Fatalf("err = %v, want fatal config file not found", err)
	}
}

func TestLoadExplicitDevNullIsDefaultsPlusEnv(t *testing.T) {
	dir := tmpDataDir(t)
	env := func(key string) string {
		if key == "WALHUB_DATA_DIR" {
			return dir
		}
		return ""
	}
	c, _, err := LoadExplicit(DevNullConfigPath, env)
	if err != nil {
		t.Fatalf("LoadExplicit(/dev/null): %v", err)
	}
	// Compiled-in defaults, NOT first-run defaults: the operator explicitly
	// declined file config (§3.1 step 2).
	if c.Server.Listen != "127.0.0.1:8080" || c.Store.Backend != "s3" {
		t.Fatalf("listen=%q backend=%q, want compiled-in defaults", c.Server.Listen, c.Store.Backend)
	}
	if c.DataDir != dir {
		t.Fatalf("DataDir = %q, want %q", c.DataDir, dir)
	}
}

func TestLoadMissingDefaultFileIsFirstRun(t *testing.T) {
	dir := tmpDataDir(t)
	env := func(key string) string {
		if key == "WALHUB_DATA_DIR" {
			return dir
		}
		return ""
	}
	c, _, err := Load([]string{filepath.Join(dir, ConfigFileName), filepath.Join(dir, ConfigAliasName)}, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Server.Listen != "0.0.0.0:8080" {
		t.Fatalf("listen = %q, want 0.0.0.0:8080", c.Server.Listen)
	}
	if c.Store.Backend != "filesystem" || c.Store.Root != filepath.Join(dir, "store") {
		t.Fatalf("store = %s @ %s, want filesystem @ <data-dir>/store", c.Store.Backend, c.Store.Root)
	}
	if c.Cache.Dir != filepath.Join(dir, "cache") {
		t.Fatalf("cache.dir = %q, want <data-dir>/cache", c.Cache.Dir)
	}
	if c.Server.Auth.Mode != "none" || !c.Server.Auth.AnonymousRead {
		t.Fatalf("auth = %s (anon=%v), want none + anonymous_read", c.Server.Auth.Mode, c.Server.Auth.AnonymousRead)
	}
	if !c.Server.AutoCreateOnPush {
		t.Fatal("auto_create_on_push = false, want true on first run")
	}
	if c.DataDir != dir {
		t.Fatalf("DataDir = %q, want %q", c.DataDir, dir)
	}
}

func TestLoadWalgitTomlAlias(t *testing.T) {
	dir := tmpDataDir(t)
	writeConfig(t, dir, ConfigAliasName, "[server]\nlisten = \"127.0.0.1:9999\"\n")
	env := func(key string) string {
		if key == "WALHUB_DATA_DIR" {
			return dir
		}
		return ""
	}
	c, _, err := Load([]string{filepath.Join(dir, ConfigFileName), filepath.Join(dir, ConfigAliasName)}, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Server.Listen != "127.0.0.1:9999" {
		t.Fatalf("listen = %q, want 127.0.0.1:9999 (walgit.toml alias)", c.Server.Listen)
	}
	// Non-first-run: compiled-in defaults for unmarked keys.
	if c.Server.AutoCreateOnPush {
		t.Fatal("auto_create_on_push = true, want compiled-in false when a file exists")
	}
}

func TestLoadFilePlusEnvWins(t *testing.T) {
	dir := tmpDataDir(t)
	writeConfig(t, dir, ConfigFileName, "[store]\nbucket = \"from-file\"\n")
	path := filepath.Join(dir, ConfigFileName)
	t.Setenv("WALHUB__STORE__BUCKET", "from-env")
	t.Setenv("WALHUB_DATA_DIR", dir)
	t.Setenv("PORT", "9999")
	c, _, err := Load([]string{path}, os.Getenv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Store.Bucket != "from-env" {
		t.Fatalf("store.bucket = %q, want env to win", c.Store.Bucket)
	}
	if c.Server.Listen != "127.0.0.1:9999" {
		t.Fatalf("listen = %q, want PORT lockstep", c.Server.Listen)
	}
}

func TestLoadPortLockstepRewritesLoopbackPublicURL(t *testing.T) {
	dir := tmpDataDir(t)
	writeConfig(t, dir, ConfigFileName, `
[server]
listen = "127.0.0.1:8080"
public_url = "http://127.0.0.1:8080"
`)
	t.Setenv("WALHUB_DATA_DIR", dir)
	t.Setenv("PORT", "9000")
	c, _, err := Load([]string{filepath.Join(dir, ConfigFileName)}, os.Getenv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Server.Listen != "127.0.0.1:9000" || c.Server.PublicURL != "http://127.0.0.1:9000" {
		t.Fatalf("listen=%q url=%q, want both port-locked", c.Server.Listen, c.Server.PublicURL)
	}
}

func TestLoadUnknownKeyListsKeyAndLine(t *testing.T) {
	dir := tmpDataDir(t)
	path := writeConfig(t, dir, ConfigFileName, "[server]\nlisten = \"127.0.0.1:8080\"\nbogus_key = 1\n")
	_, _, err := Load([]string{path}, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), `unknown key "server.bogus_key" (line 3)`) {
		t.Fatalf("err = %v, want unknown key with line", err)
	}
}
func TestLoadInvalidValidationFails(t *testing.T) {
	dir := tmpDataDir(t)
	path := writeConfig(t, dir, ConfigFileName, "[server.auth]\nmode = \"oidc\"\n")
	_, _, err := Load([]string{path}, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "anonymous_read") {
		t.Fatalf("err = %v, want fail-closed oidc violation", err)
	}
}

func TestConfigFilePointer(t *testing.T) {
	tests := []struct {
		name    string
		environ []string
		want    string
		ok      bool
	}{
		{name: "none", environ: nil, want: "", ok: false},
		{name: "primary", environ: []string{"WALHUB_CONFIG=/etc/walhub.toml"}, want: "/etc/walhub.toml", ok: true},
		{name: "legacy fallback", environ: []string{"WALGIT_CONFIG=/etc/walgit.toml"}, want: "/etc/walgit.toml", ok: true},
		{name: "primary beats legacy", environ: []string{"WALHUB_CONFIG=/a.toml", "WALGIT_CONFIG=/b.toml"}, want: "/a.toml", ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ConfigFilePointer(getenvFrom(tt.environ))
			if got != tt.want || ok != tt.ok {
				t.Fatalf("got %q,%v want %q,%v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestResolveDataDir(t *testing.T) {
	if got := ResolveDataDir(getenvFrom([]string{"WALHUB_DATA_DIR=/data"})); got != "/data" {
		t.Fatalf("WALHUB_DATA_DIR ignored: %q", got)
	}
	got := ResolveDataDir(func(string) string { return "" })
	if !filepath.IsAbs(got) {
		t.Fatalf("default data dir must be absolute: %q", got)
	}
}

func TestFirstRunDefaultsDoesNotTouchCompiledDefaults(t *testing.T) {
	before := Defaults()
	_ = FirstRunDefaults("/somewhere")
	after := Defaults()
	if before.Server.Listen != after.Server.Listen || before.Store.Backend != after.Store.Backend {
		t.Fatal("FirstRunDefaults mutated the compiled-in defaults")
	}
}

func TestMarshalTOMLRoundTrip(t *testing.T) {
	body, err := MarshalTOML(Defaults())
	if err != nil {
		t.Fatalf("MarshalTOML: %v", err)
	}
	var got Config
	if _, err := toml.Decode(string(body), &got); err != nil {
		t.Fatalf("re-decode: %v\n%s", err, body)
	}
	if got.Server.Listen != "127.0.0.1:8080" {
		t.Fatalf("listen = %q", got.Server.Listen)
	}
	if got.Server.MaxPushBytes != 64<<30 || got.Cache.MaxBytes != 20<<30 {
		t.Fatalf("sizes did not round-trip: push=%d cache=%d", got.Server.MaxPushBytes, got.Cache.MaxBytes)
	}
	if got.Server.Auth.SessionTTL != Duration(30*24*time.Hour) {
		t.Fatalf("session_ttl = %v", got.Server.Auth.SessionTTL)
	}
	if strings.Contains(string(body), "DataDir") {
		t.Fatal("DataDir must not be serialized (toml:\"-\")")
	}
}

func TestSaveSetupWritesAtomicallyWith0600(t *testing.T) {
	dir := tmpDataDir(t)
	c := FirstRunDefaults(dir)
	if err := SaveSetup(c, dir); err != nil {
		t.Fatalf("SaveSetup: %v", err)
	}
	path := filepath.Join(dir, "walhub.toml")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("saved file missing: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %v, want 0600", perm)
	}
	if _, err := os.Stat(filepath.Join(dir, "walhub.toml.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tmp file left behind: %v", err)
	}
	// The saved file joins the ordinary ladder.
	loaded, _, err := Load([]string{path}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("re-load saved config: %v", err)
	}
	if loaded.Server.Listen != "0.0.0.0:8080" || loaded.Store.Backend != "filesystem" {
		t.Fatalf("loaded listen=%q backend=%q", loaded.Server.Listen, loaded.Store.Backend)
	}
}

func TestSaveSetupRejectsInvalidConfig(t *testing.T) {
	dir := tmpDataDir(t)
	c := FirstRunDefaults(dir)
	c.Server.Auth.Mode = "oidc" // anonymous_read still true → invalid
	if err := SaveSetup(c, dir); err == nil {
		t.Fatal("SaveSetup must refuse an invalid config")
	}
	if _, err := os.Stat(filepath.Join(dir, "walhub.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid submission must write nothing")
	}
}

func TestRestartRequired(t *testing.T) {
	for _, key := range []string{"telemetry.log_format", "telemetry.log_filter", "maintenance.interval", "maintenance.follow_interval", "wal.freshness_ttl"} {
		if RestartRequired(key) {
			t.Errorf("RestartRequired(%q) = true, want read-live", key)
		}
	}
	for _, key := range []string{"store.backend", "server.listen", "server.auth.mode", "cache.dir"} {
		if !RestartRequired(key) {
			t.Errorf("RestartRequired(%q) = false, want restart-required", key)
		}
	}
}

// --- §4 per-repo settings ---

func TestParseRepoSettingsHappyPath(t *testing.T) {
	payload := []byte(`
[bundles]
main_only = false
min_commits = 5

[compaction]
trigger_packs = 8

[integrations]
whatever = { nested = "verbatim" }
`)
	rs, err := ParseRepoSettings(payload)
	if err != nil {
		t.Fatalf("ParseRepoSettings: %v", err)
	}
	if rs.Bundles == nil || rs.Bundles.MainOnly || rs.Bundles.MinCommits != 5 {
		t.Fatalf("bundles = %+v", rs.Bundles)
	}
	if rs.Compaction == nil || rs.Compaction.TriggerPacks != 8 {
		t.Fatalf("compaction = %+v", rs.Compaction)
	}
	if rs.Maintenance != nil || rs.Upstream != nil {
		t.Fatal("unset sections must stay nil")
	}
}

func TestParseRepoSettingsRejects(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr string
	}{
		{name: "host-only section", payload: "[server]\nlisten = \"x\"\n", wantErr: "not settable via settings"},
		{name: "store section", payload: "[store]\nbucket = \"x\"\n", wantErr: "not settable via settings"},
		{name: "wal section", payload: "[wal]\nmax_batch = 1\n", wantErr: "not settable via settings"},
		{name: "cache section", payload: "[cache]\ndir = \"/x\"\n", wantErr: "not settable via settings"},
		{name: "upstream token_env", payload: "[upstream]\ntoken_env = \"SECRET\"\n", wantErr: "host-only"},
		{name: "unknown section", payload: "[cats]\ncute = true\n", wantErr: "unknown key"},
		{name: "unknown key in allowed section", payload: "[bundles]\nnonsense = 1\n", wantErr: "unknown key"},
		{name: "too large", payload: "x" + strings.Repeat("# pad\n", 4096), wantErr: "limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRepoSettings([]byte(tt.payload))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestRepoSettingsMerge(t *testing.T) {
	base := Defaults()
	base.Upstream.TokenEnv = "HOST_TOKEN"
	rs, err := ParseRepoSettings([]byte(`
[bundles]
min_bytes = "4096B"

[upstream]
git = "https://upstream.example.com/repo.git"
`))
	if err != nil {
		t.Fatalf("ParseRepoSettings: %v", err)
	}
	merged, err := rs.Merge(base)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	// Pointer-set sections replace the host section wholesale ("with_settings").
	if merged.Bundles.MinBytes != 4096 || merged.Bundles.MainOnly {
		t.Fatalf("merged bundles = %+v", merged.Bundles)
	}
	if merged.Upstream.Git != "https://upstream.example.com/repo.git" {
		t.Fatalf("upstream.git = %q", merged.Upstream.Git)
	}
	if merged.Upstream.TokenEnv != "HOST_TOKEN" {
		t.Fatalf("upstream.token_env = %q, want host value preserved", merged.Upstream.TokenEnv)
	}
	if base.Bundles.MinBytes == 4096 {
		t.Fatal("Merge must not mutate the base config")
	}
}

func TestRepoSettingsValidateAgainst(t *testing.T) {
	base := Defaults()
	rs, err := ParseRepoSettings([]byte("[bundles]\n[[bundles.strategy]]\nname = \"late\"\nkind = \"incremental\"\nbase = \"missing\"\nschedule = \"0 0 * * * *\"\n"))
	if err != nil {
		t.Fatalf("ParseRepoSettings: %v", err)
	}
	if err := rs.ValidateAgainst(base); err == nil || !strings.Contains(err.Error(), "does not name an earlier-declared strategy") {
		t.Fatalf("ValidateAgainst = %v, want fail-closed bundle violation", err)
	}

	ok, err := ParseRepoSettings([]byte("[maintenance]\ninterval = \"120s\"\n"))
	if err != nil {
		t.Fatalf("ParseRepoSettings: %v", err)
	}
	if err := ok.ValidateAgainst(base); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
}

func TestLoadStatError(t *testing.T) {
	// A path whose Stat fails with something other than NotExist.
	_, _, err := Load([]string{"/etc/passwd/not-a-dir.toml"}, func(string) string { return "" })
	if err == nil || strings.Contains(err.Error(), "config file not found") {
		t.Fatalf("err = %v, want non-NotExist stat error", err)
	}
}

func TestLoadDirectoryAsConfigFails(t *testing.T) {
	dir := tmpDataDir(t)
	if _, _, err := Load([]string{dir}, func(string) string { return "" }); err == nil {
		t.Fatal("want error decoding a directory")
	}
}

func TestLoadTableHeaderUnknownKey(t *testing.T) {
	dir := tmpDataDir(t)
	path := writeConfig(t, dir, ConfigFileName, "[server]\nlisten = \"127.0.0.1:8080\"\n\n[server.bogus]\nx = 1\n")
	_, _, err := Load([]string{path}, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), `unknown key "server.bogus" (line 4)`) {
		t.Fatalf("err = %v, want unknown table with line", err)
	}
}

func TestLoadTopLevelUnknownSection(t *testing.T) {
	dir := tmpDataDir(t)
	path := writeConfig(t, dir, ConfigFileName, "[bogus_section]\nkey = 1\n")
	_, _, err := Load([]string{path}, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), `unknown key "bogus_section" (line 1)`) {
		t.Fatalf("err = %v, want unknown section with line", err)
	}
}

func TestSaveSetupMkdirFailure(t *testing.T) {
	dir := tmpDataDir(t)
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(blocker, "sub")
	c := FirstRunDefaults(sub)
	if err := SaveSetup(c, sub); err == nil {
		t.Fatal("want SaveSetup error when the data dir cannot be created")
	}
}

func TestRepoSettingsMergeAllSections(t *testing.T) {
	base := Defaults()
	rs, err := ParseRepoSettings([]byte(`
[maintenance]
interval = "120s"
[compaction]
trigger_packs = 8
[upstream]
git = "https://u.example.com/r.git"
lfs = "https://u.example.com/lfs"
follow = ["main", "next"]
`))
	if err != nil {
		t.Fatalf("ParseRepoSettings: %v", err)
	}
	merged, err := rs.Merge(base)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if merged.Maintenance.Interval != Duration(2*time.Minute) {
		t.Fatalf("maintenance.interval = %v", merged.Maintenance.Interval)
	}
	if merged.Compaction.TriggerPacks != 8 {
		t.Fatalf("compaction.trigger_packs = %d", merged.Compaction.TriggerPacks)
	}
	if merged.Upstream.Lfs != "https://u.example.com/lfs" || len(merged.Upstream.Follow) != 2 {
		t.Fatalf("upstream = %+v", merged.Upstream)
	}
}

func TestSaveSetupIOErrors(t *testing.T) {
	dir := tmpDataDir(t)
	c := FirstRunDefaults(dir)

	// walhub.toml.tmp already exists as a directory → write fails.
	if err := os.Mkdir(filepath.Join(dir, "walhub.toml.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SaveSetup(c, dir); err == nil || !strings.Contains(err.Error(), "write config") {
		t.Fatalf("err = %v, want write config error", err)
	}

	// walhub.toml exists as a directory → rename fails.
	if err := os.Mkdir(filepath.Join(dir, "walhub.toml"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SaveSetup(c, dir); err == nil || !strings.Contains(err.Error(), "rename") {
		t.Fatalf("err = %v, want rename error", err)
	}
}

func TestResolveDataDirNoHome(t *testing.T) {
	t.Setenv("WALHUB_DATA_DIR", "")
	t.Setenv("HOME", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if got := ResolveDataDir(func(string) string { return "" }); got != "/var/lib/walhub" {
		t.Fatalf("no-home data dir = %q, want /var/lib/walhub", got)
	}
}

func TestLoadNilGetenvAndEmptyCandidate(t *testing.T) {
	dir := tmpDataDir(t)
	path := writeConfig(t, dir, ConfigFileName, "[server]\nlisten = \"127.0.0.1:8080\"\n")
	c, _, err := Load([]string{"", path}, nil) // empty candidate skipped; nil getenv → os.Getenv
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Server.Listen != "127.0.0.1:8080" {
		t.Fatalf("listen=%q", c.Server.Listen)
	}
	if p, ok := ConfigFilePointer(nil); ok || p != "" {
		t.Fatalf("ConfigFilePointer(nil) = %q, %v", p, ok)
	}
	if got := ResolveDataDir(nil); !filepath.IsAbs(got) {
		t.Fatalf("ResolveDataDir(nil) = %q", got)
	}
}

func TestLoadEnvAndPortErrors(t *testing.T) {
	dir := tmpDataDir(t)
	path := writeConfig(t, dir, ConfigFileName, "[server]\nlisten = \"127.0.0.1:8080\"\n")

	t.Setenv("WALHUB_DATA_DIR", dir)
	t.Setenv("WALHUB__SERVER__ROLES", "serve") // bare string into []string → fatal
	if _, _, err := Load([]string{path}, os.Getenv); err == nil {
		t.Fatal("want env override error from Load")
	}
	os.Unsetenv("WALHUB__SERVER__ROLES")

	t.Setenv("PORT", "notaport")
	if _, _, err := Load([]string{path}, os.Getenv); err == nil {
		t.Fatal("want PORT error from Load")
	}
	os.Unsetenv("PORT")

	// Syntactically invalid TOML.
	bad := writeConfig(t, dir, "bad.toml", "listen = \n")
	if _, _, err := Load([]string{bad}, os.Getenv); err == nil {
		t.Fatal("want TOML parse error from Load")
	}
}
