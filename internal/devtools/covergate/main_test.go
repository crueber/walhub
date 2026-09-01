package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeProfile lays down one coverprofile fixture into a fresh -dir.
func writeProfile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestParseProfile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		wantTot int
		wantHit int
		wantErr bool
	}{
		{
			name: "mode set with hits and misses",
			input: "mode: set\n" +
				"internal/store/store.go:10.20,12.3 4 1\n" +
				"internal/store/store.go:14.2,18.9 6 0\n",
			wantTot: 10, wantHit: 4,
		},
		{
			name: "mode count with zero count misses",
			input: "mode: count\n" +
				"a.go:1.1,2.2 3 0\n" +
				"a.go:4.1,4.9 1 7\n",
			wantTot: 4, wantHit: 1,
		},
		{
			name: "multiple files aggregate separately",
			input: "mode: set\n" +
				"a.go:1.1,2.2 2 1\n" +
				"b.go:1.1,2.2 2 0\n" +
				"a.go:5.1,5.5 2 1\n",
			wantTot: 6, wantHit: 4,
		},
		{name: "blank lines tolerated", input: "mode: set\n\nx.go:1.1,1.2 1 1\n\n", wantTot: 1, wantHit: 1},
		{name: "missing mode header", input: "x.go:1.1,1.2 1 1\n", wantErr: true},
		{name: "garbage line", input: "mode: set\nnot a profile line\n", wantErr: true},
		{name: "bad statement count", input: "mode: set\nx.go:1.1,1.2 none 1\n", wantErr: true},
		{name: "negative count", input: "mode: set\nx.go:1.1,1.2 1 -3\n", wantErr: true},
		{name: "block location without colon", input: "mode: set\nxgo 1 1\n", wantErr: true},
		{name: "empty input", input: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, err := parseProfile(strings.NewReader(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseProfile(%q) = nil error, want error", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseProfile(%q): %v", tc.input, err)
			}
			if p.total != tc.wantTot || p.covered != tc.wantHit {
				t.Fatalf("parseProfile(%q): got total=%d covered=%d, want %d/%d", tc.input, p.total, p.covered, tc.wantTot, tc.wantHit)
			}
			if tc.name == "multiple files aggregate separately" {
				if len(p.files) != 2 || p.files[0].path != "a.go" || p.files[1].path != "b.go" {
					t.Fatalf("files not aggregated per path in stable order: %+v", p.files)
				}
			}
		})
	}
}

func TestRun(t *testing.T) {
	t.Parallel()
	// fixture: store.out at 95% (19/20), wal.out at 50% (2/4) with two weak files.
	passing := "mode: set\n" +
		"internal/store/store.go:1.1,2.2 19 1\n" +
		"internal/store/store.go:3.1,3.9 1 0\n"
	failing := "mode: set\n" +
		"internal/wal/wal.go:1.1,2.2 2 1\n" +
		"internal/wal/replay.go:1.1,2.2 2 0\n"
	tests := []struct {
		name      string
		files     map[string]string
		min       string
		wantCode  int
		wantSubs  []string // substrings required in the report
		wantNoSub []string // substrings that must not appear
	}{
		{
			name:     "happy path: every package at or above the floor",
			files:    map[string]string{"store.out": passing},
			min:      "95",
			wantCode: 0,
			wantSubs: []string{"ok   store: 95.0%"},
		},
		{
			name:     "error path: below the floor names the package, pct, and lowest files",
			files:    map[string]string{"wal.out": failing},
			min:      "95",
			wantCode: 1,
			wantSubs: []string{"FAIL wal: 50.0%", "internal/wal/replay.go", "internal/wal/wal.go", "below 95.0%"},
		},
		{
			name:      "mixed directory lists only the failures",
			files:     map[string]string{"store.out": passing, "wal.out": failing},
			min:       "95",
			wantCode:  1,
			wantSubs:  []string{"FAIL wal: 50.0%"},
			wantNoSub: []string{"FAIL store"},
		},
		{
			name:     "custom floor passes at exactly the boundary",
			files:    map[string]string{"wal.out": failing},
			min:      "50",
			wantCode: 0,
			wantSubs: []string{"ok   wal: 50.0%"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for name, content := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var out, errOut bytes.Buffer
			code := run([]string{"-dir", dir, "-min", tc.min}, &out, &errOut)
			if code != tc.wantCode {
				t.Fatalf("run() = %d, want %d\nreport:\n%s%s", code, tc.wantCode, errOut.String(), out.String())
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(out.String(), sub) {
					t.Errorf("report missing %q:\n%s", sub, out.String())
				}
			}
			for _, sub := range tc.wantNoSub {
				if strings.Contains(out.String(), sub) {
					t.Errorf("report must not contain %q:\n%s", sub, out.String())
				}
			}
		})
	}
}

func TestRunErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		setup    func(t *testing.T) string
		args     []string
		wantCode int
		wantSub  string
	}{
		{
			name:     "missing directory",
			setup:    func(t *testing.T) string { return t.TempDir() + "/absent" },
			wantCode: 1,
			wantSub:  "covergate:",
		},
		{
			name: "no profiles",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			wantCode: 1,
			wantSub:  "no *.out coverprofiles",
		},
		{
			name: "corrupt profile",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "bad.out"), []byte("mode: set\ngarbage\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			wantCode: 1,
			wantSub:  "want 3 fields",
		},
		{
			name: "profile with zero statement blocks",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "empty.out"), []byte("mode: set\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			wantCode: 1,
			wantSub:  "no statement blocks",
		},
		{
			name: "profile vanishes between listing and open (broken symlink)",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.Symlink(dir+"/missing.out", filepath.Join(dir, "gone.out")); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			wantCode: 1,
			wantSub:  "covergate:",
		},
		{
			name:     "stray positional argument",
			setup:    func(t *testing.T) string { return t.TempDir() },
			args:     []string{"extra"},
			wantCode: 2,
			wantSub:  "usage:",
		},
		{
			name:     "flag missing its value",
			setup:    func(t *testing.T) string { return t.TempDir() },
			args:     []string{"-min"},
			wantCode: 2,
			wantSub:  "usage:",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"-dir", tc.setup(t)}, tc.args...)
			var out, errOut bytes.Buffer
			if code := run(args, &out, &errOut); code != tc.wantCode {
				t.Fatalf("run(%v) = %d, want %d\nstdout: %s\nstderr: %s", args, code, tc.wantCode, out.String(), errOut.String())
			}
			if !strings.Contains(errOut.String(), tc.wantSub) {
				t.Errorf("stderr missing %q: %s", tc.wantSub, errOut.String())
			}
		})
	}
}

func TestRunFailureListsAtMostFiveFiles(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("mode: set\n")
	for i := range 6 {
		// One fully covered file (i == 0), five empty ones: 16.7% overall, five named files.
		fmt.Fprintf(&b, "f%d.go:1.1,2.2 5 %d\n", i, i)
	}
	dir := writeProfile(t, "six.out", b.String())
	var out, errOut bytes.Buffer
	if code := run([]string{"-dir", dir, "-min", "95"}, &out, &errOut); code != 1 {
		t.Fatalf("run() = %d, want 1\n%s", code, out.String())
	}
	if got := strings.Count(out.String(), ".go ("); got != 5 {
		t.Errorf("failure report names %d files, want 5:\n%s", got, out.String())
	}
}

// TestMainProcess is the child half of the main() smoke test: re-invoked via
// COVERGATE_MAIN=1 with "--" separating test flags from covergate argv, it is
// the real main running against a fixture profile.
func TestMainProcess(t *testing.T) {
	if os.Getenv("COVERGATE_MAIN") != "1" {
		t.Skip("subprocess half of TestMainSmoke")
	}
	for i, a := range os.Args {
		if a == "--" {
			os.Args = append([]string{"covergate"}, os.Args[i+1:]...)
			break
		}
	}
	main()
}

func TestMainSmoke(t *testing.T) {
	t.Parallel()
	dir := writeProfile(t, "store.out", "mode: set\ninternal/store/store.go:1.1,2.2 19 1\ninternal/store/store.go:3.1,3.9 1 0\n")
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainProcess$", "--", "-dir", dir, "-min", "95")
	cmd.Env = append(os.Environ(), "COVERGATE_MAIN=1")
	out, err := cmd.Output()
	exitErr, _ := err.(*exec.ExitError)
	if err != nil && exitErr == nil {
		t.Fatalf("re-exec failed: %v", err)
	}
	code := 0
	if exitErr != nil {
		code = exitErr.ExitCode()
	}
	if code != 0 {
		t.Fatalf("main subprocess exit = %d, want 0\noutput: %s", code, out)
	}
	if !strings.Contains(string(out), "ok   store: 95.0%") {
		t.Errorf("main output missing package line:\n%s", out)
	}
}
