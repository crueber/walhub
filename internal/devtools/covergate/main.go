// Command covergate enforces the D7 coverage floor (15_testing.md §7.1,
// 16_packaging.md D7): ≥ -min percent statement coverage, judged per package.
//
// The Makefile `cover` target writes one `go test -coverprofile` file per
// internal/... package into .cover/, then invokes:
//
//	go run ./internal/devtools/covergate -dir .cover -min 95
//
// Each *.out profile in -dir is one package's raw `go tool cover` profile
// ("mode: set", then "file.go:startLine.startCol,endLine.endCol numStmts count"
// lines). covergate computes Σ numStmts(hit > 0) / Σ numStmts per profile,
// exits 1 below the floor naming the package, its percentage, and its five
// lowest-covered files. Nothing is excluded — the Makefile already scopes the
// profiles to internal/....
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const usage = "usage: covergate -dir .cover -min 95"

// fileStat aggregates one source file's blocks within a profile.
type fileStat struct {
	path string
	// stmts is Σ numStmts across the file's blocks; hit is the covered subset.
	stmts int
	hit   int
}

// profile is one package's parsed coverprofile.
type profile struct {
	files []fileStat
	// total and covered are Σ numStmts and Σ numStmts(count > 0).
	total   int
	covered int
}

// pct returns the covered-statement percentage (0 statements ⇒ error).
func (p *profile) pct() (float64, error) {
	if p.total == 0 {
		return 0, errors.New("profile contains no statement blocks")
	}
	return 100 * float64(p.covered) / float64(p.total), nil
}

// lowest sorts the files ascending by covered percentage and returns up to n
// entries — the actionable tail the failure report names.
func (p *profile) lowest(n int) []fileStat {
	out := make([]fileStat, len(p.files))
	copy(out, p.files)
	sort.Slice(out, func(i, j int) bool {
		pi := 100 * float64(out[i].hit) / float64(out[i].stmts)
		pj := 100 * float64(out[j].hit) / float64(out[j].stmts)
		if pi != pj {
			return pi < pj
		}
		return out[i].path < out[j].path
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// parseProfile reads a `go tool cover` profile: a "mode: …" header, then one
// "name.go:l.c,l.c numStmts count" block per line. Whitespace-tolerant; empty
// lines are skipped.
func parseProfile(r io.Reader) (*profile, error) {
	p := &profile{}
	byPath := map[string]*fileStat{}
	sc := bufio.NewScanner(r)
	lineNo := 0
	sawMode := false
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !sawMode {
			if !strings.HasPrefix(line, "mode:") {
				return nil, fmt.Errorf("line %d: expected \"mode:\" header, got %q", lineNo, line)
			}
			sawMode = true
			continue
		}
		// <file>:<sl>.<sc>,<el>.<ec> <numStmts> <count>
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("line %d: want 3 fields, got %d: %q", lineNo, len(fields), line)
		}
		name, _, ok := strings.Cut(fields[0], ":")
		if !ok || name == "" {
			return nil, fmt.Errorf("line %d: bad block location %q", lineNo, fields[0])
		}
		stmts, err := strconv.Atoi(fields[1])
		if err != nil || stmts < 0 {
			return nil, fmt.Errorf("line %d: bad statement count %q", lineNo, fields[1])
		}
		count, err := strconv.Atoi(fields[2])
		if err != nil || count < 0 {
			return nil, fmt.Errorf("line %d: bad execution count %q", lineNo, fields[2])
		}
		fs := byPath[name]
		if fs == nil {
			fs = &fileStat{path: name}
			byPath[name] = fs
		}
		fs.stmts += stmts
		if count > 0 {
			fs.hit += stmts
		}
		p.total += stmts
		if count > 0 {
			p.covered += stmts
		}
	}
	// Materialize the per-file view in a stable path order.
	p.files = make([]fileStat, 0, len(byPath))
	for _, f := range byPath {
		p.files = append(p.files, *f)
	}
	sort.Slice(p.files, func(i, j int) bool { return p.files[i].path < p.files[j].path })
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading profile: %w", err)
	}
	if !sawMode {
		return nil, errors.New("missing \"mode:\" header")
	}
	return p, nil
}

// run is the testable body of main; it returns the process exit code.
func run(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("covergate", flag.ContinueOnError)
	dir := fs.String("dir", ".cover", "directory of per-package `-coverprofile` files")
	minPct := fs.Float64("min", 95.0, "minimum statement coverage percent per package")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(errOut, usage)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errOut, usage)
		return 2
	}

	entries, err := os.ReadDir(*dir)
	if err != nil {
		fmt.Fprintf(errOut, "covergate: %v\n", err)
		return 1
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".out") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Fprintf(errOut, "covergate: no *.out coverprofiles in %s\n", *dir)
		return 1
	}

	fail := 0
	for _, name := range names {
		label := strings.TrimSuffix(name, ".out")
		f, err := os.Open(filepath.Join(*dir, name))
		if err != nil {
			fmt.Fprintf(errOut, "covergate: %v\n", err)
			return 1
		}
		p, perr := parseProfile(f)
		f.Close()
		if perr != nil {
			fmt.Fprintf(errOut, "covergate: %s: %v\n", name, perr)
			return 1
		}
		pct, perr := p.pct()
		if perr != nil {
			fmt.Fprintf(errOut, "covergate: %s: %v\n", name, perr)
			return 1
		}
		if pct < *minPct {
			fail++
			fmt.Fprintf(out, "FAIL %s: %.1f%% (floor %.1f%%)\n", label, pct, *minPct)
			for _, file := range p.lowest(5) {
				fp := 100 * float64(file.hit) / float64(file.stmts)
				fmt.Fprintf(out, "     %5.1f%%  %s (%d/%d stmts)\n", fp, file.path, file.hit, file.stmts)
			}
			continue
		}
		fmt.Fprintf(out, "ok   %s: %.1f%%\n", label, pct)
	}
	if fail > 0 {
		fmt.Fprintf(out, "covergate: %d package(s) below %.1f%%\n", fail, *minPct)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
