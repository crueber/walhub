// fakes_test.go — test doubles + builders (same-package, like pulls'
// fakes_test.go): scripted roles, fixture git remotes, service wiring.
package repoimport

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// --- fake roles ------------------------------------------------------------------

// FakeRoles scripts the RoleService: host flags pass through the real
// principal; named users resolve per Roles ("" = read); Public toggles
// anonymous reads. Access writes go to a guarded map (no store).
type FakeRoles struct {
	mu       sync.Mutex
	Roles    map[string]string
	Public   bool
	Bindings map[string][]identity.AccessBinding // "owner/repo" → bindings
	Vis      map[string]identity.Visibility
	Boot     int // BootstrapRepo calls
}

func (f *FakeRoles) roleOf(name string) identity.Role {
	if r := f.Roles[strings.ToLower(name)]; r != "" {
		return identity.Role(r)
	}
	return identity.RoleRead
}

func (f *FakeRoles) Resolve(_ context.Context, _, _ string, p auth.Principal) (identity.Role, *identity.AccessDoc) {
	if p.Admin {
		return identity.RoleAdmin, nil
	}
	if p.Write {
		return identity.RoleWrite, nil
	}
	if p.Anonymous {
		if f.Public {
			return identity.RoleRead, nil
		}
		return "", nil
	}
	return f.roleOf(p.Name), nil
}

func (f *FakeRoles) CheckRead(_ context.Context, _, _ string, p auth.Principal) *auth.AuthError {
	if p.Admin || p.Write {
		return nil
	}
	if p.Anonymous {
		if f.Public {
			return nil
		}
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	return nil
}

func (f *FakeRoles) CheckRole(_ context.Context, _, _ string, p auth.Principal, want identity.Role) *auth.AuthError {
	if p.Admin {
		return nil
	}
	if p.Anonymous {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	if p.Write {
		return nil
	}
	got := f.roleOf(p.Name)
	if rankOf(got) >= rankOf(want) {
		return nil
	}
	return &auth.AuthError{Kind: auth.ErrForbidden, Why: "insufficient role: need " + string(want)}
}

func rankOf(r identity.Role) int {
	switch r {
	case identity.RoleRead:
		return 1
	case identity.RoleTriage:
		return 2
	case identity.RoleWrite:
		return 3
	case identity.RoleMaintain:
		return 4
	case identity.RoleAdmin:
		return 5
	}
	return 0
}

func (f *FakeRoles) BootstrapRepo(_ context.Context, _, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Boot++
	return false, nil
}

func (f *FakeRoles) GetAccess(_ context.Context, owner, repo string) (*identity.AccessDoc, store.Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := owner + "/" + repo
	if b, ok := f.Bindings[key]; ok {
		vis := identity.VisibilityPublic
		if v, ok := f.Vis[key]; ok {
			vis = v
		}
		return &identity.AccessDoc{Version: 1, Visibility: vis, RoleBindings: append([]identity.AccessBinding{}, b...)}, "v1", nil
	}
	return identity.SynthesizeDefault(owner), "", nil
}

func (f *FakeRoles) PutAccess(_ context.Context, owner, repo string, _ store.Version, vis identity.Visibility, bindings []identity.AccessBinding) (*identity.AccessDoc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Bindings == nil {
		f.Bindings = map[string][]identity.AccessBinding{}
	}
	if f.Vis == nil {
		f.Vis = map[string]identity.Visibility{}
	}
	key := owner + "/" + repo
	f.Bindings[key] = append([]identity.AccessBinding{}, bindings...)
	f.Vis[key] = vis
	return &identity.AccessDoc{Version: 2, Visibility: vis, RoleBindings: bindings}, nil
}

// --- wiring ------------------------------------------------------------------------

// testConfig returns Defaults with a temp cache dir + file:// allowed.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	cfg.Import.AllowFileURLs = true
	return cfg
}

// testService builds a Service over the memory store + real registry.
// roles nil → host-flag-only gating; ident exercised via realRoles.
func testService(t *testing.T, cfg *config.Config, roles RoleService) (*Service, store.ObjectStore) {
	t.Helper()
	if cfg == nil {
		cfg = testConfig(t)
	}
	return testServiceOnStore(t, cfg, roles, store.NewMemory())
}

// testServiceOnStore builds a Service over a caller-supplied store
// (CAS-failure injection for arbitration paths).
func testServiceOnStore(t *testing.T, cfg *config.Config, roles RoleService, st store.ObjectStore) (*Service, store.ObjectStore) {
	t.Helper()
	if cfg == nil {
		cfg = testConfig(t)
	}
	ctx := context.Background()
	reg := wal.NewRegistry(ctx, st, cfg)
	t.Cleanup(reg.Close)
	return New(Deps{Store: st, Reg: reg, Roles: roles, Cfg: cfg, Hostname: "testhost"}), st
}

// realRoles wires the REAL identity service (proves the S7 path).
func realRoles(st store.ObjectStore, cfg *config.Config) *identity.Service {
	return identity.New(st, cfg)
}

func adminPrincipal() auth.Principal { return auth.Principal{Name: "root", Write: true, Admin: true} }

func writerPrincipal() auth.Principal { return auth.Principal{Name: "writer", Write: true} }

// --- fixture remotes ---------------------------------------------------------------

// fixtureRepo builds a real git repo at dir with n commits on main,
// branches b1..bN (one commit each), and annotated tags v1..vT.
// Returns the file:// URL. Layout is deterministic (fixed messages).
func fixtureRepo(t *testing.T, dir string, n, branches, tags int) string {
	t.Helper()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run("init", "-b", "main", ".")
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("f%d.txt", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("content %d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", name)
		run("commit", "-q", "-m", fmt.Sprintf("commit %d", i))
	}
	for b := 0; b < branches; b++ {
		name := fmt.Sprintf("b%d", b)
		run("checkout", "-q", "-b", name)
		if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte("branch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", name+".txt")
		run("commit", "-q", "-m", "branch "+name)
		run("checkout", "-q", "main")
	}
	for v := 0; v < tags; v++ {
		run("tag", "-a", fmt.Sprintf("v%d", v), "-m", fmt.Sprintf("release %d", v))
	}
	return "file://" + dir
}

// repackSingle packs the fixture into ONE pack (the S10 pinned layout:
// single-pack => Npacks=1, so the ops RANGE is tight).
func repackSingle(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "repack", "-a", "-d")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("repack: %v\n%s", err, out)
	}
	packs, err := filepath.Glob(filepath.Join(dir, ".git", "objects", "pack", "*.pack"))
	if err != nil || len(packs) != 1 {
		// Bare-layout fallback (a bare fixture keeps packs at top level).
		packs, err = filepath.Glob(filepath.Join(dir, "objects", "pack", "*.pack"))
	}
	if err != nil || len(packs) != 1 {
		t.Fatalf("pinned single-pack layout violated: %v %v", packs, err)
	}
}

// writeFile writes one file under dir.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gitAddCommit stages + commits one path in dir.
func gitAddCommit(t *testing.T, dir, path, msg string) {
	t.Helper()
	for _, args := range [][]string{{"add", path}, {"commit", "-q", "-m", msg}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// gitOutput runs git in dir and returns trimmed stdout (dir may be a
// file:// URL — the host path is used; plain dirs pass through).
func gitOutput(t *testing.T, dirURL string, args ...string) string {
	t.Helper()
	dir := strings.TrimPrefix(dirURL, "file://")
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
