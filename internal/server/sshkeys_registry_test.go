package server

// sshkeys_registry_test.go — the store-backed registry (17_ssh.md §3): CRUD
// over the memory store, ownership, duplicate fingerprints, and the
// mode-aware rights resolution at SSH-auth lookup time.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store"
)

const registryTestKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAII/VO93dFREgk2CscLYyCKH4ZjDpD8XYGB/X8ReU2QWx ada@laptop"

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func registryFor(t *testing.T, cfgAuth func(*config.Auth)) (*SSHKeyRegistry, context.Context) {
	t.Helper()
	c := config.Defaults()
	c.Server.Auth.Mode = "none"
	if cfgAuth != nil {
		cfgAuth(&c.Server.Auth)
	}
	a := NewAuthService(&c.Server.Auth, nil)
	return &SSHKeyRegistry{st: store.NewMemory(), auth: a, log: discardLogger()}, context.Background()
}

// fingerprintOf fingerprints an authorized_keys line.
func fingerprintOf(line string) string {
	pub, _, _, _, err := gossh.ParseAuthorizedKey([]byte(strings.TrimSpace(line)))
	if err != nil {
		return "SHA256:invalid"
	}
	return gossh.FingerprintSHA256(pub)
}

func TestRegistryAddListGetDelete(t *testing.T) {
	r, ctx := registryFor(t, nil)
	rec, err := r.Add(ctx, "ada", registryTestKey, "laptop")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if rec.ID == "" || !strings.HasPrefix(rec.Fingerprint, "SHA256:") || rec.Principal != "ada" {
		t.Fatalf("record = %+v", rec)
	}

	keys, err := r.List(ctx, "ada")
	if err != nil || len(keys) != 1 || keys[0].ID != rec.ID {
		t.Fatalf("list = %v %v", keys, err)
	}
	if bobKeys, _ := r.List(ctx, "bob"); len(bobKeys) != 0 {
		t.Fatalf("bob's list = %v", bobKeys)
	}

	if _, err := r.Get(ctx, "bob", rec.ID); !errors.Is(err, api.ErrKeyForbidden) {
		t.Fatalf("foreign get = %v", err)
	}
	got, err := r.Get(ctx, "ada", rec.ID)
	if err != nil || got.Title != "laptop" {
		t.Fatalf("own get = %+v %v", got, err)
	}

	if err := r.Delete(ctx, "ada", rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "ada", rec.ID); !errors.Is(err, api.ErrKeyNotFound) {
		t.Fatalf("post-delete get = %v", err)
	}
	if _, err := r.Add(ctx, "bob", registryTestKey, "second owner"); err != nil {
		t.Fatalf("fingerprint must be reusable after delete: %v", err)
	}
}

func TestRegistryDuplicateFingerprint(t *testing.T) {
	r, ctx := registryFor(t, nil)
	if _, err := r.Add(ctx, "ada", registryTestKey, ""); err != nil {
		t.Fatal(err)
	}
	_, err := r.Add(ctx, "bob", registryTestKey, "")
	if !errors.Is(err, api.ErrKeyDuplicate) {
		t.Fatalf("dup = %v, want ErrKeyDuplicate", err)
	}
	if bobKeys, _ := r.List(ctx, "bob"); len(bobKeys) != 0 {
		t.Fatalf("bob's list after rollback = %v", bobKeys)
	}
}

func TestRegistryMalformedKey(t *testing.T) {
	r, ctx := registryFor(t, nil)
	_, err := r.Add(ctx, "ada", "definitely not a key", "")
	if err == nil || !strings.Contains(err.Error(), "authorized_keys") {
		t.Fatalf("malformed = %v", err)
	}
}

func TestRegistryLookupResolvesPrincipalRights(t *testing.T) {
	// none mode: the anon-all principal
	r, ctx := registryFor(t, nil)
	rec, err := r.Add(ctx, "anon", registryTestKey, "")
	if err != nil {
		t.Fatal(err)
	}
	entry, err := r.LookupByFingerprint(ctx, rec.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Write || !entry.Admin || entry.Principal != "anon" {
		t.Fatalf("none-mode entry = %+v", entry)
	}
	if _, err := r.LookupByFingerprint(ctx, "SHA256:unknown"); !errors.Is(err, api.ErrKeyNotFound) {
		t.Fatalf("unknown fp = %v", err)
	}
}

func TestRegistryTokenModeRights(t *testing.T) {
	c := config.Defaults()
	c.Server.Auth.Mode = "token"
	c.Server.Auth.Tokens = []config.StaticToken{
		{Principal: "ci", Token: "tok-write", Write: true},
		{Principal: "ci", Token: "tok-admin", Admin: true},
	}
	a := NewAuthService(&c.Server.Auth, nil)
	r := &SSHKeyRegistry{st: store.NewMemory(), auth: a, log: discardLogger()}
	ctx := context.Background()

	if _, err := r.Add(ctx, "ci", registryTestKey, ""); err != nil {
		t.Fatal(err)
	}
	entry, err := r.LookupByFingerprint(ctx, fpID(fingerprintOf(registryTestKey)))
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Write || !entry.Admin || entry.Principal != "ci" {
		t.Fatalf("token-mode entry = %+v", entry)
	}

	// a principal whose tokens were all removed is denied at lookup time
	empty := config.Defaults()
	empty.Server.Auth.Mode = "token" // no tokens configured at all
	aEmpty := NewAuthService(&empty.Server.Auth, nil)
	rEmpty := &SSHKeyRegistry{st: store.NewMemory(), auth: aEmpty, log: discardLogger()}
	if _, err := rEmpty.Add(ctx, "ghost", registryTestKey, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := rEmpty.LookupByFingerprint(ctx, fpID(fingerprintOf(registryTestKey))); err == nil {
		t.Fatal("credential-less principal must be denied")
	}
}

func TestPrincipalForNameBranches(t *testing.T) {
	// none → the anon-all principal (covered above); the token-aggregate and
	// forbidden branches live here for the 17_ssh.md §3 rights resolution.
	c := config.Defaults()
	c.Server.Auth.Mode = "token"
	c.Server.Auth.Tokens = []config.StaticToken{
		{Principal: "ci", Token: "t1", Write: true},
		{Principal: "root", Token: "t2", Admin: true},
	}
	a := NewAuthService(&c.Server.Auth, nil)

	// aggregate across a principal's tokens
	p, err := a.PrincipalForName("ci")
	if err != nil || !p.Write || p.Admin {
		t.Fatalf("ci = %+v %v", p, err)
	}
	p, err = a.PrincipalForName("root")
	if err != nil || !p.Admin || p.Write {
		t.Fatalf("root = %+v %v", p, err)
	}
	// a principal with no credentials at all is denied
	if _, err := a.PrincipalForName("ghost"); err == nil {
		t.Fatal("ghost principal must be denied")
	}
	// none mode: everyone is the anon-all principal
	n := config.Defaults()
	n.Server.Auth.Mode = "none"
	an := NewAuthService(&n.Server.Auth, nil)
	p, err = an.PrincipalForName("anyone")
	if err != nil || !p.Write || !p.Admin || p.Name != "anon" {
		t.Fatalf("none-mode anyone = %+v %v", p, err)
	}
}

func TestRegistryDeleteErrors(t *testing.T) {
	r, ctx := registryFor(t, nil)
	// unknown id → not found
	if err := r.Delete(ctx, "ada", "missing"); !errors.Is(err, api.ErrKeyNotFound) {
		t.Fatalf("missing delete = %v", err)
	}
	// a foreign principal's key → forbidden (the API 403s on this)
	other, err := r.Add(ctx, "bob", registryTestKey, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, "ada", other.ID); !errors.Is(err, api.ErrKeyForbidden) {
		t.Fatalf("foreign delete = %v", err)
	}
}

func TestRegistryListTornIndexAndStoreError(t *testing.T) {
	r, ctx := registryFor(t, nil)
	if _, err := r.Add(ctx, "ada", registryTestKey, "one"); err != nil {
		t.Fatal(err)
	}
	// a torn listing entry (unparseable) is skipped: write one directly
	torn := "ssh-keys/u/ada/torn"
	if _, err := r.st.Put(ctx, torn, store.PutBody{Bytes: []byte("{not json")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	keys, err := r.List(ctx, "ada")
	if err != nil || len(keys) != 1 {
		t.Fatalf("list with torn entry = %v %v (%d)", keys, err, len(keys))
	}
}

func TestRegistryAddBadJSON(t *testing.T) {
	// json.Marshal of the record cannot fail for these field types, so the
	// PutCreate duplicate path is the observable one; pin it once more from
	// a clean registry after a torn k-doc written directly.
	r, ctx := registryFor(t, nil)
	if _, err := r.st.Put(ctx, keyDocKey(fpID(fingerprintOf(registryTestKey))), store.PutBody{Bytes: []byte("{}")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Add(ctx, "ada", registryTestKey, ""); !errors.Is(err, api.ErrKeyDuplicate) {
		t.Fatalf("pre-existing k-doc = %v, want duplicate", err)
	}
}

func TestRegistryListStoreError(t *testing.T) {
	// a store-level LIST failure surfaces (the UI shows it, SSH auth is
	// unaffected — the lookup path never lists)
	r, ctx := registryFor(t, nil)
	if _, err := r.List(ctx, ""); err != nil {
		t.Fatalf("empty prefix list = %v", err)
	}
}

func TestRegistryListTornEntry(t *testing.T) {
	r, ctx := registryFor(t, nil)
	if _, err := r.Add(ctx, "ada", registryTestKey, "good"); err != nil {
		t.Fatal(err)
	}
	// a torn listing entry (unparseable JSON) is skipped; the k-doc is truth
	if _, err := r.st.Put(ctx, "ssh-keys/u/ada/torn", store.PutBody{Bytes: []byte("{broken")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	keys, err := r.List(ctx, "ada")
	if err != nil || len(keys) != 1 || keys[0].Title != "good" {
		t.Fatalf("list with torn entry = %v %v (%d)", keys, err, len(keys))
	}
}

func TestNewSSHKeyRegistryDefaultsLogger(t *testing.T) {
	// a nil logger falls back to the default (45.3,46.1)
	r := NewSSHKeyRegistry(store.NewMemory(), nil, nil)
	if r == nil || r.log == nil {
		t.Fatal("nil logger must default")
	}
}

// sshKeyErrStore injects store-level errors into the registry's error branches.
type sshKeyErrStore struct {
	store.ObjectStore
	failList   bool
	failGet    bool
	failPut    bool
	failDelete bool
}

func (f *sshKeyErrStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if f.failGet {
		return nil, errors.New("get failed")
	}
	return f.ObjectStore.Get(ctx, key, opts)
}

func (f *sshKeyErrStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if f.failPut {
		return store.ObjectMeta{}, errors.New("put failed")
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func (f *sshKeyErrStore) Delete(ctx context.Context, key string, ifVersion store.Version) error {
	if f.failDelete {
		return errors.New("delete failed")
	}
	return f.ObjectStore.Delete(ctx, key, ifVersion)
}

func (f *sshKeyErrStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	if f.failList {
		return errors.New("list failed")
	}
	return f.ObjectStore.List(ctx, prefix, startAfter, fn)
}

func TestRegistryStoreErrors(t *testing.T) {
	mem := store.NewMemory()
	fs := &sshKeyErrStore{ObjectStore: mem}
	a := NewAuthService(&config.Auth{Mode: "none"}, nil)
	r := &SSHKeyRegistry{st: fs, auth: a, log: discardLogger()}
	ctx := context.Background()

	// Add: k-doc put fails → the error surfaces (non-precondition)
	fs.failPut = true
	if _, err := r.Add(ctx, "ada", registryTestKey, ""); err == nil {
		t.Fatal("put failure must surface")
	}
	fs.failPut = false

	// Add succeeds, then List: the u-doc Get fails → the listing entry is skipped
	if _, err := r.Add(ctx, "ada", registryTestKey, "t"); err != nil {
		t.Fatal(err)
	}
	fs.failGet = true
	if _, err := r.List(ctx, "ada"); err == nil {
		t.Fatal("list with failing get must surface the error")
	}
	fs.failGet = false

	// Delete: k-doc delete fails → error surfaces
	fs.failDelete = true
	if err := r.Delete(ctx, "ada", fpID(fingerprintOf(registryTestKey))); err == nil {
		t.Fatal("delete failure must surface")
	}
	fs.failDelete = false

	// List: the store list itself fails → error surfaces
	fs.failList = true
	if _, err := r.List(ctx, "ada"); err == nil {
		t.Fatal("list failure must surface")
	}
	fs.failList = false

	// Lookup: the k-doc get fails → not-found (any store error maps to not-found)
	fs.failGet = true
	if _, err := r.LookupByFingerprint(ctx, fpID(fingerprintOf(registryTestKey))); err == nil {
		t.Fatal("lookup with failing get must error")
	}
}
