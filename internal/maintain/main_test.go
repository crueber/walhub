package maintain

import (
	"os"
	"testing"
)

// TestMain pins the git identity for every git subprocess the tests spawn:
// commit-tree and annotated tags require an author/committer, and runners
// (CI, containers) have no global git config. No user config is read or
// written; the env only affects this test binary's children.
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
