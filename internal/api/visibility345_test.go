package api

// visibility345_test.go — Forgejo #345: visibility as the read authority
// for repo surfaces, with listings filtered by the caller's read role.
//
// Matrix (surface × visibility × principal), all with the Access gate
// wired and anonymous_read=false unless noted:
//   - repo summary (dispatch + open): anon+public → 200, anon+private →
//     401, authed-member+private → 200, stranger+private → 403,
//     stranger+public → 200, admin+private → 200.
//   - listings (owners, owner repos, both detaileds): private names,
//     counts, and aggregates never surface for callers who cannot read
//     them; admins see everything (+0 filter trips).
//   - summary/listing projections carry visibility with ETag coverage.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/sizecatalog"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// scriptedGate is a per-repo ReadAccess: readable "owner/repo" keys pass,
// host admin callers pass everywhere (the P6 step-3 early-allow the
// real hook applies — Forgejo #374: host WRITE alone grants nothing on
// private repos), everything else denies (401 anonymous, 403
// authenticated).
type scriptedGate struct {
	readable map[string]bool
	calls    int
}

func (s *scriptedGate) CheckRead(_ context.Context, owner, repo string, p auth.Principal) *auth.AuthError {
	s.calls++
	if p.Admin {
		return nil
	}
	if s.readable[owner+"/"+repo] {
		return nil
	}
	if p.Anonymous {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	return &auth.AuthError{Kind: auth.ErrForbidden, Why: "read access required"}
}

var (
	anon345   = auth.Anonymous()
	reader345 = auth.Principal{Name: "reader@example.com"}
	member345 = auth.Principal{Name: "member@example.com"}
	admin345  = auth.Principal{Name: "root@example.com", Write: true, Admin: true}
	writer345 = auth.Principal{Name: "ops@example.com", Write: true}
)

func visFixture(t *testing.T) (*fixture, *scriptedGate) {
	t.Helper()
	f := newFixture(t)
	f.env.Cfg.Server.Auth.AnonymousRead = false
	seedSummary(f)
	f.view.summaries["demo/priv"] = SummaryData{Head: new(Ref), Branches: 1}
	*f.view.summaries["demo/priv"].Head = Ref{Name: "refs/heads/main", SHA: fakeSHA2}
	g := &scriptedGate{readable: map[string]bool{"demo/walgit": true}}
	f.env.Access = g
	f.env.RepoVisibility = func(_ context.Context, owner, repo string) (string, bool) {
		if owner == "demo" && repo == "priv" {
			return "private", true
		}
		return "public", true
	}
	return f, g
}

func TestDispatchVisibilityMatrix(t *testing.T) {
	newSeeded := func(t *testing.T, extra map[string]bool) (*fixture, *scriptedGate) {
		f, g := visFixture(t)
		for k, v := range extra {
			g.readable[k] = v
		}
		return f, g
	}
	cases := []struct {
		name  string
		repo  string
		extra map[string]bool
		p     *auth.Principal
		code  int
	}{
		{"anon public", "walgit", nil, nil, http.StatusOK},
		{"anon private", "priv", nil, nil, http.StatusUnauthorized},
		{"reader public", "walgit", nil, &reader345, http.StatusOK},
		{"stranger private", "priv", nil, &reader345, http.StatusForbidden},
		{"member private", "priv", map[string]bool{"demo/priv": true}, &member345, http.StatusOK},
		{"admin private", "priv", nil, &admin345, http.StatusOK},
		// Forgejo #374: host-write-only outsiders read public and
		// authenticated repos via visibility, never private ones.
		{"host-write private", "priv", nil, &writer345, http.StatusForbidden},
	}
	for _, tc := range cases {
		f, _ := newSeeded(t, tc.extra)
		w := f.do("GET", "/demo/"+tc.repo+"/api", nil, nil, tc.p)
		if w.Code != tc.code {
			t.Errorf("%s: = %d, want %d (%s)", tc.name, w.Code, tc.code, w.Body.String())
		}
		if tc.code == http.StatusUnauthorized && w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: 401 must carry Bearer", tc.name)
		}
	}
	// The flag still gates non-repo surfaces: anonymous listings stay 401
	// when anonymous_read=false (its kept non-repo meaning).
	f, _ := newSeeded(t, nil)
	if w := f.do("GET", "/api/v1/owners", nil, nil, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("anon owners with flag off = %d, want 401", w.Code)
	}
	// ...and the same public repo is anonymous-readable with the flag on.
	f.env.Cfg.Server.Auth.AnonymousRead = true
	if w := f.do("GET", "/demo/walgit/api", nil, nil, nil); w.Code != http.StatusOK {
		t.Errorf("anon public with flag on = %d, want 200", w.Code)
	}
	// Write routes never consult the read gate.
	fw, _ := newSeeded(t, nil)
	if w := fw.do("PUT", "/demo/priv/api", strings.NewReader("{}"), nil, &writer345); w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), "read access required") {
		t.Error("write routes must not consult the read gate")
	}
}

func TestSummaryVisibilityFieldAndETag(t *testing.T) {
	f, _ := visFixture(t)
	var body struct {
		Visibility string `json:"visibility"`
	}
	w := f.do("GET", "/demo/walgit/api", nil, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("summary = %d", w.Code)
	}
	decodeJSON(t, w, &body)
	if body.Visibility != "public" {
		t.Fatalf("visibility = %q, want public", body.Visibility)
	}
	etagPub := w.Header().Get("ETag")
	if !strings.Contains(etagPub, "~vpublic") {
		t.Fatalf("public etag = %q, want ~vpublic suffix", etagPub)
	}
	// A visibility flip moves no ref: the ETag must cover the field or a
	// revalidating client 304s and keeps the stale badge.
	f.env.RepoVisibility = func(_ context.Context, owner, repo string) (string, bool) {
		return "private", true
	}
	w2 := f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": etagPub}, nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("flipped visibility with stale etag = %d, want 200 (no stale 304)", w2.Code)
	}
	decodeJSON(t, w2, &body)
	if body.Visibility != "private" {
		t.Fatalf("flipped visibility = %q, want private", body.Visibility)
	}
	etagPriv := w2.Header().Get("ETag")
	if !strings.Contains(etagPriv, "~vprivate") || etagPriv == etagPub {
		t.Fatalf("private etag = %q, must differ from %q", etagPriv, etagPub)
	}
	// Unwired hook → empty field, bare ref etag (legacy shape).
	f.env.RepoVisibility = nil
	w3 := f.do("GET", "/demo/walgit/api", nil, nil, nil)
	decodeJSON(t, w3, &body)
	if body.Visibility != "" {
		t.Fatalf("unwired visibility = %q, want empty", body.Visibility)
	}
	if etag := w3.Header().Get("ETag"); etag != `"`+fakeSHA+`"` {
		t.Fatalf("unwired etag = %q, want bare ref", etag)
	}
}

func TestListingsVisibilityFilter(t *testing.T) {
	newListed := func(t *testing.T) (*fixture, *scriptedGate) {
		f := newFixture(t)
		f.reg.repos = map[string][]string{
			"acme": {"pub1", "priv1"},
			"solo": {"pub2"},
			"hide": {"priv2"},
		}
		g := &scriptedGate{readable: map[string]bool{
			"acme/pub1": true, "solo/pub2": true,
		}}
		f.env.Access = g
		f.env.RepoVisibility = func(_ context.Context, _, _ string) (string, bool) {
			return "public", true
		}
		return f, g
	}
	// Owners: the private-only owner vanishes for a filtered caller.
	f, _ := newListed(t)
	w := f.do("GET", "/api/v1/owners", nil, nil, &reader345)
	if w.Code != http.StatusOK {
		t.Fatalf("owners = %d", w.Code)
	}
	var owners []string
	decodeJSON(t, w, &owners)
	assertStrings(t, owners, []string{"acme", "solo"})
	// Owner repos: private names never list.
	w = f.do("GET", "/api/v1/owners/acme/repos", nil, nil, &reader345)
	var repos []string
	decodeJSON(t, w, &repos)
	assertStrings(t, repos, []string{"pub1"})
	// Private-only owner's repos read empty (never 404, never the name).
	w = f.do("GET", "/api/v1/owners/hide/repos", nil, nil, &reader345)
	repos = nil
	decodeJSON(t, w, &repos)
	if len(repos) != 0 {
		t.Fatalf("hide repos = %v, want []", repos)
	}
	// Admins bypass the filter with zero gate probes.
	fa, ga := newListed(t)
	before := ga.calls
	w = fa.do("GET", "/api/v1/owners", nil, nil, &admin345)
	var got []string
	decodeJSON(t, w, &got)
	assertStrings(t, got, []string{"acme", "hide", "solo"})
	if ga.calls != before {
		t.Fatalf("admin owners paid %d gate probes, want +0", ga.calls-before)
	}
	// Anonymous with the flag on sees the public slice (the privacy half:
	// flag admits, visibility filters).
	f.env.Cfg.Server.Auth.AnonymousRead = true
	w = f.do("GET", "/api/v1/owners", nil, nil, nil)
	owners = nil
	decodeJSON(t, w, &owners)
	assertStrings(t, owners, []string{"acme", "solo"})
	// Detailed owners: counts cover visible repos only; the hidden owner
	// is absent (never zero-valued).
	w = f.do("GET", "/api/v1/owners/detailed", nil, nil, &reader345)
	var det struct {
		Owners []OwnerActivityRow `json:"owners"`
	}
	decodeJSON(t, w, &det)
	counts := map[string]int{}
	for _, o := range det.Owners {
		counts[o.Name] = o.RepoCount
	}
	if len(counts) != 2 || counts["acme"] != 1 || counts["solo"] != 1 {
		t.Fatalf("detailed counts = %v, want acme:1 solo:1", counts)
	}
	// Repos detailed: one row, carrying the visibility spelling.
	w = f.do("GET", "/api/v1/owners/acme/repos/detailed", nil, nil, &reader345)
	var rows struct {
		Repos []RepoSizeRow `json:"repos"`
	}
	decodeJSON(t, w, &rows)
	if len(rows.Repos) != 1 || rows.Repos[0].Name != "pub1" {
		t.Fatalf("detailed rows = %+v, want [pub1]", rows.Repos)
	}
	if rows.Repos[0].Visibility != "public" {
		t.Fatalf("row visibility = %q, want public", rows.Repos[0].Visibility)
	}
	// Nil gate → legacy unfiltered behavior, byte-identical.
	fn, _ := newListed(t)
	fn.env.Access = nil
	w = fn.do("GET", "/api/v1/owners/acme/repos", nil, nil, &reader345)
	repos = nil
	decodeJSON(t, w, &repos)
	assertStrings(t, repos, []string{"pub1", "priv1"})
}

func TestVisibleOwnersHelper(t *testing.T) {
	newListed := func(t *testing.T) (*fixture, *scriptedGate) {
		f := newFixture(t)
		f.reg.repos = map[string][]string{
			"acme": {"pub1", "priv1"},
			"hide": {"priv2"},
		}
		g := &scriptedGate{readable: map[string]bool{"acme/pub1": true}}
		f.env.Access = g
		return f, g
	}
	// Filtered: the private-only owner drops out.
	f, _ := newListed(t)
	got, err := f.env.VisibleOwners(context.Background(), reader345)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, got, []string{"acme"})
	// Admin: unfiltered, +0 gate probes.
	fa, ga := newListed(t)
	before := ga.calls
	got, err = fa.env.VisibleOwners(context.Background(), admin345)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, got, []string{"acme", "hide"})
	if ga.calls != before {
		t.Fatalf("admin paid %d probes", ga.calls-before)
	}
	// Nil gate: legacy passthrough.
	fn, _ := newListed(t)
	fn.env.Access = nil
	got, err = fn.env.VisibleOwners(context.Background(), reader345)
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, got, []string{"acme", "hide"})
	// Nil registry → 503-class error (never a panic).
	fe, _ := newListed(t)
	fe.env.Repos = nil
	if _, err := fe.env.VisibleOwners(context.Background(), reader345); err == nil {
		t.Error("nil registry must error")
	}
	if _, err := fe.env.VisibleRepos(context.Background(), "acme", reader345); err == nil {
		t.Error("nil registry must error")
	}
	if _, err := fe.env.VisibleReposByOwner(context.Background(), reader345); err == nil {
		t.Error("nil registry must error")
	}
	// Registry failure propagates.
	ff, _ := newListed(t)
	ff.reg.fail = errBoom
	if _, err := ff.env.VisibleOwners(context.Background(), reader345); err == nil {
		t.Error("registry failure must propagate")
	}
}

func TestFilterCatalogRollup(t *testing.T) {
	mk := func(repo, sha, ts string) *proto.RepoCatalogEntry {
		when, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			t.Fatal(err)
		}
		stamp := proto.TimeFromGo(when)
		return &proto.RepoCatalogEntry{Repo: repo, LastCommitSHA: sha, LastCommitTime: &stamp}
	}
	cat := &proto.RepoCatalog{Entries: []*proto.RepoCatalogEntry{
		mk("acme/pub1", "aaa", "2026-01-01T00:00:00Z"),
		mk("acme/priv1", "zzz", "2026-09-01T00:00:00Z"),
		mk("solo/pub2", "mmm", "2026-06-01T00:00:00Z"),
		nil, // corrupt rows never panic the fold
		{Repo: "broken"},
	}}
	allow := map[string]map[string]bool{
		"acme": {"pub1": true},
		"solo": {"pub2": true},
	}
	got := filterCatalog(cat, allow)
	if len(got.Entries) != 2 {
		t.Fatalf("filtered entries = %d, want 2", len(got.Entries))
	}
	roll := sizecatalog.OwnerRollups(got)
	if roll["acme"].LastCommitSHA != "aaa" {
		t.Fatalf("acme rollup = %+v, want the public tip aaa", roll["acme"])
	}
	if roll["solo"].LastCommitSHA != "mmm" {
		t.Fatalf("solo rollup = %+v", roll["solo"])
	}
	if filterCatalog(nil, allow) != nil {
		t.Fatal("nil catalog must stay nil")
	}
}

func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
