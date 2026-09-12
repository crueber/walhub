package identity

// Org profile parity tests (Forgejo #359): location/timezone/bio_markdown
// round-trip through the service and the HTTP PUT, validation mirroring
// the owner-profile limits, and bucket back-compat in both directions.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestPutOrgProfileRoundTrip(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	upd, err := s.PutOrg(ctx, "acme", OrgEdit{
		DisplayName: "Acme Corp",
		Description: "we build things",
		Location:    "Berlin, DE",
		Timezone:    "Europe/Berlin",
		BioMarkdown: "# hi\n\nwe build things",
	})
	if err != nil {
		t.Fatalf("PutOrg: %v", err)
	}
	if upd.Version != 2 {
		t.Errorf("version = %d, want 2", upd.Version)
	}
	got, err := s.GetOrg(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "Acme Corp" || got.Description != "we build things" ||
		got.Location != "Berlin, DE" || got.Timezone != "Europe/Berlin" ||
		got.BioMarkdown != "# hi\n\nwe build things" {
		t.Errorf("round-trip broken: %+v", got)
	}
	// Full-document replace: absent fields clear.
	upd, err = s.PutOrg(ctx, "acme", OrgEdit{DisplayName: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	if upd.Location != "" || upd.Timezone != "" || upd.BioMarkdown != "" || upd.Description != "" {
		t.Errorf("PUT must clear absent fields: %+v", upd)
	}
	if upd.DisplayName != "Acme" {
		t.Errorf("display_name = %q", upd.DisplayName)
	}
}

func TestPutOrgProfileValidation(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", 201)
	longRunes := strings.Repeat("ü", 201) // 201 runes, 402 bytes
	okRunes := strings.Repeat("ü", 200)   // 200 runes: at the budget
	bigBio := strings.Repeat("b", maxOrgBio+1)
	okBio := strings.Repeat("b", maxOrgBio)
	for _, c := range []struct {
		name string
		edit OrgEdit
		ok   bool
	}{
		{"empty clears", OrgEdit{}, true},
		{"display at budget", OrgEdit{DisplayName: strings.Repeat("x", 200)}, true},
		{"display over budget", OrgEdit{DisplayName: long}, false},
		{"display rune budget", OrgEdit{DisplayName: longRunes}, false},
		{"display multibyte at budget", OrgEdit{DisplayName: okRunes}, true},
		{"location at budget", OrgEdit{Location: strings.Repeat("x", 200)}, true},
		{"location over budget", OrgEdit{Location: long}, false},
		{"timezone area/city", OrgEdit{Timezone: "Europe/Berlin"}, true},
		{"timezone utc", OrgEdit{Timezone: "UTC"}, true},
		{"timezone etc offset", OrgEdit{Timezone: "Etc/GMT+5"}, true},
		{"timezone too many segments", OrgEdit{Timezone: "a/b/c/d/e"}, false},
		{"timezone bad chars", OrgEdit{Timezone: "not a zone!"}, false},
		{"timezone empty segment", OrgEdit{Timezone: "Europe//Berlin"}, false},
		{"timezone over bytes", OrgEdit{Timezone: strings.Repeat("z", 65)}, false},
		{"bio at budget", OrgEdit{BioMarkdown: okBio}, true},
		{"bio over budget", OrgEdit{BioMarkdown: bigBio}, false},
		{"bio invalid utf8", OrgEdit{BioMarkdown: "ok\xffbad"}, false},
		// Description stays unbudgeted (pre-existing field, never
		// validated — a long tagline must not start 400ing).
		{"description unbounded", OrgEdit{Description: strings.Repeat("d", 1<<16)}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.PutOrg(ctx, "acme", c.edit)
			if c.ok && err != nil {
				t.Errorf("PutOrg(%+v) = %v, want success", c.edit, err)
			}
			if !c.ok && !errors.Is(err, ErrInvalid) {
				t.Errorf("PutOrg(%+v) = %v, want ErrInvalid", c.edit, err)
			}
		})
	}
}

func TestOrgProfileHTTPRoundTrip(t *testing.T) {
	s := testService()
	if _, err := s.CreateOrg(reqCtx(), "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	h := testHandler(s, alice)
	body := `{"display_name":"Acme Corp","description":"tag","location":"Berlin, DE",` +
		`"timezone":"Europe/Berlin","bio_markdown":"# hi"}`
	w := doReq(h, "PUT", "/api/v1/orgs/acme", body)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		DisplayName string `json:"display_name"`
		Description string `json:"description"`
		Location    string `json:"location"`
		Timezone    string `json:"timezone"`
		BioMarkdown string `json:"bio_markdown"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "Acme Corp" || got.Description != "tag" || got.Location != "Berlin, DE" ||
		got.Timezone != "Europe/Berlin" || got.BioMarkdown != "# hi" {
		t.Errorf("PUT body wrong: %+v", got)
	}
	// GET carries the fields on both lanes.
	for _, target := range []string{"/api/v1/orgs/acme", "/api-browser/v1/orgs/acme"} {
		w := doReq(h, "GET", target, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", target, w.Code)
		}
		var doc struct {
			Location    string `json:"location"`
			Timezone    string `json:"timezone"`
			BioMarkdown string `json:"bio_markdown"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Location != "Berlin, DE" || doc.Timezone != "Europe/Berlin" || doc.BioMarkdown != "# hi" {
			t.Errorf("GET %s missing profile fields: %s", target, w.Body.String())
		}
	}
	// Over-limit and malformed fields 400 with the mirror message.
	for _, c := range []struct {
		name, body, want string
	}{
		{"display too long", `{"display_name":"` + strings.Repeat("x", 201) + `"}`, "display_name too long"},
		{"location too long", `{"location":"` + strings.Repeat("x", 201) + `"}`, "location too long"},
		{"bad timezone", `{"timezone":"not a zone!"}`, "timezone must be an IANA zone name"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := doReq(h, "PUT", "/api/v1/orgs/acme", c.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("PUT = %d, want 400", w.Code)
			}
			if !strings.Contains(w.Body.String(), c.want) {
				t.Errorf("body %q lacks %q", w.Body.String(), c.want)
			}
		})
	}
	// Bio over 64 KiB 400s (body cap is 128 KiB so the field budget,
	// not the transport cap, fires).
	w = doReq(h, "PUT", "/api/v1/orgs/acme", `{"bio_markdown":"`+strings.Repeat("b", maxOrgBio+1)+`"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "bio_markdown too long") {
		t.Errorf("big bio = %d: %s", w.Code, w.Body.String())
	}
}

// TestOrgProfileBackCompat pins law 5 both ways: a legacy org.json
// (pre-#359, no new keys) reads with zero new fields, and a new org.json
// unmarshals into the legacy shape without error (old readers ignore new
// keys).
func TestOrgProfileBackCompat(t *testing.T) {
	legacy := `{"version":1,"org":"acme","display_name":"Acme","description":"d",` +
		`"created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-01T00:00:00Z"}`
	o, err := parseOrg([]byte(legacy))
	if err != nil {
		t.Fatalf("legacy org.json must parse: %v", err)
	}
	if o.Location != "" || o.Timezone != "" || o.BioMarkdown != "" ||
		o.AvatarContentType != "" || o.AvatarUpdatedAt != "" {
		t.Errorf("legacy doc must read zero new fields: %+v", o)
	}
	// New doc into the legacy shape: unknown keys ignored, old keys intact.
	type legacyOrg struct {
		Version     int    `json:"version"`
		Org         string `json:"org"`
		DisplayName string `json:"display_name"`
		Description string `json:"description"`
		CreatedAt   string `json:"created_at"`
		UpdatedAt   string `json:"updated_at"`
	}
	newDoc := `{"version":3,"org":"acme","display_name":"Acme Corp","description":"tag",` +
		`"location":"Berlin","timezone":"Europe/Berlin","bio_markdown":"# hi",` +
		`"avatar_content_type":"image/png","avatar_updated_at":"2026-09-12T00:00:00Z",` +
		`"created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-12T00:00:00Z"}`
	var lo legacyOrg
	if err := json.Unmarshal([]byte(newDoc), &lo); err != nil {
		t.Fatalf("new org.json must parse into the legacy shape: %v", err)
	}
	if lo.DisplayName != "Acme Corp" || lo.Description != "tag" || lo.Version != 3 {
		t.Errorf("legacy fields must survive: %+v", lo)
	}
	// A stored legacy org (bytes without new keys) serves through the new
	// code with zero new fields.
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "legacy", "Legacy", "d", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetOrg(ctx, "legacy")
	if err != nil || got == nil {
		t.Fatalf("GetOrg legacy: %v %+v", err, got)
	}
	if got.Location != "" || got.Timezone != "" || got.BioMarkdown != "" || got.AvatarContentType != "" {
		t.Errorf("stored legacy org must read zero new fields: %+v", got)
	}
}

// TestPutOrgPreservesAvatar pins the pointer discipline: a profile PUT
// edits profile fields only and never clears the avatar pointer (avatar
// changes go through the avatar endpoints).
func TestPutOrgPreservesAvatar(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutOrgAvatar(ctx, "acme", testPNG); err != nil {
		t.Fatal(err)
	}
	upd, err := s.PutOrg(ctx, "acme", OrgEdit{DisplayName: "Acme!", Location: "Berlin"})
	if err != nil {
		t.Fatal(err)
	}
	if upd.AvatarContentType != "image/png" || upd.AvatarUpdatedAt == "" {
		t.Errorf("profile PUT must preserve the avatar pointer: %+v", upd)
	}
	raw, ct, err := s.GetOrgAvatar(ctx, "acme")
	if err != nil || ct != "image/png" || string(raw) != string(testPNG) {
		t.Errorf("avatar bytes must survive a profile PUT: %q %v", ct, err)
	}
}
