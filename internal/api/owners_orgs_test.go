package api

// owners_orgs_test.go — Forgejo #348 owners/detailed is_org marker.
//
// GET /api/v1/owners/detailed carries is_org per row (always present,
// never null) from the Env.Orgs OrgLister seam: one ListOrgs call per
// listing regardless of owner count (law 6). Nil seam → all false;
// list error → 200 with all false (display metadata fails open — the
// CollabCounts precedent, never a 503 for a badge).

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeOrgLister is the OrgLister seam double: fixed names or an error.
type fakeOrgLister struct {
	names []string
	err   error
}

func (f *fakeOrgLister) ListOrgs(_ context.Context) ([]string, error) {
	return f.names, f.err
}

func TestOwnersDetailedIsOrg(t *testing.T) {
	cases := []struct {
		name string
		orgs OrgLister
		want map[string]bool
	}{
		{"nil seam marks nothing", nil, map[string]bool{"alice": false, "bob": false, "ghost": false, "zed": false}},
		{"listed orgs marked", &fakeOrgLister{names: []string{"alice", "zed"}}, map[string]bool{"alice": true, "bob": false, "ghost": false, "zed": true}},
		{"empty list marks nothing", &fakeOrgLister{names: []string{}}, map[string]bool{"alice": false, "bob": false, "ghost": false, "zed": false}},
		{"unknown names ignored", &fakeOrgLister{names: []string{"nope"}}, map[string]bool{"alice": false, "bob": false, "ghost": false, "zed": false}},
		{"list error fails open", &fakeOrgLister{err: errors.New("bucket down")}, map[string]bool{"alice": false, "bob": false, "ghost": false, "zed": false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := multiOwnerFixture(t)
			f.env.Orgs = tc.orgs
			w := f.req("GET", "/api/v1/owners/detailed")
			if w.Code != 200 {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			rows := decodeOwnersDetailed(t, w.Body.Bytes())
			got := map[string]bool{}
			for _, r := range rows {
				got[r.Name] = r.IsOrg
			}
			for name, want := range tc.want {
				if got[name] != want {
					t.Errorf("is_org[%s] = %v, want %v (rows %+v)", name, got[name], want, rows)
				}
			}
		})
	}
}

// TestOwnersDetailedIsOrgPresent pins the wire shape: is_org is always
// present on every row (never omitted, never null) so old and new
// clients decode identically (14 §14.12).
func TestOwnersDetailedIsOrgPresent(t *testing.T) {
	f := multiOwnerFixture(t)
	f.env.Orgs = &fakeOrgLister{names: []string{"alice"}}
	w := f.req("GET", "/api/v1/owners/detailed")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var doc struct {
		Owners []map[string]any `json:"owners"`
	}
	decodeJSON(t, w, &doc)
	if len(doc.Owners) == 0 {
		t.Fatal("no rows")
	}
	for _, row := range doc.Owners {
		v, ok := row["is_org"]
		if !ok {
			t.Fatalf("row %v missing is_org", row)
		}
		if _, ok := v.(bool); !ok {
			t.Fatalf("row %v is_org = %T, want bool", row, v)
		}
	}
	raw := w.Body.String()
	if strings.Contains(raw, `"is_org":null`) {
		t.Fatalf("is_org must never be null: %.200s", raw)
	}
}

// TestOwnersDetailedIsOrgTwins pins the marker on every lane twin.
func TestOwnersDetailedIsOrgTwins(t *testing.T) {
	for _, p := range []string{"/api/v1/owners/detailed", "/api-browser/v1/owners/detailed", "/services/api/owners/detailed"} {
		f := multiOwnerFixture(t)
		f.env.Orgs = &fakeOrgLister{names: []string{"bob"}}
		w := f.req("GET", p)
		if w.Code != 200 {
			t.Fatalf("%s status = %d, want 200", p, w.Code)
		}
		rows := decodeOwnersDetailed(t, w.Body.Bytes())
		for _, r := range rows {
			want := r.Name == "bob"
			if r.IsOrg != want {
				t.Errorf("%s: is_org[%s] = %v, want %v", p, r.Name, r.IsOrg, want)
			}
		}
	}
}
