// orghooks_test.go — Forgejo #363: org-scoped webhooks (CRUD gating,
// validation, caps, fan-out incl. HMAC, membership events, sweep gate,
// retention guard). Package notify (white-box, same harness as the repo
// webhook tests).
package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// fakeOrgOwner is the test OrgOwnerChecker: named owners per org, plus
// an unavailable toggle for the 503-mapping path.
type fakeOrgOwner struct {
	owners      map[string]bool // "org\x00principal" → owner
	unavailable bool
}

func newFakeOrgOwner() *fakeOrgOwner { return &fakeOrgOwner{owners: map[string]bool{}} }

func (f *fakeOrgOwner) grant(org, principal string) { f.owners[org+"\x00"+principal] = true }

func (f *fakeOrgOwner) CheckOrgOwner(_ context.Context, org string, p auth.Principal) *auth.AuthError {
	if f.unavailable {
		return &auth.AuthError{Kind: auth.ErrUnavailable, Why: "directory down"}
	}
	if p.Admin || f.owners[org+"\x00"+p.Name] {
		return nil
	}
	if p.Anonymous {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	return &auth.AuthError{Kind: auth.ErrForbidden, Why: "org owner required"}
}

// orgHarness wires an owner-gated harness (amy owns acme).
func orgHarness(t *testing.T) (*harness, *fakeOrgOwner) {
	t.Helper()
	x := newHarness(t)
	own := newFakeOrgOwner()
	own.grant("acme", "amy@example.com")
	x.svc.OrgOwner = own
	return x, own
}

func orgCreate(t *testing.T, x *harness, org, principal, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/v1/orgs/"+org+"/webhooks", strings.NewReader(body))
	if principal != "" {
		r.Header.Set("X-Test-Principal", principal)
	}
	return do(x.handler, r)
}

// --- gating -------------------------------------------------------------------

func TestOrgWebhookGatingTable(t *testing.T) {
	x, _ := orgHarness(t)
	// Seed one hook as the owner for the id-bearing rows.
	rec := orgCreate(t, x, "acme", "amy@example.com", `{"url":"https://hooks.example/acme"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed create = %d %q", rec.Code, rec.Body.String())
	}
	var created map[string]any
	mustJSON(t, rec, &created)
	id, _ := created["id"].(string)

	paths := []struct{ method, path string }{
		{"GET", "/api/v1/orgs/acme/webhooks"},
		{"POST", "/api/v1/orgs/acme/webhooks"},
		{"GET", "/api/v1/orgs/acme/webhooks/" + id},
		{"PATCH", "/api/v1/orgs/acme/webhooks/" + id},
		{"DELETE", "/api/v1/orgs/acme/webhooks/" + id},
		{"POST", "/api/v1/orgs/acme/webhooks/" + id + "/ping"},
		{"GET", "/api/v1/orgs/acme/webhooks/" + id + "/deliveries"},
		{"GET", "/api-browser/v1/orgs/acme/webhooks"},
	}
	for _, tc := range paths {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var r *http.Request
			if tc.method == "POST" || tc.method == "PATCH" {
				r = httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			} else {
				r = httptest.NewRequest(tc.method, tc.path, nil)
			}
			// Anonymous → 401 with WWW-Authenticate.
			rec := do(x.handler, r)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("anon = %d, want 401", rec.Code)
			}
			if h := rec.Header().Get("WWW-Authenticate"); h == "" {
				t.Fatal("401 must carry WWW-Authenticate")
			}
			// Authenticated non-owner → 403.
			if tc.method == "POST" || tc.method == "PATCH" {
				r = httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			} else {
				r = httptest.NewRequest(tc.method, tc.path, nil)
			}
			r.Header.Set("X-Test-Principal", "bob@example.com")
			if rec := do(x.handler, r); rec.Code != http.StatusForbidden {
				t.Fatalf("non-owner = %d, want 403", rec.Code)
			}
		})
	}
}

func TestOrgWebhookBadOrgAndUnknown(t *testing.T) {
	x, _ := orgHarness(t)
	// Malformed org spelling never reaches the gate (404, fail closed).
	for _, p := range []string{
		"/api/v1/orgs/ACME!/webhooks",
		"/api/v1/orgs/acme/webhooks/nope",
		"/api/v1/orgs/acme/webhooks/nope/ping",
		"/api/v1/orgs/acme/webhooks/nope/deliveries",
	} {
		m := "GET"
		if strings.HasSuffix(p, "/ping") {
			m = "POST"
		}
		if strings.HasSuffix(p, "nope") && !strings.Contains(p, "/webhooks/nope/") {
			m = "GET"
		}
		r := httptest.NewRequest(m, p, nil)
		r.Header.Set("X-Test-Principal", "amy@example.com")
		if rec := do(x.handler, r); rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404", m, p, rec.Code)
		}
	}
	// PATCH/DELETE on unknown ids are 404 too.
	for _, m := range []string{"PATCH", "DELETE"} {
		r := httptest.NewRequest(m, "/api/v1/orgs/acme/webhooks/nope", strings.NewReader(`{}`))
		r.Header.Set("X-Test-Principal", "amy@example.com")
		if rec := do(x.handler, r); rec.Code != http.StatusNotFound {
			t.Fatalf("%s unknown = %d, want 404", m, rec.Code)
		}
	}
	// Host admin passes the gate with no checker wiring.
	x2 := newHarness(t)
	r := httptest.NewRequest("GET", "/api/v1/orgs/acme/webhooks", nil)
	r.Header.Set("X-Test-Principal", "root")
	r.Header.Set("X-Test-Admin", "1")
	if rec := do(x2.handler, r); rec.Code != http.StatusOK {
		t.Fatalf("host admin = %d, want 200", rec.Code)
	}
	// Nil checker fails closed for authenticated non-admins (403, not 500).
	r = httptest.NewRequest("GET", "/api/v1/orgs/acme/webhooks", nil)
	r.Header.Set("X-Test-Principal", "bob@example.com")
	if rec := do(x2.handler, r); rec.Code != http.StatusForbidden {
		t.Fatalf("nil-checker authed = %d, want 403", rec.Code)
	}
}

func TestOrgWebhookCheckerUnavailable(t *testing.T) {
	x, own := orgHarness(t)
	own.unavailable = true
	r := httptest.NewRequest("GET", "/api/v1/orgs/acme/webhooks", nil)
	r.Header.Set("X-Test-Principal", "amy@example.com")
	if rec := do(x.handler, r); rec.Code != http.StatusInternalServerError {
		t.Fatalf("unavailable = %d, want 500", rec.Code)
	}
}

// --- validation + CRUD ---------------------------------------------------------

func TestOrgWebhookValidationTable(t *testing.T) {
	x, _ := orgHarness(t)
	cases := []struct {
		name string
		org  string
		body string
		want int
	}{
		{"missing url", "acme", `{}`, 400},
		{"empty url", "acme", `{"url":""}`, 400},
		{"http non-loopback", "acme", `{"url":"http://hooks.example/x"}`, 400},
		{"bad scheme", "acme", `{"url":"ftp://hooks.example/x"}`, 400},
		{"unknown event", "acme", `{"url":"https://hooks.example/x","events":["push"]}`, 400},
		{"bad json", "acme", `{`, 400},
		{"bad org", "ACME!", `{"url":"https://hooks.example/x"}`, 404},
		{"https ok", "acme", `{"url":"https://hooks.example/x"}`, 201},
		{"loopback http ok", "acme", `{"url":"http://127.0.0.1:9/x"}`, 201},
		{"org action ok", "acme", `{"url":"https://hooks.example/x","events":["member_added","team_member_removed"]}`, 201},
		{"repo action ok", "acme", `{"url":"https://hooks.example/x","events":["commented","opened"]}`, 201},
		{"wildcard ok", "acme", `{"url":"https://hooks.example/x","events":["*"]}`, 201},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := orgCreate(t, x, tc.org, "amy@example.com", tc.body); rec.Code != tc.want {
				t.Fatalf("create = %d %q, want %d", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}

func TestOrgWebhookCRUD(t *testing.T) {
	x, _ := orgHarness(t)
	rec := orgCreate(t, x, "acme", "amy@example.com",
		`{"url":"https://hooks.example/acme","events":["member_added"],"secret":"s3"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %q", rec.Code, rec.Body.String())
	}
	var created map[string]any
	mustJSON(t, rec, &created)
	id, _ := created["id"].(string)
	if len(id) != 24 || created["secret_set"] != true {
		t.Fatalf("created = %v", created)
	}
	if _, has := created["secret"]; has {
		t.Fatal("secret must never be returned")
	}

	// Get hides the secret too.
	rec = do(x.handler, req(t, "GET", "/api/v1/orgs/acme/webhooks/"+id, "amy@example.com"))
	var got map[string]any
	mustJSON(t, rec, &got)
	if got["secret_set"] != true {
		t.Fatalf("get = %v", got)
	}
	if _, has := got["secret"]; has {
		t.Fatal("secret must never be returned")
	}

	// List shows it (no secret, arrays never null).
	rec = do(x.handler, req(t, "GET", "/api/v1/orgs/acme/webhooks", "amy@example.com"))
	var list struct {
		Webhooks []map[string]any `json:"webhooks"`
	}
	mustJSON(t, rec, &list)
	if len(list.Webhooks) != 1 {
		t.Fatalf("list = %+v", list)
	}

	// Patch events + deactivate.
	r := httptest.NewRequest("PATCH", "/api/v1/orgs/acme/webhooks/"+id, strings.NewReader(`{"events":["*"],"active":false}`))
	r.Header.Set("X-Test-Principal", "amy@example.com")
	if rec := do(x.handler, r); rec.Code != 200 {
		t.Fatalf("patch = %d %q", rec.Code, rec.Body.String())
	}
	// Bad patch is 400.
	r = httptest.NewRequest("PATCH", "/api/v1/orgs/acme/webhooks/"+id, strings.NewReader(`{"events":["nope"]}`))
	r.Header.Set("X-Test-Principal", "amy@example.com")
	if rec := do(x.handler, r); rec.Code != 400 {
		t.Fatalf("bad patch = %d, want 400", rec.Code)
	}
	// Ping on an inactive hook is 400.
	rec = do(x.handler, req(t, "POST", "/api/v1/orgs/acme/webhooks/"+id+"/ping", "amy@example.com"))
	if rec.Code != 400 {
		t.Fatalf("inactive ping = %d, want 400", rec.Code)
	}

	// Delete → 204, then get is 404 and list is [].
	rec = do(x.handler, req(t, "DELETE", "/api/v1/orgs/acme/webhooks/"+id, "amy@example.com"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %q", rec.Code, rec.Body.String())
	}
	rec = do(x.handler, req(t, "GET", "/api/v1/orgs/acme/webhooks/"+id, "amy@example.com"))
	if rec.Code != 404 {
		t.Fatalf("get after delete = %d, want 404", rec.Code)
	}
	rec = do(x.handler, req(t, "GET", "/api/v1/orgs/acme/webhooks", "amy@example.com"))
	var list2 struct {
		Webhooks []map[string]any `json:"webhooks"`
	}
	mustJSON(t, rec, &list2)
	if list2.Webhooks == nil || len(list2.Webhooks) != 0 {
		t.Fatalf("list after delete = %+v", list2)
	}
	// Re-delete is 404.
	rec = do(x.handler, req(t, "DELETE", "/api/v1/orgs/acme/webhooks/"+id, "amy@example.com"))
	if rec.Code != 404 {
		t.Fatalf("re-delete = %d, want 404", rec.Code)
	}
}

func TestOrgWebhookCapMirrorsRepo(t *testing.T) {
	x, _ := orgHarness(t)
	x.svc.MaxHooks = 1
	rec := orgCreate(t, x, "acme", "amy@example.com", `{"url":"https://hooks.example/a"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first = %d", rec.Code)
	}
	rec = orgCreate(t, x, "acme", "amy@example.com", `{"url":"https://hooks.example/b"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second = %d %q, want 409", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("error content-type = %q", ct)
	}
}

// --- fan-out --------------------------------------------------------------------

func TestOrgHookFanoutEndToEnd(t *testing.T) {
	x, _ := orgHarness(t)
	x.addProfile("amy@example.com", "bob@example.com")
	seedRepo(t, x, "acme", "one")
	seedRepo(t, x, "acme", "two")
	seedRepo(t, x, "other", "repo")
	sink, srv := newSink(t, "s3cr3t")
	defer srv.Close()

	hk, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{
		URL: strPtr(srv.URL), Secret: strPtr("s3cr3t"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Stored hook keeps the secret; the wire strips it.
	if got := x.svc.GetOrgHook(ctx(), "acme", hk.ID); got == nil || got.Secret != "s3cr3t" {
		t.Fatalf("secret not stored: %+v", got)
	}

	// One org event + one collab event in each member repo + one in a
	// foreign repo (must NOT fan out to the acme hook).
	x.svc.EmitOrgEvent(ctx(), "acme", OrgActionMemberAdded, "amy@example.com", "bob joined acme")
	x.writeThread(t, "acme", "one", 7, "T", "amy@example.com")
	x.svc.EmitIssue(ctx(), "acme/one", 7, "subscribed", "bob@example.com", "", "commented", []string{})
	x.writeThread(t, "acme", "two", 3, "U", "amy@example.com")
	x.svc.EmitIssue(ctx(), "acme/two", 3, "subscribed", "bob@example.com", "", "opened", []string{})
	x.writeThread(t, "other", "repo", 1, "V", "amy@example.com")
	x.svc.EmitIssue(ctx(), "other/repo", 1, "subscribed", "bob@example.com", "", "commented", []string{})

	x.svc.DeliverOrg(ctx(), "acme")
	if got := sink.count(); got != 3 {
		t.Fatalf("sink posts = %d, want 3 (1 org + 2 member-repo)", got)
	}
	// Cursors advanced on every scope.
	if c := readCursorAt(ctx(), x.svc, OrgHookCursorKey("acme", hk.ID)); c != 1 {
		t.Fatalf("org cursor = %d, want 1", c)
	}
	for _, repo := range []string{"one", "two"} {
		if c := readCursorAt(ctx(), x.svc, OrgHookRepoCursorKey("acme", hk.ID, repo)); c != 1 {
			t.Fatalf("repo cursor %s = %d, want 1", repo, c)
		}
	}
	// The foreign repo's event never touched the acme ring.
	d := x.svc.ReadOrgDeliveries(ctx(), "acme", hk.ID)
	if len(d.Entries) != 3 {
		t.Fatalf("deliveries = %+v", d.Entries)
	}
	for _, e := range d.Entries {
		if e.Status != 200 {
			t.Fatalf("delivery = %+v, want 200", e)
		}
	}
	// Second pass: nothing new, no redelivery.
	x.svc.DeliverOrg(ctx(), "acme")
	if got := sink.count(); got != 3 {
		t.Fatalf("redelivered: %d", got)
	}
}

func TestOrgHookFilterAndFailure(t *testing.T) {
	x, _ := orgHarness(t)
	x.addProfile("amy@example.com", "bob@example.com")
	seedRepo(t, x, "acme", "one")
	sink, srv := newSink(t, "")
	defer srv.Close()

	hk, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{
		URL: strPtr(srv.URL), Events: []string{OrgActionMemberAdded},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A repo event outside the filter advances the repo cursor without a POST.
	x.writeThread(t, "acme", "one", 7, "T", "amy@example.com")
	x.svc.EmitIssue(ctx(), "acme/one", 7, "subscribed", "bob@example.com", "", "commented", []string{})
	// An org event outside the filter advances the org cursor without a POST.
	x.svc.EmitOrgEvent(ctx(), "acme", OrgActionTeamCreated, "", "team acme/devs created")
	x.svc.DeliverOrg(ctx(), "acme")
	if got := sink.count(); got != 0 {
		t.Fatalf("filtered posts = %d", got)
	}
	if c := readCursorAt(ctx(), x.svc, OrgHookCursorKey("acme", hk.ID)); c != 1 {
		t.Fatalf("org cursor = %d, want 1", c)
	}
	if c := readCursorAt(ctx(), x.svc, OrgHookRepoCursorKey("acme", hk.ID, "one")); c != 1 {
		t.Fatalf("repo cursor = %d, want 1", c)
	}
	// Failure holds the cursor; the ring records the 500.
	sink.fail = true
	x.svc.EmitOrgEvent(ctx(), "acme", OrgActionMemberAdded, "", "carol joined acme")
	x.svc.DeliverOrg(ctx(), "acme")
	if got := sink.count(); got != 0 {
		t.Fatalf("failed posts = %d", got)
	}
	if c := readCursorAt(ctx(), x.svc, OrgHookCursorKey("acme", hk.ID)); c != 1 {
		t.Fatalf("held cursor = %d, want 1", c)
	}
	d := x.svc.ReadOrgDeliveries(ctx(), "acme", hk.ID)
	if len(d.Entries) != 1 || d.Entries[0].Status != 500 || d.Entries[0].Event != OrgActionMemberAdded {
		t.Fatalf("deliveries = %+v", d.Entries)
	}
	// Recovery delivers exactly once.
	sink.fail = false
	x.svc.DeliverOrg(ctx(), "acme")
	if got := sink.count(); got != 1 {
		t.Fatalf("recovered posts = %d, want 1", got)
	}
	if c := readCursorAt(ctx(), x.svc, OrgHookCursorKey("acme", hk.ID)); c != 2 {
		t.Fatalf("recovered cursor = %d, want 2", c)
	}
}

func TestOrgHookPing(t *testing.T) {
	x, _ := orgHarness(t)
	sink, srv := newSink(t, "s3cr3t")
	defer srv.Close()
	hk, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{
		URL: strPtr(srv.URL), Secret: strPtr("s3cr3t"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := x.svc.PingOrgHook(ctx(), "acme", hk.ID, "amy@example.com")
	if err != nil || !ok {
		t.Fatalf("ping = %v, %v", ok, err)
	}
	if got := sink.count(); got != 1 {
		t.Fatalf("ping posts = %d", got)
	}
	// No-backlog ping advances the org cursor past the ping (no redelivery).
	if c := readCursorAt(ctx(), x.svc, OrgHookCursorKey("acme", hk.ID)); c != 1 {
		t.Fatalf("ping cursor = %d, want 1", c)
	}
	x.svc.DeliverOrg(ctx(), "acme")
	if got := sink.count(); got != 1 {
		t.Fatalf("ping redelivered: %d", got)
	}
	// Unknown hook → 404-shaped error; inactive → invalid.
	if _, err := x.svc.PingOrgHook(ctx(), "acme", "nope", "amy"); !isErr(err, ErrNotFound) {
		t.Fatalf("ping unknown = %v, want not found", err)
	}
	if _, err := x.svc.PatchOrgHook(ctx(), "acme", hk.ID, HookSpec{Active: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.svc.PingOrgHook(ctx(), "acme", hk.ID, "amy"); !isErr(err, ErrInvalid) {
		t.Fatalf("ping inactive = %v, want invalid", err)
	}
}

// --- emission ---------------------------------------------------------------------

func TestEmitOrgEventTable(t *testing.T) {
	x, _ := orgHarness(t)
	// Unknown action reserves nothing (no gap, no wake).
	x.svc.EmitOrgEvent(ctx(), "acme", "push", "amy@example.com", "x")
	if head := x.svc.orgHead(ctx(), "acme"); head != 0 {
		t.Fatalf("head after unknown action = %d, want 0", head)
	}
	// Bad org spelling drops before any store write.
	x.svc.EmitOrgEvent(ctx(), "ACME!", OrgActionMemberAdded, "", "x")
	if head := x.svc.orgHead(ctx(), "ACME!"); head != 0 {
		t.Fatalf("head after bad org = %d", head)
	}
	// Every org action appends a Kind=org event naming the org.
	for _, action := range []string{
		OrgActionMemberAdded, OrgActionMemberRemoved, OrgActionMemberRoleChanged,
		OrgActionTeamCreated, OrgActionTeamDeleted,
		OrgActionTeamMemberAdded, OrgActionTeamMemberRemoved,
		OrgActionInviteCreated, OrgActionInviteAccepted, OrgActionInviteCancelled,
	} {
		x.svc.EmitOrgEvent(ctx(), "acme", action, "amy@example.com", action+" happened")
	}
	if head := x.svc.orgHead(ctx(), "acme"); head != 10 {
		t.Fatalf("head = %d, want 10", head)
	}
	for seq := 1; seq <= 10; seq++ {
		ev := x.svc.readOrgEvent(ctx(), "acme", seq)
		if ev == nil {
			t.Fatalf("seq %d missing", seq)
		}
		if ev.Kind != OrgHookKind || ev.Repo != "acme" || ev.Seq != seq {
			t.Fatalf("seq %d = %+v", seq, ev)
		}
		if ev.Actor != "amy@example.com" || ev.Title == "" || ev.At == "" {
			t.Fatalf("seq %d attribution = %+v", seq, ev)
		}
	}
}

// --- sweep gate + tasks --------------------------------------------------------------

func TestOrgSweepGate(t *testing.T) {
	x, _ := orgHarness(t)
	// Task IDs derive from the service clock and the harness freezes
	// it: advance the clock per call so successive passes are
	// distinguishable records (same convention as the task-table tests).
	tick := 0
	x.svc.Now = func() time.Time {
		tick++
		return x.now.Add(time.Duration(tick) * time.Minute)
	}
	// No hooks anywhere: the sweep schedules nothing.
	x.svc.sweepOrgHooks(ctx())
	if rec := x.svc.TaskStatus(orgTaskRepo("acme"), TaskKindOrgWebhooks); rec != nil {
		t.Fatalf("hookless sweep started %+v", rec)
	}
	sink, srv := newSink(t, "")
	defer srv.Close()
	if _, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{URL: strPtr(srv.URL)}); err != nil {
		t.Fatal(err)
	}
	// The create armed pending: the sweep starts exactly one pass.
	x.svc.sweepOrg(ctx(), "acme")
	first := x.svc.TaskStatus(orgTaskRepo("acme"), TaskKindOrgWebhooks)
	if first == nil {
		t.Fatal("armed sweep started nothing")
	}
	waitOrgTask(t, x, "acme")
	// Quiet + drained: the next sweep starts no new pass (same record).
	x.svc.sweepOrg(ctx(), "acme")
	second := x.svc.TaskStatus(orgTaskRepo("acme"), TaskKindOrgWebhooks)
	if second == nil || second.ID != first.ID {
		t.Fatalf("quiet sweep started a new pass: %+v", second)
	}
	// A member-repo emission trips the repo half of the gate. A second
	// member repo with no activity log probes absent (head -1, skipped).
	seedRepo(t, x, "acme", "one")
	seedRepo(t, x, "acme", "two")
	x.addProfile("amy@example.com", "bob@example.com")
	x.writeThread(t, "acme", "one", 7, "T", "amy@example.com")
	x.svc.EmitIssue(ctx(), "acme/one", 7, "subscribed", "bob@example.com", "", "commented", []string{})
	x.svc.sweepOrg(ctx(), "acme")
	third := x.svc.TaskStatus(orgTaskRepo("acme"), TaskKindOrgWebhooks)
	if third == nil || third.ID == second.ID {
		t.Fatal("repo activity did not schedule an org pass")
	}
	waitOrgTask(t, x, "acme")
	if got := sink.count(); got != 1 {
		t.Fatalf("sweep-driven posts = %d, want 1", got)
	}
}

// waitOrgTask polls for the org pass to finish (delivery is async under
// the task table; the store work itself is synchronous in the leader).
func waitOrgTask(t *testing.T, x *harness, org string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec := x.svc.TaskStatus(orgTaskRepo(org), TaskKindOrgWebhooks)
		if rec != nil && rec.State == TaskFinished {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("org task for %s never finished: %+v", org, rec)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOrgDeleteDropsCursorsAndRing(t *testing.T) {
	x, _ := orgHarness(t)
	x.addProfile("amy@example.com", "bob@example.com")
	seedRepo(t, x, "acme", "one")
	sink, srv := newSink(t, "")
	defer srv.Close()
	hk, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{URL: strPtr(srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	x.svc.EmitOrgEvent(ctx(), "acme", OrgActionMemberAdded, "", "x")
	x.writeThread(t, "acme", "one", 7, "T", "amy@example.com")
	x.svc.EmitIssue(ctx(), "acme/one", 7, "subscribed", "bob@example.com", "", "commented", []string{})
	x.svc.DeliverOrg(ctx(), "acme")
	if got := sink.count(); got != 2 {
		t.Fatalf("posts = %d, want 2", got)
	}
	if err := x.svc.DeleteOrgHook(ctx(), "acme", hk.ID); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		OrgHookKey("acme", hk.ID),
		OrgHookCursorKey("acme", hk.ID),
		OrgHookRepoCursorKey("acme", hk.ID, "one"),
		OrgHookDeliveriesKey("acme", hk.ID),
	} {
		if ok, _ := store.Exists(ctx(), x.svc.Store, key); ok {
			t.Fatalf("key survives delete: %s", key)
		}
	}
}

// --- retention guard -------------------------------------------------------------------

func TestOrgRetentionHoldsFloor(t *testing.T) {
	x, _ := orgHarness(t)
	x.addProfile("amy@example.com", "bob@example.com")
	seedRepo(t, x, "acme", "one")
	sink, srv := newSink(t, "")
	defer srv.Close()
	hk, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{URL: strPtr(srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	// Two OLD activity events (past the 7-day floor) with the org
	// cursor still at zero: retention must hold the floor for
	// undelivered org fan-out.
	old := x.now.AddDate(0, 0, -30).Format(dateTimeFmt)
	targets := []target{{principal: "bob@example.com", reason: ReasonSubscribed}}
	for i := 0; i < 2; i++ {
		seq, err := x.svc.reserveSeq(ctx(), "acme", "one")
		if err != nil {
			t.Fatal(err)
		}
		if err := x.svc.appendActivity(ctx(), "acme", "one", seq,
			Emission{Repo: "acme/one", Num: 7, Kind: "issue", Class: "subscribed"},
			ActionCommented, "T", "amy@example.com", old, targets, false); err != nil {
			t.Fatal(err)
		}
	}
	if got := x.svc.orgRepoMinCursor(ctx(), "acme", "one"); got != 0 {
		t.Fatalf("org floor = %d, want 0 (undelivered)", got)
	}
	x.svc.retainRepoEvents(ctx(), "acme", "one", x.now)
	for _, seq := range []int{1, 2} {
		if ev := x.svc.readActivity(ctx(), "acme", "one", seq); ev == nil {
			t.Fatalf("retention deleted seq %d below the org cursor", seq)
		}
	}
	// After the org pass delivers both, the floor releases: the event
	// strictly below the cursor compacts (the event AT the cursor is
	// kept — repo-hook parity, cursors point at the last delivered).
	x.svc.DeliverOrg(ctx(), "acme")
	if got := sink.count(); got != 2 {
		t.Fatalf("posts = %d, want 2", got)
	}
	x.svc.retainRepoEvents(ctx(), "acme", "one", x.now)
	if ev := x.svc.readActivity(ctx(), "acme", "one", 1); ev != nil {
		t.Fatalf("seq 1 survives past the released floor: %+v", ev)
	}
	if ev := x.svc.readActivity(ctx(), "acme", "one", 2); ev == nil {
		t.Fatal("seq 2 (at cursor) must be kept")
	}
	_ = hk
}

func TestOrgRepoMinCursorEmpty(t *testing.T) {
	x, _ := orgHarness(t)
	// No org hooks: no floor (-1); inactive hooks do not hold it either.
	if got := x.svc.orgRepoMinCursor(ctx(), "acme", "one"); got != -1 {
		t.Fatalf("floor without hooks = %d, want -1", got)
	}
	_, srv := newSink(t, "")
	defer srv.Close()
	hk, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com",
		HookSpec{URL: strPtr(srv.URL), Active: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if got := x.svc.orgRepoMinCursor(ctx(), "acme", "one"); got != -1 {
		t.Fatalf("floor with inactive hook = %d, want -1", got)
	}
	_ = hk
}

// --- misc ---------------------------------------------------------------------

func TestValidateOrgHookEvents(t *testing.T) {
	for _, events := range [][]string{
		nil, {}, {"*"},
		{OrgActionMemberAdded, OrgActionInviteCancelled},
		{ActionCommented, ActionPing, ActionOpened},
		{OrgActionTeamCreated, ActionClosed},
	} {
		if err := validateOrgHookEvents(events); err != nil {
			t.Fatalf("%v: %v", events, err)
		}
	}
	for _, events := range [][]string{{"push"}, {"member_added", "nope"}, {""}} {
		if err := validateOrgHookEvents(events); err == nil {
			t.Fatalf("%v: want error", events)
		}
	}
}

func TestOrgHookKeys(t *testing.T) {
	if got := OrgHookKey("acme", "id"); got != "orgs/acme/webhooks/id.json" {
		t.Fatalf("hook key = %q", got)
	}
	if got := OrgHookCursorKey("acme", "id"); got != "orgs/acme/webhooks/cursors/id.json" {
		t.Fatalf("cursor key = %q", got)
	}
	if got := OrgHookRepoCursorKey("acme", "id", "one"); got != "orgs/acme/webhooks/cursors/id/repos/one.json" {
		t.Fatalf("repo cursor key = %q", got)
	}
	if got := OrgHookDeliveriesKey("acme", "id"); got != "orgs/acme/webhooks/id/deliveries/recent.json" {
		t.Fatalf("deliveries key = %q", got)
	}
	if got := OrgEventKey("acme", 1); got != "orgs/acme/orgevents/000000000001.json" {
		t.Fatalf("event key = %q", got)
	}
	if got := OrgHookStateKey("acme"); got != "orgs/acme/meta/orghook_state.json" {
		t.Fatalf("state key = %q", got)
	}
}

func TestOrgDeliveriesEmpty(t *testing.T) {
	x, _ := orgHarness(t)
	d := x.svc.ReadOrgDeliveries(ctx(), "acme", "nope")
	if d.Entries == nil || len(d.Entries) != 0 {
		t.Fatalf("empty ring = %+v", d)
	}
}

func TestStartOrgWebhooksValidation(t *testing.T) {
	x, _ := orgHarness(t)
	if rec := x.svc.StartOrgWebhooks(ctx(), "ACME!"); rec != nil {
		t.Fatalf("bad org started %+v", rec)
	}
	x.svc.Drain()
	if rec := x.svc.StartOrgWebhooks(ctx(), "acme"); rec != nil {
		t.Fatalf("drained start = %+v, want nil", rec)
	}
}

func TestOrgHookEventShape(t *testing.T) {
	x, _ := orgHarness(t)
	x.svc.EmitOrgEvent(ctx(), "acme", OrgActionMemberAdded, "amy@example.com", "t")
	raw, _, err := store.GetBytes(ctx(), x.svc.Store, OrgEventKey("acme", 1), store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var ev ActivityEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Kind != OrgHookKind || ev.Repo != "acme" || ev.Action != OrgActionMemberAdded {
		t.Fatalf("event = %+v", ev)
	}
}

func boolPtr(b bool) *bool { return &b }
