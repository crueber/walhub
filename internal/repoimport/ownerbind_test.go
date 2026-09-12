// ownerbind_test.go — Forgejo #346: creation/import owner admission matrix.
//
// checkCreate consults the shared identity admission rule (owner == self,
// member org, or host admin) BEFORE any namespace write. The matrix below
// runs against the REAL identity service (memory store + real orgs), so it
// pins the production wiring, not a fake. Deny cases go through Begin to
// prove no partial state (no running entry, no stream, no task); the 403
// bodies must name the allowed owners.
package repoimport

import (
	"context"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

func ownerSvc(t *testing.T, cfgRoles func(st store.ObjectStore) RoleService) (*Service, store.ObjectStore) {
	t.Helper()
	cfg := testConfig(t)
	st := store.NewMemory()
	return testServiceOnStore(t, cfg, cfgRoles(st), st)
}

func seedOrgs(t *testing.T, st store.ObjectStore) {
	t.Helper()
	ctx := context.Background()
	ident := identity.New(st, testConfig(t))
	if _, err := ident.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatalf("CreateOrg acme: %v", err)
	}
	if _, err := ident.SetMember(ctx, "acme", "bob@example.com", identity.OrgMember); err != nil {
		t.Fatalf("SetMember bob: %v", err)
	}
	if _, err := ident.CreateOrg(ctx, "solo", "Solo", "", "carol@example.com"); err != nil {
		t.Fatalf("CreateOrg solo: %v", err)
	}
}

func mkImportParams(t *testing.T, owner string) Params {
	t.Helper()
	p, _, err := ParseRequest([]byte(`{"source_url":"file:///srv/r.git","owner":"`+owner+`","name":"w"}`), testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckCreateOwnerMatrix(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	st := store.NewMemory()
	seedOrgs(t, st)
	svc, _ := testServiceOnStore(t, cfg, realRoles(st, cfg), st)

	writer := func(name string) auth.Principal { return auth.Principal{Name: name, Write: true} }
	admin := auth.Principal{Name: "root@example.com", Write: true, Admin: true}
	tests := []struct {
		name      string
		owner     string
		p         auth.Principal
		want      int // 0 = allow
		wantMsgIn []string
	}{
		{"self", "alice@example.com", writer("alice@example.com"), 0, nil},
		// Forgejo #370: usernames are valid owners — an import under
		// the user's username passes admission (the email-owner bad
		// target failure is gone because the owner is never an email).
		{"username self", "crueber", writer("crueber"), 0, nil},
		{"org owner", "acme", writer("alice@example.com"), 0, nil},
		{"org member", "acme", writer("bob@example.com"), 0, nil},
		{"nonmember org", "acme", writer("mallory@example.com"), 403,
			[]string{`"acme"`, "mallory@example.com"}},
		{"other user", "carol@example.com", writer("alice@example.com"), 403,
			[]string{`"carol@example.com"`, "alice@example.com"}},
		{"unclaimed foreign prefix", "free", writer("mallory@example.com"), 403,
			[]string{`"free"`, "mallory@example.com"}},
		{"anonymous", "acme", auth.Anonymous(), 401, nil},
		{"admin bypass", "solo", admin, 0, nil},
		{"self without host write still gated", "pleb", auth.Principal{Name: "pleb"}, 403, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.checkCreate(ctx, tc.p, tc.owner, "w")
			if tc.want == 0 {
				if err != nil {
					t.Fatalf("checkCreate(%q) = %v, want allow", tc.owner, err)
				}
				return
			}
			se, ok := err.(*StatusError)
			if !ok || se.Status != tc.want {
				t.Fatalf("checkCreate(%q) = %v, want %d", tc.owner, err, tc.want)
			}
			for _, sub := range tc.wantMsgIn {
				if !strings.Contains(se.Message, sub) {
					t.Fatalf("message %q must contain %q", se.Message, sub)
				}
			}
		})
	}
}

// TestBeginOwnerDenyNoPartialState proves the deny lands before any
// namespace write: Begin fails and installs no running entry, no stream,
// and no task.
func TestBeginOwnerDenyNoPartialState(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	st := store.NewMemory()
	seedOrgs(t, st)
	svc, _ := testServiceOnStore(t, cfg, realRoles(st, cfg), st)

	p := auth.Principal{Name: "mallory@example.com", Write: true}
	_, _, err := svc.Begin(ctx, p, mkImportParams(t, "acme"), "")
	se, ok := err.(*StatusError)
	if !ok || se.Status != 403 {
		t.Fatalf("Begin = %v, want 403", err)
	}
	if !strings.Contains(se.Message, "mallory@example.com") {
		t.Fatalf("403 must name the allowed owners: %q", se.Message)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if len(svc.running) != 0 || len(svc.streams) != 0 {
		t.Fatalf("deny left partial state: running=%d streams=%d", len(svc.running), len(svc.streams))
	}
}

// TestBeginOwnerProbeError proves a roster probe failure answers 503,
// never 403-as-404.
func TestBeginOwnerProbeError(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	broken := &errGetStore{ObjectStore: store.NewMemory()}
	svc, _ := testServiceOnStore(t, cfg, realRoles(broken, cfg), broken)

	p := auth.Principal{Name: "alice@example.com", Write: true}
	_, _, err := svc.Begin(ctx, p, mkImportParams(t, "acme"), "")
	se, ok := err.(*StatusError)
	if !ok || se.Status != 503 {
		t.Fatalf("Begin = %v, want 503", err)
	}
}
