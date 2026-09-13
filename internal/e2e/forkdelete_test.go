// forkdelete_test.go — Forgejo #451 end-to-end: deleting a fork parent
// with live children preserves the pack set as a meta repository and
// the children keep working (reads + push + clone) with real git on a
// real server subprocess. Skipped in -short mode (the e2e tier).
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestE2E_ForkDeleteKeepsChildrenWorking:
// seed parent → push main → fork (pull-fork task) → DELETE the parent →
// the child's clone/push/read paths keep working, the parent is gone
// (404 summary, no refs), and the parent name refuses re-create.
func TestE2E_ForkDeleteKeepsChildrenWorking(t *testing.T) {
	requireModernGit(t)
	if testing.Short() {
		t.Skip("e2e tier: skipped in -short mode")
	}
	g := newGitClient(t)
	s := chainServer(t)
	owner := testOwner
	repo := uniqueRepo("forkdelparent")
	forkName := repo + "-fork"

	// ---- 1. seed the parent -------------------------------------------------
	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	seedSHA := g.commitFile(work, "hello.txt", "hello parent\n", "seed commit")
	parentURL := s.gitURL(owner, repo)
	g.run(work, "remote", "add", "origin", parentURL)
	g.runAuth(work, chainAliceTok, "push", "-u", "origin", "main")
	if refs := g.lsRemoteAuth(parentURL, chainAliceTok); refs["refs/heads/main"] != seedSHA {
		t.Fatalf("parent main = %q, want %q", refs["refs/heads/main"], seedSHA)
	}

	// ---- 2. fork it ----------------------------------------------------------
	forkRes := mustAPI(t, "POST", s.base+"/api/v1/repos/"+owner+"/"+repo+"/forks",
		fmt.Sprintf(`{"target_owner":%q,"name":%q,"visibility":"public","branch":"refs/heads/main"}`,
			owner, forkName), chainAliceTok)
	if got := jget(t, forkRes, "repo"); got != owner+"/"+forkName {
		t.Fatalf("fork repo = %v, want %s/%s", got, owner, forkName)
	}
	forkURL := s.gitURL(owner, forkName)
	forkLane := s.base + "/" + owner + "/" + forkName + "/api"
	pollUntil(t, 60*time.Second, "fork child to become servable", func() bool {
		st, data := apiCall(t, "GET", forkLane, "", chainAliceTok)
		if st != 200 {
			return false
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			return false
		}
		return doc["fork_parent"] == owner+"/"+repo
	})
	if refs := g.lsRemoteAuth(forkURL, chainAliceTok); refs["refs/heads/main"] != seedSHA {
		t.Fatalf("child main = %q, want seed %q", refs["refs/heads/main"], seedSHA)
	}

	// ---- 3. DELETE the parent (admin) ----------------------------------------
	parentLane := s.base + "/" + owner + "/" + repo + "/api"
	if st, data := apiCall(t, "DELETE", parentLane, "", chainAliceTok); st != 204 {
		t.Fatalf("DELETE parent: status %d, want 204 (body %s)", st, data)
	}

	// ---- 4. parent is gone: 404 summary, no refs, name held ------------------
	if st, _ := apiCall(t, "GET", parentLane, "", chainAliceTok); st != 404 {
		t.Fatalf("GET deleted parent summary: status %d, want 404", st)
	}
	if refs := g.lsRemoteAuthOK(parentURL, chainAliceTok); refs != nil {
		t.Fatalf("deleted parent still serves refs: %v", refs)
	}

	// ---- 5. child reads keep working (clone the full history) ----------------
	cloneDir := filepath.Join(t.TempDir(), "child-clone")
	if out, err := runGitSoft(g, []string{"-c", "http.extraHeader=Authorization: Bearer " + chainAliceTok, "clone", forkURL, cloneDir}); err != nil {
		t.Fatalf("clone of child after parent delete: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(cloneDir, "hello.txt"))
	if err != nil || string(got) != "hello parent\n" {
		t.Fatalf("cloned child content = %q, err %v", got, err)
	}

	// ---- 6. child push keeps working ------------------------------------------
	childSHA := g.commitFile(cloneDir, "child.txt", "child change\n", "child commit")
	g.runAuth(cloneDir, chainAliceTok, "push", "origin", "main")
	pollUntil(t, 60*time.Second, "child push to land", func() bool {
		refs := g.lsRemoteAuthOK(forkURL, chainAliceTok)
		return refs != nil && refs["refs/heads/main"] == childSHA
	})

	// ---- 7. child summary still projects the (meta) parent --------------------
	st, data := apiCall(t, "GET", forkLane, "", chainAliceTok)
	if st != 200 {
		t.Fatalf("GET child summary after parent delete: status %d", st)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("child summary decode: %v", err)
	}
	if doc["fork_parent"] != owner+"/"+repo {
		t.Fatalf("child fork_parent = %v, want %s/%s", doc["fork_parent"], owner, repo)
	}

	// ---- 8. the parent name absorbs on re-create --------------------------------
	// The defined behavior (planner's call, #451): re-create lands a
	// fresh manifest on the absent key — 201 on the API path, and the
	// git auto-create path takes the same shape. The absorbed parent
	// serves its own pushes while the child keeps reading the
	// preserved packs through the same prefix.
	if st, _ := apiCall(t, "PUT", parentLane, "", chainAliceTok); st != 201 {
		t.Fatalf("PUT re-create of meta name: status %d, want 201 (absorb)", st)
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	g.initRepo(fresh)
	freshSHA := g.commitFile(fresh, "other.txt", "other\n", "other commit")
	g.run(fresh, "remote", "add", "origin", parentURL)
	g.runAuth(fresh, chainAliceTok, "push", "-u", "origin", "main")
	if refs := g.lsRemoteAuth(parentURL, chainAliceTok); refs["refs/heads/main"] != freshSHA {
		t.Fatalf("absorbed parent main = %q, want %q", refs["refs/heads/main"], freshSHA)
	}
	// The child is unaffected by the absorption.
	if refs := g.lsRemoteAuth(forkURL, chainAliceTok); refs["refs/heads/main"] != childSHA {
		t.Fatalf("child main after absorb = %q, want %q", refs["refs/heads/main"], childSHA)
	}
}
