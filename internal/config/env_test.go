package config

import (
	"strings"
	"testing"
	"time"
)

func getenvFrom(environ []string) func(string) string {
	return func(key string) string {
		for _, kv := range environ {
			if k, v, ok := strings.Cut(kv, "="); ok && k == key {
				return v
			}
		}
		return ""
	}
}

func applyEnvOn(t *testing.T, base *Config, environ ...string) (*Config, []Override, error) {
	t.Helper()
	if base == nil {
		base = Defaults()
	}
	c := *base
	overrides, err := applyEnv(&c, getenvFrom(environ), environ)
	return &c, overrides, err
}

func TestApplyEnvPrimaryPrefix(t *testing.T) {
	c, overrides, err := applyEnvOn(t, nil, "WALHUB__STORE__BUCKET=acme")
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if len(overrides) != 0 {
		t.Fatalf("overrides = %+v, want none", overrides)
	}
	if c.Store.Bucket != "acme" {
		t.Fatalf("store.bucket = %q, want acme", c.Store.Bucket)
	}
}

func TestApplyEnvLegacyAlias(t *testing.T) {
	c, _, err := applyEnvOn(t, nil, "WALGIT__STORE__BUCKET=legacy")
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if c.Store.Bucket != "legacy" {
		t.Fatalf("store.bucket = %q, want legacy", c.Store.Bucket)
	}
}

func TestApplyEnvPrimaryWinsOnConflict(t *testing.T) {
	c, overrides, err := applyEnvOn(t, nil, "WALGIT__STORE__BUCKET=old", "WALHUB__STORE__BUCKET=new")
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if c.Store.Bucket != "new" {
		t.Fatalf("store.bucket = %q, want new (WALHUB__ wins)", c.Store.Bucket)
	}
	if len(overrides) != 1 {
		t.Fatalf("overrides = %+v, want 1 ignored legacy override", overrides)
	}
	if overrides[0].Name != "WALGIT__STORE__BUCKET" || !strings.Contains(overrides[0].Reason, "overridden by WALHUB__") {
		t.Fatalf("override = %+v, want legacy ignored with reason", overrides[0])
	}
}

func TestApplyEnvNestedSection(t *testing.T) {
	c, _, err := applyEnvOn(t, nil, "WALHUB__SERVER__AUTH__MODE=oidc")
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if c.Server.Auth.Mode != "oidc" {
		t.Fatalf("server.auth.mode = %q, want oidc", c.Server.Auth.Mode)
	}
}

func TestApplyEnvTypedValues(t *testing.T) {
	c, overrides, err := applyEnvOn(t, nil,
		`WALHUB__SERVER__ROLES=["serve", "maintain"]`,
		"WALHUB__SERVER__MAX_CONCURRENT_REQUESTS=100",
		"WALHUB__CACHE__MAX_BYTES=64GiB",
		"WALHUB__WAL__BATCH_WINDOW=5ms",
		"WALHUB__CACHE__DISK_HIGH_WATERMARK=0.95",
		"WALHUB__STORE__ACCEL=true", // unknown key: ignored override, never fatal
	)
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	found := false
	for _, o := range overrides {
		if o.Key == "store.accel" && strings.Contains(o.Reason, "unknown key") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want ignored override for store.accel, got %+v", overrides)
	}
	want := []string{"serve", "maintain"}
	if len(c.Server.Roles) != 2 || c.Server.Roles[0] != want[0] || c.Server.Roles[1] != want[1] {
		t.Fatalf("server.roles = %v, want %v", c.Server.Roles, want)
	}
	if c.Server.MaxConcurrentRequests != 100 {
		t.Fatalf("max_concurrent_requests = %d, want 100", c.Server.MaxConcurrentRequests)
	}
	if c.Cache.MaxBytes != 64<<30 {
		t.Fatalf("cache.max_bytes = %d, want 64GiB", c.Cache.MaxBytes)
	}
	if c.WAL.BatchWindow != Duration(5*time.Millisecond) {
		t.Fatalf("wal.batch_window = %v, want 5ms", c.WAL.BatchWindow)
	}
	if c.Cache.DiskHighWatermark != 0.95 {
		t.Fatalf("disk_high_watermark = %v, want 0.95", c.Cache.DiskHighWatermark)
	}
}

func TestApplyEnvBareStringForArrayIsError(t *testing.T) {
	_, _, err := applyEnvOn(t, nil, "WALHUB__SERVER__ROLES=serve")
	if err == nil {
		t.Fatal("want error assigning bare string to []string")
	}
	for _, want := range []string{"server.roles", `"serve"`, "[]string"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestApplyEnvInlineTable(t *testing.T) {
	c, _, err := applyEnvOn(t, nil, `WALHUB__STORE__S3={endpoint = "http://127.0.0.1:9000", region = "eu-central-1"}`)
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if c.Store.S3.Endpoint != "http://127.0.0.1:9000" || c.Store.S3.Region != "eu-central-1" {
		t.Fatalf("store.s3 = %+v", c.Store.S3)
	}
}

func TestApplyEnvDateRejected(t *testing.T) {
	_, _, err := applyEnvOn(t, nil, "WALHUB__SERVER__LISTEN=1979-05-27")
	if err == nil || !strings.Contains(err.Error(), "dates") {
		t.Fatalf("want date rejection error, got %v", err)
	}
}

func TestApplyEnvStrategyIndexOverride(t *testing.T) {
	c, _, err := applyEnvOn(t, nil, "WALGIT__BUNDLES__STRATEGY__0__BACKFILL_MAX=1")
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if c.Bundles.Strategy[0].BackfillMax != 1 {
		t.Fatalf("strategy[0].backfill_max = %d, want 1", c.Bundles.Strategy[0].BackfillMax)
	}
	if c.Bundles.Strategy[0].Name != "weekly" {
		t.Fatalf("strategy[0].name = %q, want weekly (in-place mutation)", c.Bundles.Strategy[0].Name)
	}
}

func TestApplyEnvStrategyIndexOutOfRange(t *testing.T) {
	_, _, err := applyEnvOn(t, nil, "WALGIT__BUNDLES__STRATEGY__5__BACKFILL_MAX=1")
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("want out-of-range error, got %v", err)
	}
}

func TestApplyEnvPlacementAllOrNothing(t *testing.T) {
	base := Defaults()
	base.Placement = Placement{Serve: []string{"host-a/*"}, Maintain: []string{"host-a/*"}}
	c, _, err := applyEnvOn(t, base, `WALGIT__PLACEMENT__SERVE=["acme/*"]`)
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if len(c.Placement.Serve) != 1 || c.Placement.Serve[0] != "acme/*" {
		t.Fatalf("placement.serve = %v, want [acme/*]", c.Placement.Serve)
	}
	if len(c.Placement.Maintain) != 1 || c.Placement.Maintain[0] != "*" {
		t.Fatalf("placement.maintain = %v, want reset to [*]", c.Placement.Maintain)
	}
	if len(c.Placement.ServeExclude) != 0 || len(c.Placement.MaintainExclude) != 0 {
		t.Fatalf("placement excludes not defaulted: %+v", c.Placement)
	}
}

func TestApplyEnvLogFilterOverride(t *testing.T) {
	c, _, err := applyEnvOn(t, nil, "WALHUB_LOG=debug")
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if c.Telemetry.LogFilter != "debug" {
		t.Fatalf("log_filter = %q, want debug", c.Telemetry.LogFilter)
	}
	c, _, _ = applyEnvOn(t, nil, "WALHUB_LOG=debug", "RUST_LOG=warn")
	if c.Telemetry.LogFilter != "warn" {
		t.Fatalf("log_filter = %q, want warn (RUST_LOG wins)", c.Telemetry.LogFilter)
	}
}

func TestApplyEnvNonDoubleUnderscoreIgnored(t *testing.T) {
	c, overrides, err := applyEnvOn(t, nil, "WALGIT_TOKEN_ALICE=secret", "WALHUB_LOG=debug")
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if len(overrides) != 0 {
		t.Fatalf("overrides = %+v, want none", overrides)
	}
	if c.Telemetry.LogFilter != "debug" {
		t.Fatalf("log_filter = %q", c.Telemetry.LogFilter)
	}
}

func TestApplyPort(t *testing.T) {
	tests := []struct {
		name       string
		listen     string
		publicURL  string
		port       string
		wantListen string
		wantURL    string
		wantErr    bool
	}{
		{name: "no port", listen: "127.0.0.1:8080", port: "", wantListen: "127.0.0.1:8080"},
		{name: "port override", listen: "127.0.0.1:8080", port: "9000", wantListen: "127.0.0.1:9000"},
		{name: "loopback public url lockstep", listen: "127.0.0.1:8080", publicURL: "http://127.0.0.1:8080", port: "9000",
			wantListen: "127.0.0.1:9000", wantURL: "http://127.0.0.1:9000"},
		{name: "localhost url lockstep", listen: "[::1]:8080", publicURL: "http://localhost:8080", port: "9000",
			wantListen: "[::1]:9000", wantURL: "http://localhost:9000"},
		{name: "ipv6 loopback url lockstep", listen: "127.0.0.1:8080", publicURL: "http://[::1]:8080", port: "9000",
			wantListen: "127.0.0.1:9000", wantURL: "http://[::1]:9000"},
		{name: "public url untouched", listen: "0.0.0.0:8080", publicURL: "https://git.example.com", port: "9000",
			wantListen: "0.0.0.0:9000", wantURL: "https://git.example.com"},
		{name: "port out of range", listen: "127.0.0.1:8080", port: "70000", wantErr: true},
		{name: "port zero", listen: "127.0.0.1:8080", port: "0", wantErr: true},
		{name: "port not a number", listen: "127.0.0.1:8080", port: "http", wantErr: true},
		{name: "listen without port", listen: "127.0.0.1", port: "9000", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Defaults()
			c.Server.Listen = tt.listen
			c.Server.PublicURL = tt.publicURL
			err := applyPort(c, getenvFrom([]string{"PORT=" + tt.port}))
			if tt.wantErr {
				if err == nil {
					t.Fatal("applyPort: want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("applyPort: %v", err)
			}
			if c.Server.Listen != tt.wantListen {
				t.Fatalf("listen = %q, want %q", c.Server.Listen, tt.wantListen)
			}
			if c.Server.PublicURL != tt.wantURL {
				t.Fatalf("public_url = %q, want %q", c.Server.PublicURL, tt.wantURL)
			}
		})
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "127.8.8.8": true, "::1": true, "localhost": true, "LOCALHOST": true,
		"0.0.0.0": false, "git.example.com": false, "::": false, "": false,
	} {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestApplyEnvCoercionEdges(t *testing.T) {
	// Whole-array replacement via inline tables.
	c, _, err := applyEnvOn(t, nil,
		`WALGIT__BUNDLES__STRATEGY=[{name = "solo", kind = "full", schedule = "0 0 * * * *"}]`)
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if len(c.Bundles.Strategy) != 1 || c.Bundles.Strategy[0].Name != "solo" || c.Bundles.Strategy[0].Schedule != "0 0 * * * *" {
		t.Fatalf("strategy = %+v", c.Bundles.Strategy)
	}

	// Unknown key inside an inline table → ignored override, never fatal.
	c, overrides, err := applyEnvOn(t, nil, `WALHUB__STORE__S3={bogus = 1}`)
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if len(overrides) != 1 || !strings.Contains(overrides[0].Reason, "unknown key store.s3") {
		t.Fatalf("overrides = %+v, want unknown-key override", overrides)
	}
	if c.Store.S3.Region != "us-east-1" {
		t.Fatalf("region = %q, want default untouched", c.Store.S3.Region)
	}

	// Unsigned fields.
	c, _, err = applyEnvOn(t, nil, "WALGIT__WAL__SNAPSHOT_EVERY_ENTRIES=256")
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if c.WAL.SnapshotEveryEntries != 256 {
		t.Fatalf("snapshot_every_entries = %d", c.WAL.SnapshotEveryEntries)
	}
	if _, _, err = applyEnvOn(t, nil, "WALGIT__WAL__CAS_MAX_RETRIES=-1"); err == nil {
		t.Error("want error for negative unsigned")
	}

	// Typed fields reject booleans and floats.
	if _, _, err = applyEnvOn(t, nil, "WALGIT__WAL__BATCH_WINDOW=true"); err == nil {
		t.Error("want error for boolean duration")
	}
	if _, _, err = applyEnvOn(t, nil, "WALGIT__WAL__BATCH_WINDOW=1.5"); err == nil {
		t.Error("want error for fractional duration")
	}
}

func TestApplyEnvTypedParseErrors(t *testing.T) {
	for _, env := range []string{"WALGIT__CACHE__MAX_BYTES=notasize", "WALGIT__CACHE__EVICT_IDLE_AFTER=nonsense"} {
		if _, _, err := applyEnvOn(t, nil, env); err == nil {
			t.Errorf("%s: want typed parse error", env)
		}
	}
}

func TestApplyEnvFloatTargetRejectsString(t *testing.T) {
	if _, _, err := applyEnvOn(t, nil, "WALGIT__CACHE__DISK_HIGH_WATERMARK=abc"); err == nil {
		t.Error("want type error for non-numeric watermark")
	}
}

func TestApplyEnvEdgePaths(t *testing.T) {
	c, overrides, err := applyEnvOn(t, nil,
		"WALHUB__=ignored-empty-name",          // empty path: skipped
		"WALHUB__STORE__BUCKET=first",          // duplicate primary
		"WALHUB__STORE__BUCKET=second",         // duplicate primary: first wins
		"WALHUB__CACHE__MAX_BYTES=123",         // bare int → applyText int64
		"WALGIT__CACHE__DISK_HIGH_WATERMARK=1", // int64 into float
		"WALHUB__STORE__PREFIX=",               // empty value → empty string
		"WALHUB__PLACEMENT__BOGUS=1",           // unknown placement key → ignored override
		"WALGIT__WAL__MAX_BATCH=32",            // non-placement key (placement pass skips it)
	)
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if c.Store.Bucket != "first" {
		t.Fatalf("store.bucket = %q, want first (duplicate primary: first wins)", c.Store.Bucket)
	}
	if c.Store.Prefix != "" {
		t.Fatalf("store.prefix = %q, want empty", c.Store.Prefix)
	}
	if c.WAL.MaxBatch != 32 {
		t.Fatalf("wal.max_batch = %d, want 32", c.WAL.MaxBatch)
	}
	if len(overrides) == 0 {
		t.Fatal("want ignored overrides")
	}
	for _, o := range overrides {
		switch o.Key {
		case "store.s3", "server.listen", "placement.bogus", "server.roles":
		default:
			t.Errorf("unexpected override %+v", o)
		}
	}
}

func TestApplyEnvCoercionErrors(t *testing.T) {
	fatal := []string{
		"WALHUB__WAL__CAS_MAX_RETRIES=abc",   // uint target rejects string
		"WALHUB__SERVER__HTTP2=1",            // bool target rejects integer
		`WALGIT__SERVER__LISTEN=true`,        // string target rejects bool
		`WALGIT__SERVER__ROLES=["serve", 1]`, // array element type error
		"WALGIT__STORE__S3=x",                // struct target rejects scalar
		"WALGIT__STORE__S3={endpoint = 1}",   // struct field type error
	}
	for _, env := range fatal {
		if _, _, err := applyEnvOn(t, nil, env); err == nil {
			t.Errorf("%s: want coercion error", env)
		}
	}
}

func TestApplyEnvPlacementFatalError(t *testing.T) {
	if _, _, err := applyEnvOn(t, nil, "WALHUB__PLACEMENT__SERVE=acme"); err == nil {
		t.Fatal("want fatal type error for bare string into []string")
	}
}

func TestApplyEnvNilGetenv(t *testing.T) {
	t.Setenv("RUST_LOG", "trace")
	c := Defaults()
	environ := []string{"WALGIT__STORE__BUCKET=legacy"}
	overrides, err := applyEnv(c, nil, environ)
	if err != nil {
		t.Fatalf("applyEnv(nil getenv): %v", err)
	}
	if c.Store.Bucket != "legacy" || c.Telemetry.LogFilter != "trace" {
		t.Fatalf("bucket=%q filter=%q", c.Store.Bucket, c.Telemetry.LogFilter)
	}
	if len(overrides) != 0 {
		t.Fatalf("overrides = %+v", overrides)
	}
}
