// main_test.go — pins the git identity for every git subprocess the tests
// spawn (field lesson: annotated tags and commit-tree need an
// author/committer; runners and containers have no global git config).
package repoimport

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME":     "walhub-test",
		"GIT_AUTHOR_EMAIL":    "test@walhub.test",
		"GIT_COMMITTER_NAME":  "walhub-test",
		"GIT_COMMITTER_EMAIL": "test@walhub.test",
	} {
		os.Setenv(k, v)
	}
	os.Exit(m.Run())
}
