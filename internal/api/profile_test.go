package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// ownerSelf returns a write principal whose name matches the owner slug.
func ownerSelf(owner string) *auth.Principal {
	p := auth.Principal{Name: owner, Write: true}
	return &p
}

// otherWriter is an authenticated writer who does not own the namespace.
func otherWriter() *auth.Principal { p := auth.Principal{Name: "mallory", Write: true}; return &p }

func TestOwnerProfileGetEmpty(t *testing.T) {
	f := newFixture(t)
	// Unknown owner reads as an empty profile (200, §8 ownerRepos convention).
	w := f.do("GET", "/api/v1/owners/ghost/profile", nil, nil, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != ccSWR {
		t.Fatalf("cache-control = %q, want SWR", cc)
	}
	var doc OwnerProfile
	decodeJSON(t, w, &doc)
	if doc.Owner != "ghost" || doc.DisplayName != "" || doc.Location != "" || doc.Timezone != "" || doc.BioMarkdown != "" {
		t.Fatalf("empty profile = %+v", doc)
	}
	if doc.CanEdit {
		t.Fatal("anonymous must not get can_edit")
	}
}

func TestOwnerProfileGetInvalidSlug(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{
		"/api/v1/owners/bad%20owner/profile", // space: not in the repo-id charset
		"/api/v1/owners/.hidden/profile",     // leading dot
		"/api/v1/owners/..%2F/profile",       // decoded ".." — rejected before key use
	} {
		if w := f.do("GET", path, nil, nil, nil); w.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, w.Code)
		}
		if w := f.do("PUT", path, strings.NewReader(`{}`), nil, ownerSelf("x")); w.Code != http.StatusNotFound {
			t.Fatalf("PUT %s = %d, want 404", path, w.Code)
		}
	}
}

func TestOwnerProfilePutRoundTrip(t *testing.T) {
	f := newFixture(t)
	body := `{"display_name":"Demo Team","location":"Berlin","timezone":"Europe/Berlin","bio_markdown":"# hi\n\nWe build things."}`
	w := f.do("PUT", "/api/v1/owners/demo/profile", strings.NewReader(body), nil, ownerSelf("demo"))
	if w.Code != 200 {
		t.Fatalf("PUT status = %d (%s)", w.Code, w.Body.String())
	}
	var doc OwnerProfile
	decodeJSON(t, w, &doc)
	if doc.Owner != "demo" || doc.DisplayName != "Demo Team" || doc.Location != "Berlin" ||
		doc.Timezone != "Europe/Berlin" || doc.BioMarkdown != "# hi\n\nWe build things." {
		t.Fatalf("PUT echo = %+v", doc)
	}
	if doc.UpdatedAt == "" {
		t.Fatal("PUT must stamp updated_at")
	}
	// GET (anonymous) sees the stored doc, without can_edit.
	w = f.do("GET", "/api/v1/owners/demo/profile", nil, nil, nil)
	var got OwnerProfile
	decodeJSON(t, w, &got)
	if got.DisplayName != "Demo Team" || got.Timezone != "Europe/Berlin" || got.UpdatedAt != doc.UpdatedAt {
		t.Fatalf("GET after PUT = %+v", got)
	}
	if got.CanEdit {
		t.Fatal("anonymous GET must not carry can_edit")
	}
	// GET as the owner carries can_edit.
	w = f.do("GET", "/api/v1/owners/demo/profile", nil, nil, ownerSelf("demo"))
	decodeJSON(t, w, &got)
	if !got.CanEdit {
		t.Fatal("owner GET must carry can_edit")
	}
	// Clearing a field is PUTting "": bio removed, rest kept.
	w = f.do("PUT", "/api/v1/owners/demo/profile",
		strings.NewReader(`{"display_name":"Demo Team","location":"Berlin","timezone":"Europe/Berlin","bio_markdown":""}`),
		nil, ownerSelf("demo"))
	if w.Code != 200 {
		t.Fatalf("clear PUT status = %d", w.Code)
	}
	w = f.do("GET", "/api/v1/owners/demo/profile", nil, nil, nil)
	decodeJSON(t, w, &got)
	if got.BioMarkdown != "" || got.DisplayName != "Demo Team" {
		t.Fatalf("after clear = %+v", got)
	}
}

func TestOwnerProfileAuthMatrix(t *testing.T) {
	f := newFixture(t)
	body := `{"display_name":"x"}`
	// Anonymous with auth required → 401 + Bearer challenge.
	f.env.Cfg.Server.Auth.AnonymousRead = false
	if w := f.do("PUT", "/api/v1/owners/demo/profile", strings.NewReader(body), nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anon (auth required) = %d, want 401", w.Code)
	}
	// Anonymous with public reads → 403 (never a silent allow).
	f.env.Cfg.Server.Auth.AnonymousRead = true
	if w := f.do("PUT", "/api/v1/owners/demo/profile", strings.NewReader(body), nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("anon (public) = %d, want 403", w.Code)
	}
	// Authenticated without write → 403.
	if w := f.do("PUT", "/api/v1/owners/demo/profile", strings.NewReader(body), nil, readP()); w.Code != http.StatusForbidden {
		t.Fatalf("reader = %d, want 403", w.Code)
	}
	// Writer, wrong namespace → 403.
	if w := f.do("PUT", "/api/v1/owners/demo/profile", strings.NewReader(body), nil, otherWriter()); w.Code != http.StatusForbidden {
		t.Fatalf("other writer = %d, want 403", w.Code)
	}
	// Host admin, any namespace → 200.
	admin := auth.Principal{Name: "root", Write: true, Admin: true}
	if w := f.do("PUT", "/api/v1/owners/demo/profile", strings.NewReader(body), nil, &admin); w.Code != 200 {
		t.Fatalf("admin = %d, want 200", w.Code)
	}
	// Name match is case-insensitive (slugs are lowercase by convention).
	upper := auth.Principal{Name: "DEMO", Write: true}
	if w := f.do("PUT", "/api/v1/owners/demo/profile", strings.NewReader(body), nil, &upper); w.Code != 200 {
		t.Fatalf("case-insensitive self = %d, want 200", w.Code)
	}
}

func TestOwnerProfileValidation(t *testing.T) {
	f := newFixture(t)
	self := ownerSelf("demo")
	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{oops`},
		{"unknown field", `{"display_name":"x","nickname":"y"}`},
		{"bad timezone charset", `{"timezone":"not a tz!!"}`},
		{"timezone too long", `{"timezone":"` + strings.Repeat("Area/", 20) + `City"}`},
		{"display name too long", `{"display_name":"` + strings.Repeat("é", 201) + `"}`},
		{"location too long", `{"location":"` + strings.Repeat("x", 201) + `"}`},
		{"bio too long", `{"bio_markdown":"` + strings.Repeat("x", maxProfileBio+1) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := f.do("PUT", "/api/v1/owners/demo/profile", strings.NewReader(tc.body), nil, self)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Fatalf("error must be plain text, got %q", ct)
			}
		})
	}
	// Etc/GMT+5 shape and bare UTC are accepted (shape, not tz-db).
	for _, tz := range []string{"UTC", "Etc/UTC", "Etc/GMT+5", "America/Argentina/Buenos_Aires"} {
		w := f.do("PUT", "/api/v1/owners/demo/profile", strings.NewReader(`{"timezone":"`+tz+`"}`), nil, self)
		if w.Code != 200 {
			t.Fatalf("timezone %q = %d, want 200 (%s)", tz, w.Code, w.Body.String())
		}
	}
}

func TestOwnerProfileTwins(t *testing.T) {
	f := newFixture(t)
	self := ownerSelf("demo")
	for _, base := range []string{"/api-browser/v1", "/services/api"} {
		w := f.do("PUT", base+"/owners/demo/profile", strings.NewReader(`{"location":"Oslo"}`), nil, self)
		if w.Code != 200 {
			t.Fatalf("PUT %s = %d", base, w.Code)
		}
		w = f.do("GET", base+"/owners/demo/profile", nil, nil, nil)
		if w.Code != 200 {
			t.Fatalf("GET %s = %d", base, w.Code)
		}
		var doc OwnerProfile
		decodeJSON(t, w, &doc)
		if doc.Location != "Oslo" {
			t.Fatalf("twin %s location = %q", base, doc.Location)
		}
	}
}

func TestOwnerProfileCorrupt(t *testing.T) {
	f := newFixture(t)
	if _, err := store.PutBytes(context.Background(), f.env.Store, store.OwnerProfileKey("demo"),
		[]byte("{corrupt"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	// A corrupt stored doc is a bucket error (503), never a 200 with garbage.
	if w := f.do("GET", "/api/v1/owners/demo/profile", nil, nil, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt GET = %d, want 503", w.Code)
	}
	// PUT still converges: the CAS loop replaces the corrupt body.
	if w := f.do("PUT", "/api/v1/owners/demo/profile", strings.NewReader(`{"display_name":"healed"}`),
		nil, ownerSelf("demo")); w.Code != 200 {
		t.Fatalf("healing PUT = %d, want 200", w.Code)
	}
}

// fakeOwnerEditor exercises the OwnerEditor seam (org-owner grant / probe
// failure) without importing the identity package.
type fakeOwnerEditor struct {
	allow map[string]bool
	err   error
}

func (e *fakeOwnerEditor) CanEditOwnerProfile(_ context.Context, owner string, _ auth.Principal) (bool, error) {
	if e.err != nil {
		return false, e.err
	}
	return e.allow[owner], nil
}

func TestOwnerProfileEditorSeam(t *testing.T) {
	f := newFixture(t)
	f.env.OwnerEdit = &fakeOwnerEditor{allow: map[string]bool{"acme": true}}
	// A non-matching writer edits the granted namespace (org-owner analog).
	if w := f.do("PUT", "/api/v1/owners/acme/profile", strings.NewReader(`{"display_name":"Acme"}`),
		nil, otherWriter()); w.Code != 200 {
		t.Fatalf("seam-granted PUT = %d, want 200", w.Code)
	}
	// ... but not an ungranted one.
	if w := f.do("PUT", "/api/v1/owners/other/profile", strings.NewReader(`{"display_name":"x"}`),
		nil, otherWriter()); w.Code != http.StatusForbidden {
		t.Fatalf("seam-denied PUT = %d, want 403", w.Code)
	}
	// GET carries the seam grant as can_edit.
	w := f.do("GET", "/api/v1/owners/acme/profile", nil, nil, otherWriter())
	var doc OwnerProfile
	decodeJSON(t, w, &doc)
	if !doc.CanEdit {
		t.Fatal("seam-granted GET must carry can_edit")
	}
	// Probe failure fails closed (503), never 403-as-404 or allow.
	f.env.OwnerEdit = &fakeOwnerEditor{err: context.DeadlineExceeded}
	if w := f.do("PUT", "/api/v1/owners/acme/profile", strings.NewReader(`{}`), nil, otherWriter()); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("probe-failure PUT = %d, want 503", w.Code)
	}
	// ... while reads degrade to can_edit=false, never a read error.
	w = f.do("GET", "/api/v1/owners/acme/profile", nil, nil, otherWriter())
	if w.Code != 200 {
		t.Fatalf("probe-failure GET = %d, want 200", w.Code)
	}
	var degraded OwnerProfile
	decodeJSON(t, w, &degraded)
	if degraded.CanEdit {
		t.Fatal("probe-failure GET must not carry can_edit")
	}
}

func TestOwnerProfileSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	st1, err := store.NewFilesystemRoot(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	f.env.Store = st1
	if w := f.do("PUT", "/api/v1/owners/demo/profile", strings.NewReader(`{"display_name":"Persistent"}`),
		nil, ownerSelf("demo")); w.Code != 200 {
		t.Fatalf("PUT = %d", w.Code)
	}
	// A fresh backend handle over the same directory (the restart analog:
	// no state outside the object store) reads the profile back.
	st2, err := store.NewFilesystemRoot(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	f.env.Store = st2
	w := f.do("GET", "/api/v1/owners/demo/profile", nil, nil, nil)
	var doc OwnerProfile
	decodeJSON(t, w, &doc)
	if doc.DisplayName != "Persistent" {
		t.Fatalf("after reopen = %+v", doc)
	}
}

// failStore fails every access: the 503 paths for store outages behind the
// profile endpoints (never a 200 with garbage, never a panic on nil meta).
type failStore struct {
	store.ObjectStore
	getErr error
	putErr error
}

func (s *failStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.ObjectStore.Get(ctx, key, opts)
}

func (s *failStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if s.putErr != nil {
		return store.ObjectMeta{}, s.putErr
	}
	return s.ObjectStore.Put(ctx, key, body, opts)
}

func TestOwnerProfileReadGate(t *testing.T) {
	f := newFixture(t)
	// Anonymous GET with auth required → 401 (the bio is public only when
	// anonymous_read admits it — same gate as every AuthRead route).
	f.env.Cfg.Server.Auth.AnonymousRead = false
	if w := f.do("GET", "/api/v1/owners/demo/profile", nil, nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anon GET (auth required) = %d, want 401", w.Code)
	}
	f.env.Cfg.Server.Auth.AnonymousRead = true
	if w := f.do("GET", "/api/v1/owners/demo/profile", nil, nil, nil); w.Code != 200 {
		t.Fatalf("anon GET (public) = %d, want 200", w.Code)
	}
}

func TestOwnerProfileNoStore(t *testing.T) {
	f := newFixture(t)
	f.env.Store = nil
	if w := f.do("GET", "/api/v1/owners/demo/profile", nil, nil, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET without store = %d, want 503", w.Code)
	}
	if w := f.do("PUT", "/api/v1/owners/demo/profile", strings.NewReader(`{}`), nil, ownerSelf("demo")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT without store = %d, want 503", w.Code)
	}
}

func TestOwnerProfileStoreOutage(t *testing.T) {
	f := newFixture(t)
	boom := &failStore{ObjectStore: f.env.Store, getErr: context.DeadlineExceeded, putErr: context.DeadlineExceeded}
	f.env.Store = boom
	// Read outage → 503, never an empty 200 masquerading as "unset".
	if w := f.do("GET", "/api/v1/owners/demo/profile", nil, nil, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET on outage = %d, want 503", w.Code)
	}
	// Write outage on the CAS read → 503.
	if w := f.do("PUT", "/api/v1/owners/demo/profile", strings.NewReader(`{}`), nil, ownerSelf("demo")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT on read outage = %d, want 503", w.Code)
	}
	// Write outage on the PUT itself → 503 (non-CAS error, no retry).
	boom.getErr = nil
	if w := f.do("PUT", "/api/v1/owners/demo/profile", strings.NewReader(`{}`), nil, ownerSelf("demo")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT on write outage = %d, want 503", w.Code)
	}
	// Perpetual CAS loss → 409 after the bounded loop, never a spin.
	boom.putErr = store.NewPrecondition("owners/demo/profile.json", "v2")
	if w := f.do("PUT", "/api/v1/owners/demo/profile", strings.NewReader(`{}`), nil, ownerSelf("demo")); w.Code != http.StatusConflict {
		t.Fatalf("PUT on CAS loss = %d, want 409", w.Code)
	}
}

func TestOwnerProfileBodyLimit(t *testing.T) {
	f := newFixture(t)
	big := `{"bio_markdown":"` + strings.Repeat("x", maxProfileBody) + `"}`
	if w := f.do("PUT", "/api/v1/owners/demo/profile", strings.NewReader(big), nil, ownerSelf("demo")); w.Code != http.StatusBadRequest {
		t.Fatalf("oversize body = %d, want 400", w.Code)
	}
}
