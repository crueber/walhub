package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// minGitMajor/minGitMinor gate the suite on a git client new enough for
// `git init -b` and the smart-HTTP behaviors the scenarios rely on.
const (
	minGitMajor = 2
	minGitMinor = 47
)

var (
	gitOnce   sync.Once
	gitModern bool
	gitVerStr string
)

// requireModernGit skips the scenario when the installed git is too old.
func requireModernGit(t *testing.T) {
	t.Helper()
	gitOnce.Do(func() {
		out, err := exec.Command("git", "--version").CombinedOutput()
		if err != nil {
			gitVerStr = fmt.Sprintf("git --version failed: %v", err)
			return
		}
		var major, minor int
		line := strings.TrimSpace(string(out))
		if _, err := fmt.Sscanf(line, "git version %d.%d", &major, &minor); err != nil {
			gitVerStr = "unparseable: " + line
			return
		}
		gitModern = major > minGitMajor ||
			(major == minGitMajor && minor >= minGitMinor)
		gitVerStr = line
	})
	if !gitModern {
		t.Skipf("git >= %d.%d required, have %s", minGitMajor, minGitMinor, gitVerStr)
	}
}

// freePort returns a port that is free at the moment of the call. It is
// released before the server binds it — the usual test-only race, made
// unlikely because nothing else in the suite listens meanwhile.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// e2eServer is one walhub subprocess. The server listens on 0.0.0.0 (or the
// config's listen address); clients always talk to it over 127.0.0.1.
type e2eServer struct {
	t       *testing.T
	cmd     *exec.Cmd
	port    int
	dataDir string
	base    string
	logs    *bytes.Buffer
	stopped bool
}

// startServer boots the built binary with WALHUB_DATA_DIR and PORT set, and
// waits until /healthz answers 200. Killing the process on cleanup is the
// caller's job via stop() (registered as t.Cleanup by the scenario helpers).
func startServer(t *testing.T, bin, dataDir string, port int) *e2eServer {
	t.Helper()
	logs := &bytes.Buffer{}
	cmd := exec.Command(bin)
	cmd.Env = []string{
		"WALHUB_DATA_DIR=" + dataDir,
		"PORT=" + strconv.Itoa(port),
		"HOME=" + dataDir,
		"PATH=" + os.Getenv("PATH"),
	}
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start walhub: %v\nlogs:\n%s", err, logs.String())
	}
	s := &e2eServer{
		t:       t,
		cmd:     cmd,
		port:    port,
		dataDir: dataDir,
		base:    fmt.Sprintf("http://127.0.0.1:%d", port),
		logs:    logs,
	}
	s.waitHealthy(20 * time.Second)
	return s
}

// waitReady polls /healthz until it answers 200, with a deadline.
func (s *e2eServer) waitHealthy(deadline time.Duration) {
	s.t.Helper()
	deadlineAt := time.Now().Add(deadline)
	for time.Now().Before(deadlineAt) {
		if s.stopped {
			s.t.Fatal("server stopped before becoming ready")
		}
		if status, _ := httpGet(s.base+"/healthz", 2*time.Second); status == http.StatusOK {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.t.Fatalf("server not ready within %v\nlogs:\n%s", deadline, s.logs.String())
}

// stop terminates the subprocess (SIGTERM, escalating to SIGKILL).
func (s *e2eServer) stop() {
	if s.stopped || s.cmd.Process == nil {
		return
	}
	s.stopped = true
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
}

// gitURL is the smart-HTTP clone URL of owner/name on this server.
func (s *e2eServer) gitURL(owner, repo string) string {
	return fmt.Sprintf("%s/%s/%s.git", s.base, owner, repo)
}

// ---- HTTP helpers -------------------------------------------------------------

var httpClient = &http.Client{Timeout: 10 * time.Second}

func httpGet(url string, timeout time.Duration) (int, []byte) {
	c := httpClient
	if timeout > 0 {
		c = &http.Client{Timeout: timeout}
	}
	resp, err := c.Get(url)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body
}

// getJSON fetches url and decodes the JSON body into out; fails on any
// non-2xx.
func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := httpClient.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		t.Fatalf("GET %s: status %d, body %s", url, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("GET %s: decode JSON: %v\nbody: %s", url, err, body)
	}
}

// httpPost issues a JSON POST and returns the status and body.
func httpPost(t *testing.T, url string, body []byte) (int, []byte) {
	t.Helper()
	resp, err := httpClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, data
}

// httpMethod issues an arbitrary request with a JSON body and returns the
// status and body.
func httpRequest(t *testing.T, method, url string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, data
}

// ---- git client helpers --------------------------------------------------------

// gitClient runs the real git binary with an isolated config: no prompts, no
// system/global config from the user's machine, deterministic identity and
// default branch (docs/go/15_testing.md §5.2: never touch the user's git
// config).
type gitClient struct {
	t       *testing.T
	sandbox string // per-scenario dir holding the isolated gitconfig
}

func newGitClient(t *testing.T) *gitClient {
	t.Helper()
	sandbox := t.TempDir()
	cfg := "[init]\n\tdefaultBranch = main\n[user]\n\tname = E2E Tester\n\temail = e2e@example.com\n"
	if err := os.WriteFile(filepath.Join(sandbox, "gitconfig"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}
	return &gitClient{t: t, sandbox: sandbox}
}

// run executes git with the isolated environment and fails the test on error.
func (g *gitClient) run(dir string, args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL="+filepath.Join(g.sandbox, "gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+g.sandbox,
		"XDG_CONFIG_HOME="+filepath.Join(g.sandbox, ".config"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %v (in %s): %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// initRepo creates a fresh repo with a main branch at dir.
func (g *gitClient) initRepo(dir string) {
	g.t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		g.t.Fatalf("mkdir %s: %v", dir, err)
	}
	g.run(dir, "init", "-b", "main")
}

// commitFile writes path=content, commits it, and returns the new HEAD sha.
func (g *gitClient) commitFile(dir, path, content, msg string) string {
	g.t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		g.t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		g.t.Fatalf("write %s: %v", full, err)
	}
	g.run(dir, "add", path)
	g.run(dir, "commit", "-m", msg)
	return strings.TrimSpace(g.run(dir, "rev-parse", "HEAD"))
}

// lsRemote runs git ls-remote (with extra args such as --tags or a URL) and
// returns a ref->sha map.
func (g *gitClient) lsRemote(url string, extra ...string) map[string]string {
	g.t.Helper()
	args := append([]string{"ls-remote"}, extra...)
	args = append(args, url)
	out := g.run(g.sandbox, args...)
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		refs[parts[1]] = parts[0]
	}
	return refs
}

var nameCounter atomic.Uint64

// uniqueRepo returns a repo name unique within this process, so scenarios
// never collide even if they share a server.
func uniqueRepo(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), nameCounter.Add(1))
}
