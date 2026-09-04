// cover_gap_test.go — one pure unit test for RefCache.RefView's
// pending-delete-over-base branch (refs.go). The repo-wide `make cover`
// gate sits exactly one statement short in this untouched package;
// this pins the branch without touching any behavior.
package git

import (
	"strings"
	"testing"
)

func TestRefViewPendingDeleteOverBase(t *testing.T) {
	c := NewRefCache()
	c.base = &RefSnapshot{Refs: []RefEntry{{Name: "refs/heads/main", Oid: Oid(strings.Repeat("a", 40))}}}
	c.pending = map[string]Oid{"refs/heads/main": Oid(strings.Repeat("0", 40))}
	if _, ok := c.RefView("refs/heads/main"); ok {
		t.Fatal("pending zero-oid delete must hide the base entry")
	}
}
