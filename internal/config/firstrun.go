package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FirstRunDefaults returns the zero-config boot configuration (divergence D5,
// 11_config_cli.md §2.3): a fresh machine serves git over HTTP with no file.
// Every unmarked key keeps its compiled-in §2 table value.
func FirstRunDefaults(dataDir string) *Config {
	c := Defaults()
	c.DataDir = dataDir
	c.Server.Listen = "0.0.0.0:8080"
	c.Server.AutoCreateOnPush = true
	c.Store.Backend = "filesystem"
	c.Store.Root = filepath.Join(dataDir, "store")
	c.Cache.Dir = filepath.Join(dataDir, "cache")
	c.Server.Auth.Mode = "none"
	return c
}

// LoadSetupBase returns the file-visible base config that setup edits on top
// of (§3.4): compiled-in defaults ⊕ the first existing candidate file — no env
// overlay, no PORT lockstep (those stay env-side and are never baked into the
// file). Candidates are checked in priority order (an explicit --config path
// first, then the data-dir files, mirroring the §3.1 ladder). With no
// candidate present the zero-config first-run shape is the base, so a first
// save cannot silently swap the filesystem store for the s3 default or move
// the listener off 0.0.0.0. An unparseable file returns the error; the caller
// falls back to FirstRunDefaults (the file's own errors surface separately
// through the boot state).
func LoadSetupBase(dataDir string, candidates []string) (*Config, error) {
	c := Defaults()
	loaded := false
	for _, path := range candidates {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if err := decodeFile(path, c); err != nil {
			return nil, err
		}
		loaded = true
		break
	}
	if !loaded {
		c = FirstRunDefaults(dataDir)
	}
	c.DataDir = dataDir
	return c, nil
}

// ResolveDataDir resolves the data dir (11_config_cli.md §3.1.1): the
// WALHUB_DATA_DIR env var, else /var/lib/walhub in a container context
// (/.dockerenv present or KUBERNETES_SERVICE_HOST set), else the XDG default
// ~/.local/share/walhub.
func ResolveDataDir(getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if d := getenv("WALHUB_DATA_DIR"); d != "" {
		return d
	}
	if IsContainer() {
		return "/var/lib/walhub"
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "/var/lib/walhub"
	}
	return filepath.Join(home, ".local", "share", "walhub")
}

// IsContainer reports whether the process appears to run in a container.
func IsContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return os.Getenv("KUBERNETES_SERVICE_HOST") != ""
}

// EnsureDataDir creates the data dir (mode 0700) if missing — §3.1.1.
func EnsureDataDir(dataDir string) error {
	return os.MkdirAll(dataDir, 0o700)
}

// --- Setup save semantics (§3.7, divergence D6) ---

// readLiveKeys are picked up on the next tick/emit without a restart; every
// other key is consumed once at subsystem construction and needs a restart.
var readLiveKeys = []string{
	"telemetry.log_format",
	"telemetry.log_filter",
	"maintenance.interval",
	"maintenance.follow_interval",
	"wal.freshness_ttl",
}

// RestartRequired reports whether a key takes effect only after a restart
// (the default) or is read live from the effective config.
func RestartRequired(key string) bool {
	for _, k := range readLiveKeys {
		if k == strings.ToLower(key) {
			return false
		}
	}
	return true
}

// ValidateForSave runs the full §5 fail-closed validation; invalid input
// writes nothing.
func ValidateForSave(c *Config) error {
	_, errs := Validate(c)
	if len(errs) == 0 {
		return nil
	}
	msgs := make([]string, len(errs))
	for i, err := range errs {
		msgs[i] = err.Error()
	}
	return errors.New(strings.Join(msgs, "; "))
}

// SaveSetup validates c (§5), then atomically serializes it to
// <data-dir>/walhub.toml: write walhub.toml.tmp (mode 0600), fsync, rename.
// Readers see either the old or the new file, never a partial one.
func SaveSetup(c *Config, dataDir string) error {
	if err := ValidateForSave(c); err != nil {
		return err
	}
	if err := EnsureDataDir(dataDir); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	body, err := MarshalTOML(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmpPath := filepath.Join(dataDir, "walhub.toml.tmp")
	if err := writeFileSync(tmpPath, body); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmpPath, filepath.Join(dataDir, "walhub.toml")); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename config into place: %w", err)
	}
	return nil
}

// writeFileSync writes body with mode 0600 and fsyncs before returning.
func writeFileSync(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(body)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
