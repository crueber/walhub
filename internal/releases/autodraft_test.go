package releases

import (
	"fmt"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

func TestAutodraftWindow(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	// Tags: v1 (older) < v2 (newer); merge shas m1 (in v1), m2 (new in v2).
	x.git.tags["v1"] = strings.Repeat("1", 40)
	x.git.tags["v2"] = strings.Repeat("2", 40)
	m1, m2 := strings.Repeat("a", 40), strings.Repeat("b", 40)
	x.git.ancestors[m1+"\x00"+strings.Repeat("2", 40)] = true
	x.git.ancestors[m1+"\x00"+strings.Repeat("1", 40)] = true
	x.git.ancestors[m2+"\x00"+strings.Repeat("2", 40)] = true
	// m2 NOT an ancestor of v1.

	seedIndex(t, x,
		prCard(3, "Third", "carol"),
		prCard(2, "Second", "bob"),
		prCard(1, "First", "amy"),
		map[string]any{"num": 9, "kind": "issue", "title": "not a pr", "author": "zed"},
	)
	seedPR(t, x, 1, "First", "amy", true, m1, "2026-09-01T10:00:00Z")
	seedPR(t, x, 2, "Second", "bob", true, m2, "2026-09-03T10:00:00Z")
	seedPR(t, x, 3, "Third", "carol", false, "", "")

	// Releases exist: v1 published → since defaults to v1.
	mustPut(t, x, writer(), "v1", ReleaseInput{})

	ad, err := x.svc.Autodraft(ctx(), "o", "r", writer(), "v2", "")
	if err != nil {
		t.Fatal(err)
	}
	if ad.Tag != "v2" || ad.Since != "v1" {
		t.Fatalf("endpoints: %+v", ad)
	}
	if len(ad.PRs) != 1 || ad.PRs[0].Num != 2 {
		t.Fatalf("window: %+v", ad.PRs)
	}
	if want := "- #2 Second (@bob)"; ad.Body != want {
		t.Fatalf("body %q want %q", ad.Body, want)
	}
	// Explicit since=v2 → empty window.
	ad2, err := x.svc.Autodraft(ctx(), "o", "r", writer(), "v2", "v2")
	if err != nil {
		t.Fatal(err)
	}
	if len(ad2.PRs) != 0 || ad2.Body != "" {
		t.Fatalf("empty window: %+v", ad2)
	}
	// Unknown tag / unknown since → 404 unknown revision.
	if _, err := x.svc.Autodraft(ctx(), "o", "r", writer(), "nope", ""); !isErr(err, ErrNotFound) {
		t.Fatalf("unknown tag: %v", err)
	}
	if _, err := x.svc.Autodraft(ctx(), "o", "r", writer(), "v2", "nope"); !isErr(err, ErrNotFound) {
		t.Fatalf("unknown since: %v", err)
	}
}

func TestAutodraftNoReleasesPreviousTag(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("1", 40)
	x.git.tags["v2"] = strings.Repeat("2", 40)
	x.git.tagList = []string{"v2", "v1"} // creordate desc
	m1 := strings.Repeat("a", 40)
	x.git.ancestors[m1+"\x00"+strings.Repeat("2", 40)] = true
	x.git.ancestors[m1+"\x00"+strings.Repeat("1", 40)] = true

	seedIndex(t, x, prCard(1, "First", "amy"))
	seedPR(t, x, 1, "First", "amy", true, m1, "2026-09-01T10:00:00Z")

	// No releases: since defaults to the tag preceding v2 (v1) → m1 is in
	// v1 → empty window.
	ad, err := x.svc.Autodraft(ctx(), "o", "r", writer(), "v2", "")
	if err != nil {
		t.Fatal(err)
	}
	if ad.Since != "v1" || len(ad.PRs) != 0 {
		t.Fatalf("previous-tag since: %+v", ad)
	}
	// Drafting v1 itself (oldest): no older tag → all reachable history.
	ad2, err := x.svc.Autodraft(ctx(), "o", "r", writer(), "v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if ad2.Since != "" || len(ad2.PRs) != 1 {
		t.Fatalf("first-tag window: %+v", ad2)
	}
}

func TestAutodraftEmptyAndGates(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	// No index at all → empty draft, no git ancestry needed.
	ad, err := x.svc.Autodraft(ctx(), "o", "r", writer(), "v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ad.PRs) != 0 || ad.Body != "" || ad.More {
		t.Fatalf("empty: %+v", ad)
	}
	// Anonymous → 401.
	if _, err := x.svc.Autodraft(ctx(), "o", "r", auth.Anonymous(), "v1", ""); !isErr(err, ErrUnauthorized) {
		t.Fatalf("anon: %v", err)
	}
	// Candidates but no git → 503.
	x2 := newHarness(t)
	grantWrite(x2)
	seedIndex(t, x2, prCard(1, "First", "amy"))
	seedPR(t, x2, 1, "First", "amy", true, strings.Repeat("a", 40), "2026-09-01T10:00:00Z")
	x2.svc.Git = nil
	x2.svc.Dirs = nil
	if _, err := x2.svc.Autodraft(ctx(), "o", "r", writer(), "v1", ""); !isErr(err, ErrUnavailable) {
		t.Fatalf("nil git: %v", err)
	}
}

func TestAutodraftCapAndOrdering(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v9"] = strings.Repeat("9", 40)
	// 105 merged PRs, all in the window: output caps at 100, more=true,
	// newest merge first.
	var cards []map[string]any
	for i := 1; i <= 105; i++ {
		cards = append(cards, prCard(i, titleN(i), "amy"))
		merge := mergeSHAN(i)
		at := fmt.Sprintf("2026-09-%02dT10:00:00Z", 1+(i%27))
		seedPR(t, x, i, titleN(i), "amy", true, merge, at)
		x.git.ancestors[merge+"\x00"+strings.Repeat("9", 40)] = true
	}
	seedIndex(t, x, cards...)
	ad, err := x.svc.Autodraft(ctx(), "o", "r", writer(), "v9", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ad.PRs) != MaxAutodraftPRs || !ad.More {
		t.Fatalf("cap: %d more=%v", len(ad.PRs), ad.More)
	}
	if ad.PRs[0].Num != 80 {
		t.Fatalf("order: first=%d", ad.PRs[0].Num)
	}
	if x.git.ancestorN > 2*MaxAutodraftPRs {
		t.Fatalf("git bound: %d ancestry probes", x.git.ancestorN)
	}
}

func titleN(i int) string { return "Change " + itoa(i) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

func mergeSHAN(i int) string {
	base := "0123456789abcdef"
	out := make([]byte, 40)
	for j := range out {
		out[j] = base[(i+j)%len(base)]
	}
	return string(out)
}
