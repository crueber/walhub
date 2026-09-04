// Package main is the walhub binary: global --config/--data-dir flags, the
// subcommand dispatcher (a hand-rolled table, 11_config_cli.md §6.2), build
// identity, and the §6.1 exit codes (0 ok, 1 command/config error, 2 argv /
// missing explicit config, 3 config check --strict with ignored overrides).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
)

// buildSHA is linker-injected at build time (-X main.buildSHA=$(SHA)).
var buildSHA = "dev"

// Exit codes (normative, §6.1).
const (
	exitOK     = 0 // success
	exitErr    = 1 // command/config error (bad flag value, validation, strategy not found…)
	exitArg    = 2 // argv errors + a missing explicitly-named config file
	exitStrict = 3 // config check --strict observed ignored overrides
)

// version resolves the build identity (§6.1): linker-injected buildSHA → VCS
// build info (first 12 hex chars) → "dev".
func version() string {
	if buildSHA != "" && buildSHA != "dev" {
		return buildSHA
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 12 {
				return s.Value[:12]
			}
		}
	}
	return "dev"
}

// command is one dispatch-table row; two-token subcommands resolve through the
// flat table ("bundle run", "wal ls", …).
type command struct {
	name string // full dispatch key: "serve", "bundle run", …
	help string
	run  func(ctx context.Context, c *cli, args []string) int
}

// cli carries the peeled globals to every subcommand.
type cli struct {
	configPath string // explicit --config or the env pointer ("" = default location)
	dataDir    string // explicit --data-dir ("" = resolve from env/defaults)
}

var subcommands = []command{
	{"serve", "run the walhub server (the default)", runServe},
	{"compact", "run one maintenance pass / loop the maintainer", runCompact},
	{"bundle run", "run bundle slots due now (--repo ID, --strategy N)", runBundleRun},
	{"bundle plan", "print the slot table for a repo", runBundlePlan},
	{"bundle compose", "build one bundle slot now", runBundleCompose},
	{"bundle rm", "remove bundle entries by id", runBundleRm},
	{"repo create", "register a new repository", runRepoCreate},
	{"repo list", "list repositories", runRepoList},
	{"repo info", "print manifest stats for a repo", runRepoInfo},
	{"repo policy", "read/save/clear the repo policy document", runRepoPolicy},
	{"repo settings", "per-repo settings (D24) surface", runRepoSettings},
	{"access", "read/replace the repo access.json role bindings", runAccess},
	{"wal ls", "log table for a repo", runWalLs},
	{"wal show", "one log entry, full", runWalShow},
	{"wal materialize", "build a standalone repo at a point in time", runWalMaterialize},
	{"wal add-pack", "publish an existing pack as a COMPACT entry", runWalAddPack},
	{"wal annotate-pack", "retrofit side files via manifest-only CAS", runWalAnnotatePack},
	{"wal rev-index", "write a .rev from an .idx", runWalRevIndex},
	{"synth", "deterministic synthetic repo via git fast-import", runSynth},
	{"import", "import a git directory (--from) or a git URL (--url)", runImport},
	{"mirror", "external bridge via a local bare buffer repo", runMirror},
	{"config check", "validate file ⊕ env, report ignored overrides", runConfigCheck},
	{"config dump", "print the effective config as TOML", runConfigDump},
	{"version", "print the build identity", runVersion},
}

// main dispatches: peel the globals, resolve the subcommand (two-token
// subcommands consume a second token), run, map errors onto exit codes.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := &cli{}
	rest := peelGlobals(os.Args[1:], c)
	if len(rest) == 0 {
		os.Exit(runServe(ctx, c, nil))
	}
	// Two-token commands first (bundle/wal/repo/config consume a second token).
	if len(rest) >= 2 {
		if cmd := lookup(rest[0] + " " + rest[1]); cmd != nil {
			os.Exit(cmd.run(ctx, c, rest[2:]))
		}
	}
	if cmd := lookup(rest[0]); cmd != nil {
		os.Exit(cmd.run(ctx, c, rest[1:]))
	}
	fmt.Fprintf(os.Stderr, "walhub: unknown command %q\n\n", rest[0])
	fmt.Fprint(os.Stderr, helpText)
	os.Exit(exitArg)
}

func lookup(name string) *command {
	for i := range subcommands {
		if subcommands[i].name == name {
			return &subcommands[i]
		}
	}
	return nil
}

func runVersion(ctx context.Context, c *cli, args []string) int {
	fmt.Println("walhub " + version())
	return exitOK
}

// usageText mirrors the help table above (kept separate so help stays stable).
var usageText = `Commands:
  serve | compact | bundle | repo | wal | synth | import | mirror | config | version
Run "walhub help" or "walhub <command> -h" for details.
`

// peelGlobals removes --config PATH / --data-dir PATH / --help / -h from
// anywhere in argv (both positions accepted, last wins) into c.
func peelGlobals(argv []string, c *cli) []string {
	var rest []string
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch arg {
		case "--help", "-h":
			fmt.Print(helpText)
			os.Exit(exitOK)
		case "--config", "--data-dir":
			if i+1 >= len(argv) {
				fmt.Fprintf(os.Stderr, "walhub: %s requires a PATH\n", arg)
				os.Exit(exitArg)
			}
			if arg == "--config" {
				c.configPath = argv[i+1]
			} else {
				c.dataDir = argv[i+1]
			}
			i++
		default:
			if v, ok := cutFlag(arg, "--config="); ok {
				c.configPath = v
			} else if v, ok := cutFlag(arg, "--data-dir="); ok {
				c.dataDir = v
			} else {
				rest = append(rest, arg)
			}
		}
	}
	return rest
}

func cutFlag(arg, prefix string) (string, bool) {
	if len(arg) > len(prefix) && arg[:len(prefix)] == prefix {
		return arg[len(prefix):], true
	}
	return "", false
}

const helpText = `walhub — git over HTTP on a WAL engine

Usage: walhub [--config PATH] [--data-dir PATH] <command>

Commands:
  serve                                  run the server (default)
  compact [REPO|--all] [--once] [--base] maintenance compaction pass
  bundle run [--repo ID] [--strategy N]  build due bundle slots
  bundle plan REPO                       print the slot table
  bundle compose REPO [--strategy N]     build one slot now
  bundle rm REPO IDS…                    remove bundle entries
  repo create REPO [--object-format F]   register a repository
  repo list                              list repositories
  repo info REPO                         manifest stats
   repo policy get|set --file F|clear REPO
   repo settings show|set|clear|history REPO
   access get REPO | access put --file F REPO
  wal ls REPO [--from N] [--to N]        log table
  wal show REPO SEQ                      one entry, full
  wal materialize REPO --at-seq N --out DIR
  wal add-pack REPO PACK [--tier N]
  wal annotate-pack REPO CHECKSUM [--rev] [--bitmap] [--commit-graph]
  wal rev-index IDX [--out P]
  synth --out DIR --size s|m|l [--seed N]
  import [--direct] --from GITDIR owner/name
  import --url URL owner/name [--ref REF]… [--default-branch-only]
      [--include-pull-heads] [--include-notes] [--token-env VAR]
      [--format sha1|sha256] [--dangerous]
  mirror --from URL --to URL --dir PATH
  config check [--env-file PATH]… [--strict]
  config dump
  version

Exit codes: 0 success · 1 command/config error · 2 argv error or missing
explicit config file · 3 config check --strict observed ignored overrides.
`
