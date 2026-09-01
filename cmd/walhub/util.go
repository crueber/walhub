// util.go — flag/table helpers shared by the thin subcommands.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// newFlagSet builds a per-subcommand FlagSet whose parse errors print usage
// and funnel to exit 2 (§6.3: the dispatcher overrides flag's os.Exit(2)).
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, "usage: walhub %s [flags]\n", name) }
	return fs
}

// mustParse parses argv; on error the process exits 2 (§6.3).
func mustParse(fs *flag.FlagSet, args []string) []string {
	if err := fs.Parse(args); err != nil {
		os.Exit(exitArg)
	}
	return fs.Args()
}

// pad right-pads s with spaces to width w (human tables).
func pad(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

// short12 truncates a hex checksum to the documented 12-char display.
func short12(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

// readFlagSet is mustParse with the positional args beyond flags.
func readFlagSet(name string, args []string) []string {
	return mustParse(newFlagSet(name), args)
}
