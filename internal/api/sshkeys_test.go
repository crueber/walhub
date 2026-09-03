package api

// sshkeys_test.go — the /api/v1/ssh-keys surface (17_ssh.md §3): ownership,
// gates, and the registry error mapping, over a stub store.

import (
	"context"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

type stubSSHKeys struct {
	recs   map[string]SshKeyRecord
	addErr error
	getErr error
}

func (s *stubSSHKeys) List(_ context.Context, principal string) ([]SshKeyRecord, error) {
	var out []SshKeyRecord
	for _, r := range s.recs {
		if r.Principal == principal {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *stubSSHKeys) Add(_ context.Context, principal, key, title string) (SshKeyRecord, error) {
	if s.addErr != nil {
		return SshKeyRecord{}, s.addErr
	}
	rec := SshKeyRecord{Principal: principal, ID: "fp-1", Key: key, Title: title}
	s.recs[rec.ID] = rec
	return rec, nil
}

func (s *stubSSHKeys) Get(_ context.Context, principal, id string) (SshKeyRecord, error) {
	if s.getErr != nil {
		return SshKeyRecord{}, s.getErr
	}
	rec, ok := s.recs[id]
	if !ok {
		return SshKeyRecord{}, ErrKeyNotFound
	}
	if rec.Principal != principal {
		return SshKeyRecord{}, ErrKeyForbidden
	}
	return rec, nil
}

func (s *stubSSHKeys) Delete(_ context.Context, principal, id string) error {
	if _, err := s.Get(nil, principal, id); err != nil {
		return err
	}
	delete(s.recs, id)
	return nil
}

const validKeyLine = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAII/VO93dFREgk2CscLYyCKH4ZjDpD8XYGB/X8ReU2QWx ada@laptop"

func TestSSHKeysAPINoneMode(t *testing.T) {
	f := newFixture(t) // auth none? the fixture is token mode — build none here
	f.env.Cfg.Server.Auth.Mode = "none"
	f.env.Cfg.Server.Auth.AnonymousRead = true
	stub := &stubSSHKeys{recs: map[string]SshKeyRecord{}}
	f.env.SSHKeys = stub

	// anonymous caller (no principal): none mode → anon, write allowed
	w := f.do("POST", "/api/v1/ssh-keys", strings.NewReader(`{"key": "`+validKeyLine+`", "title": "laptop"}`), nil, nil)
	if w.Code != 201 {
		t.Fatalf("add = %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"principal":"anon"`) {
		t.Fatalf("record principal = %s", w.Body.String())
	}

	// list shows it
	w = f.do("GET", "/api/v1/ssh-keys", nil, nil, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "fp-1") {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}

	// duplicate → 409
	stub.addErr = ErrKeyDuplicate
	w = f.do("POST", "/api/v1/ssh-keys", strings.NewReader(`{"key": "`+validKeyLine+`"}`), nil, nil)
	if w.Code != 409 {
		t.Fatalf("duplicate = %d", w.Code)
	}
	stub.addErr = nil

	// malformed key → 400 (the registry's format error)
	stub.addErr = ErrKeyBadKeyLine
	w = f.do("POST", "/api/v1/ssh-keys", strings.NewReader(`{"key": "junk"}`), nil, nil)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "authorized_keys") {
		t.Fatalf("bad key = %d %s", w.Code, w.Body.String())
	}
	stub.addErr = nil

	// empty key → 400 before the registry
	w = f.do("POST", "/api/v1/ssh-keys", strings.NewReader(`{"key": "  "}`), nil, nil)
	if w.Code != 400 {
		t.Fatalf("empty key = %d", w.Code)
	}

	// delete: wrong owner -> 403; unknown -> 404; own -> 200
	w = f.do("DELETE", "/api/v1/ssh-keys/fp-1", nil, nil, &auth.Principal{Name: "someoneelse", Write: true})
	if w.Code != 403 {
		t.Fatalf("foreign delete = %d", w.Code)
	}
	w = f.do("DELETE", "/api/v1/ssh-keys/missing-fp", nil, nil, nil)
	if w.Code != 404 {
		t.Fatalf("missing delete = %d", w.Code)
	}
	w = f.do("DELETE", "/api/v1/ssh-keys/fp-1", nil, nil, nil)
	if w.Code != 200 {
		t.Fatalf("own delete = %d", w.Code)
	}
	if _, ok := stub.recs["fp-1"]; ok {
		t.Fatal("record must be gone")
	}
}

func TestSSHKeysAPINotConfigured(t *testing.T) {
	f := newFixture(t)
	// SSHKeys nil → 503 on every verb
	w := f.do("GET", "/api/v1/ssh-keys", nil, nil, readP())
	if w.Code != 503 {
		t.Fatalf("list = %d %s, want 503", w.Code, w.Body.String())
	}
	w = f.do("POST", "/api/v1/ssh-keys", strings.NewReader(`{"key": "x"}`), nil, readP())
	if w.Code != 503 {
		t.Fatalf("add = %d %s, want 503", w.Code, w.Body.String())
	}
	w = f.do("DELETE", "/api/v1/ssh-keys/x", nil, nil, readP())
	if w.Code != 503 {
		t.Fatalf("delete = %d, want 503", w.Code)
	}
}

func TestSSHKeysAPIWriteGate(t *testing.T) {
	// AuthWrite was relaxed to AuthRead for add/delete (identity self-service):
	// a read-only principal may manage their own keys (their SSH rights still
	// resolve per principal at auth time).
	f := newFixture(t)
	stub := &stubSSHKeys{recs: map[string]SshKeyRecord{}}
	f.env.SSHKeys = stub
	w := f.do("POST", "/api/v1/ssh-keys", strings.NewReader(`{"key": "`+validKeyLine+`"}`), nil, readP())
	if w.Code != 201 {
		t.Fatalf("read-only principal add = %d %s", w.Code, w.Body.String())
	}
	if rec := stub.recs["fp-1"]; rec.Principal != "reader" {
		t.Fatalf("record principal = %q", rec.Principal)
	}
	_ = store.ErrKindNotFound // keep the store import if unused elsewhere
}
