// config.go — config resolution shared by every subcommand (11_config_cli.md
// §3/§6.1) plus `config check` / `config dump`.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"git.packden.us/crueber/walhub/internal/config"
)

// fileState names the three boot states (§3.7, config dump).
const (
	statePresent = "present"
	stateAbsent  = "absent"
	stateInvalid = "invalid"
)

// resolveConfig runs the §3.1 ladder for a subcommand:
//   - explicit --config (or WALHUB_CONFIG / WALGIT_CONFIG pointer): a missing
//     file is fatal (the caller maps it onto exit 2);
//   - else <data-dir>/walhub.toml then walgit.toml: a missing default file is
//     the zero-config first run, NOT an error.
//
// It returns the effective config, the file state, and the load error (non-nil
// for invalid files or a missing explicit file).
func resolveConfig(c *cli) (*config.Config, string, error) {
	explicit := c.configPath
	if explicit == "" {
		if ptr, ok := config.ConfigFilePointer(os.Getenv); ok {
			explicit = ptr
		}
	}
	if explicit != "" {
		cfg, _, err := config.LoadExplicit(explicit, os.Getenv)
		if err != nil {
			if strings.Contains(err.Error(), "config file not found") {
				return nil, stateAbsent, err // missing explicit file (§6.1 exit 2)
			}
			return nil, stateInvalid, err
		}
		return cfg, statePresent, nil
	}
	cands := defaultConfigPaths(c)
	cfg2, _, err := config.Load(cands, os.Getenv)
	if err != nil {
		return nil, stateInvalid, err
	}
	for _, p := range cands {
		if _, statErr := os.Stat(p); statErr == nil {
			return cfg2, statePresent, nil
		}
	}
	return cfg2, stateAbsent, nil
}

// defaultConfigPaths is [<data-dir>/walhub.toml, <data-dir>/walgit.toml].
func defaultConfigPaths(c *cli) []string {
	dd := c.dataDir
	if dd == "" {
		dd = config.ResolveDataDir(os.Getenv)
	}
	return []string{filepath.Join(dd, config.ConfigFileName), filepath.Join(dd, config.ConfigAliasName)}
}

// dataDirFor resolves the data dir for a command.
func dataDirFor(c *cli) string {
	if c.dataDir != "" {
		return c.dataDir
	}
	return config.ResolveDataDir(os.Getenv)
}

// exitCodeForLoad maps a resolveConfig error onto the §6.1 codes.
func exitCodeForLoad(state string, err error) (int, string) {
	if state == stateAbsent && err != nil {
		return exitArg, err.Error() // missing explicitly-named file
	}
	return exitErr, err.Error() // invalid config surfaced by a command
}

// ---- config check -------------------------------------------------------------

// runConfigCheck validates file ⊕ env; ignored overrides go to stderr;
// --strict exits 3 when any override was ignored (§6).
func runConfigCheck(ctx context.Context, c *cli, args []string) int {
	fs := newFlagSet("config check")
	var strict bool
	var envFiles multiFlag
	fs.BoolVar(&strict, "strict", false, "exit 3 when any override was ignored")
	fs.Var(&envFiles, "env-file", "load KEY=VALUE pairs into the override set (repeatable)")
	_ = mustParse(fs, args)
	// --env-file values are injected into the environment snapshot the loader
	// reads (they do NOT touch the real environment).
	cfg, _, err := loadWithEnvFiles(c, envFiles)
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
		return exitErr
	}
	warnings, errs := config.Validate(cfg)
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning: "+w)
	}
	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, "error: "+e.Error())
		}
		return exitErr
	}
	if strict && len(envFiles.ignored) > 0 {
		for _, o := range envFiles.ignored {
			fmt.Fprintln(os.Stderr, "warning: ignored override "+o)
		}
		return exitStrict
	}
	return exitOK
}

// loadWithEnvFiles is resolveConfig with --env-file pairs layered under the
// real environment (KEY=VALUE lines; comments and blanks ignored).
func loadWithEnvFiles(c *cli, envFiles multiFlag) (*config.Config, string, error) {
	if len(envFiles.pairs) == 0 {
		return resolveConfig(c)
	}
	merged := map[string]string{}
	for k, v := range envFiles.pairs {
		merged[k] = v
	}
	// The loader reads the environment through os.Getenv; the env-file pairs
	// are appended as ignored-override-free context by re-running the ladder
	// with a getenv closure.
	explicit := c.configPath
	if explicit == "" {
		if ptr, ok := config.ConfigFilePointer(os.Getenv); ok {
			explicit = ptr
		}
	}
	getenv := func(key string) string {
		if v, ok := merged[key]; ok {
			return v
		}
		return os.Getenv(key)
	}
	if explicit != "" {
		cfg, _, err := config.LoadExplicit(explicit, getenv)
		if err != nil {
			if strings.Contains(err.Error(), "config file not found") {
				return nil, stateAbsent, err
			}
			return nil, stateInvalid, err
		}
		return cfg, statePresent, nil
	}
	cands := defaultConfigPaths(c)
	cfg, _, err := config.Load(cands, getenv)
	if err != nil {
		return nil, stateInvalid, err
	}
	for _, p := range cands {
		if _, statErr := os.Stat(p); statErr == nil {
			return cfg, statePresent, nil
		}
	}
	return cfg, stateAbsent, nil
}

// multiFlag accumulates --env-file KEY=VALUE pairs.
type multiFlag struct {
	pairs   map[string]string
	ignored []string
}

func (m *multiFlag) String() string { return fmt.Sprint(m.pairs) }

func (m *multiFlag) Set(v string) error {
	if m.pairs == nil {
		m.pairs = map[string]string{}
	}
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("env-file entry %q is not KEY=VALUE", v)
	}
	if _, dup := m.pairs[k]; dup {
		m.ignored = append(m.ignored, k)
	}
	m.pairs[k] = val
	return nil
}

// ---- config dump ---------------------------------------------------------------

// runConfigDump prints the effective config as TOML plus the file state.
func runConfigDump(ctx context.Context, c *cli, args []string) int {
	fs := newFlagSet("config dump")
	var envFiles multiFlag
	fs.Var(&envFiles, "env-file", "load KEY=VALUE pairs into the override set")
	_ = mustParse(fs, args)
	cfg, state, err := loadWithEnvFiles(c, envFiles)
	if err != nil {
		code, msg := exitCodeForLoad(state, err)
		fmt.Fprintf(os.Stderr, "walhub: %s\n", msg)
		return code
	}
	fmt.Printf("# file_state: %s\n", state)
	if err := toml.NewEncoder(os.Stdout).Encode(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "walhub: encode config: %v\n", err)
		return exitErr
	}
	return exitOK
}

// (dev/null branch: LoadExplicit handles /dev/null as defaults+env.)
