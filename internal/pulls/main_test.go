package pulls

import (
	"os"
	"testing"
)

// TestMain pins the git identity for spawned subprocesses (same reason as
// internal/git: commit-tree needs an author/committer; runners have no
// global git config). Env affects this test binary's children only.
func TestMain(m *testing.M) {
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME":     "t",
		"GIT_AUTHOR_EMAIL":    "t@t",
		"GIT_COMMITTER_NAME":  "t",
		"GIT_COMMITTER_EMAIL": "t@t",
	} {
		os.Setenv(k, v)
	}
	os.Exit(m.Run())
}
