// datadir_sync_test.go — Forgejo #611: --data-dir must re-point the
// flag-derived paths (DataDir, Store.Root, Cache.Dir) for every subcommand,
// while explicit file values and WALHUB__* env overlay win.
package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
)

// resolveWithEnv runs resolveConfig with the process environment (t.Setenv
// isolation per case) and a --data-dir flag value.
func resolveWithEnv(t *testing.T, dataDir string) (*config.Config, string) {
	t.Helper()
	c := &cli{dataDir: dataDir}
	cfg, state, err := resolveConfig(c)
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	return cfg, state
}

// TestDataDirFlagSyncMatrix is the #611 flag/env/file matrix. The env default
// data dir is pinned via WALHUB_DATA_DIR so ResolveDataDir is hermetic.
func TestDataDirFlagSyncMatrix(t *testing.T) {
	envDD := t.TempDir()
	t.Setenv("WALHUB_DATA_DIR", envDD)

	t.Run("flag repoints first-run store and cache", func(t *testing.T) {
		flagDD := t.TempDir()
		cfg, state := resolveWithEnv(t, flagDD)
		if state != stateAbsent {
			t.Fatalf("state = %q, want absent (no file in flag dir)", state)
		}
		if cfg.DataDir != flagDD {
			t.Fatalf("DataDir = %q, want %q", cfg.DataDir, flagDD)
		}
		if want := filepath.Join(flagDD, "store"); cfg.Store.Root != want {
			t.Fatalf("Store.Root = %q, want %q", cfg.Store.Root, want)
		}
		if want := filepath.Join(flagDD, "cache"); cfg.Cache.Dir != want {
			t.Fatalf("Cache.Dir = %q, want %q", cfg.Cache.Dir, want)
		}
	})

	t.Run("no flag leaves env-derived paths alone", func(t *testing.T) {
		cfg, _ := resolveWithEnv(t, "")
		if cfg.DataDir != envDD {
			t.Fatalf("DataDir = %q, want %q", cfg.DataDir, envDD)
		}
		if want := filepath.Join(envDD, "store"); cfg.Store.Root != want {
			t.Fatalf("Store.Root = %q, want %q", cfg.Store.Root, want)
		}
		if want := filepath.Join(envDD, "cache"); cfg.Cache.Dir != want {
			t.Fatalf("Cache.Dir = %q, want %q", cfg.Cache.Dir, want)
		}
	})

	t.Run("explicit file store.root wins over flag", func(t *testing.T) {
		flagDD := t.TempDir()
		custom := t.TempDir()
		body := "[store]\nbackend = \"filesystem\"\nroot = " + quoteTOML(custom) + "\n"
		if err := os.WriteFile(filepath.Join(flagDD, "walhub.toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, state := resolveWithEnv(t, flagDD)
		if state != statePresent {
			t.Fatalf("state = %q, want present", state)
		}
		if cfg.Store.Root != custom {
			t.Fatalf("Store.Root = %q, want explicit %q", cfg.Store.Root, custom)
		}
		if cfg.DataDir != flagDD {
			t.Fatalf("DataDir = %q, want %q", cfg.DataDir, flagDD)
		}
		// Cache had no file value: compiled-in default, not an env-derived
		// path, so the sync leaves it — exactly like serve (openStore and
		// the registry consume cfg.Cache.Dir identically on both paths).
		if cfg.Cache.Dir != "/tmp/walgit" {
			t.Fatalf("Cache.Dir = %q, want compiled default /tmp/walgit", cfg.Cache.Dir)
		}
	})

	t.Run("explicit file cache.dir wins over flag", func(t *testing.T) {
		flagDD := t.TempDir()
		custom := t.TempDir()
		body := "[cache]\ndir = " + quoteTOML(custom) + "\n"
		if err := os.WriteFile(filepath.Join(flagDD, "walhub.toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, _ := resolveWithEnv(t, flagDD)
		if cfg.Cache.Dir != custom {
			t.Fatalf("Cache.Dir = %q, want explicit %q", cfg.Cache.Dir, custom)
		}
	})

	t.Run("env overlay wins over flag", func(t *testing.T) {
		flagDD := t.TempDir()
		envRoot := t.TempDir()
		envCache := t.TempDir()
		t.Setenv("WALHUB__STORE__ROOT", quoteBare(envRoot))
		t.Setenv("WALHUB__CACHE__DIR", quoteBare(envCache))
		cfg, _ := resolveWithEnv(t, flagDD)
		if cfg.Store.Root != envRoot {
			t.Fatalf("Store.Root = %q, want env %q", cfg.Store.Root, envRoot)
		}
		if cfg.Cache.Dir != envCache {
			t.Fatalf("Cache.Dir = %q, want env %q", cfg.Cache.Dir, envCache)
		}
		if cfg.DataDir != flagDD {
			t.Fatalf("DataDir = %q, want %q", cfg.DataDir, flagDD)
		}
	})

	t.Run("explicit --config plus flag syncs too", func(t *testing.T) {
		flagDD := t.TempDir()
		file := filepath.Join(t.TempDir(), "custom.toml")
		if err := os.WriteFile(file, []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}
		c := &cli{configPath: file, dataDir: flagDD}
		cfg, state, err := resolveConfig(c)
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		if state != statePresent {
			t.Fatalf("state = %q, want present", state)
		}
		if cfg.DataDir != flagDD {
			t.Fatalf("DataDir = %q, want %q", cfg.DataDir, flagDD)
		}
		// An (empty) explicit file means compiled-in defaults, not
		// first-run defaults: Store.Root is "" (openStore falls back to
		// the flag dir at open time) and Cache.Dir is the compiled
		// default — the sync only moves env-default-derived paths,
		// exactly like serve.
		if cfg.Store.Root != "" {
			t.Fatalf("Store.Root = %q, want empty (compiled default)", cfg.Store.Root)
		}
		if cfg.Cache.Dir != "/tmp/walgit" {
			t.Fatalf("Cache.Dir = %q, want compiled default /tmp/walgit", cfg.Cache.Dir)
		}
	})

	t.Run("env-file data dir also syncs to flag", func(t *testing.T) {
		envDD := t.TempDir()
		t.Setenv("WALHUB_DATA_DIR", envDD)
		flagDD := t.TempDir()
		fileDD := t.TempDir()
		c := &cli{dataDir: flagDD}
		mf := multiFlag{pairs: map[string]string{"WALHUB_DATA_DIR": fileDD}}
		cfg, state, err := loadWithEnvFiles(c, mf)
		if err != nil {
			t.Fatalf("loadWithEnvFiles: %v", err)
		}
		if state != stateAbsent {
			t.Fatalf("state = %q, want absent", state)
		}
		// The ladder derived paths from the env-file dir; the flag sync
		// (using the same merged getenv) must still re-point them.
		if cfg.DataDir != flagDD {
			t.Fatalf("DataDir = %q, want %q", cfg.DataDir, flagDD)
		}
		if want := filepath.Join(flagDD, "store"); cfg.Store.Root != want {
			t.Fatalf("Store.Root = %q, want %q", cfg.Store.Root, want)
		}
		if want := filepath.Join(flagDD, "cache"); cfg.Cache.Dir != want {
			t.Fatalf("Cache.Dir = %q, want %q", cfg.Cache.Dir, want)
		}
	})
}

// TestServeFixupIdempotent pins that re-running the serve.go fixup over an
// already-synced config is a no-op (the redundant pass #611 keeps).
func TestServeFixupIdempotent(t *testing.T) {
	envDD := t.TempDir()
	t.Setenv("WALHUB_DATA_DIR", envDD)
	flagDD := t.TempDir()
	cfg, _ := resolveWithEnv(t, flagDD)
	before := *cfg
	envDataDir := config.ResolveDataDir(os.Getenv)
	cfg.DataDir = flagDD
	if cfg.Store.Root == filepath.Join(envDataDir, "store") {
		cfg.Store.Root = filepath.Join(flagDD, "store")
	}
	if cfg.Cache.Dir == filepath.Join(envDataDir, "cache") {
		cfg.Cache.Dir = filepath.Join(flagDD, "cache")
	}
	if !reflect.DeepEqual(*cfg, before) {
		t.Fatalf("second fixup pass changed the config: %+v -> %+v", before, *cfg)
	}
}

func quoteTOML(s string) string { return "\"" + s + "\"" }

// quoteBare returns s in TOML-value syntax for the env overlay (a plain
// absolute path needs double quotes to parse as a string).
func quoteBare(s string) string { return "\"" + s + "\"" }
