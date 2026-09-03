package server

// sshkeys.go — the store-backed SSH public-key registry (17_ssh.md §3).
// Keys belong to principals and are managed through the UI/API; the bucket
// is the only durable state (Law 4). The TOML surface is the server only:
// listener + host key.
//
// Layout (two objects per key, one truth):
//
//	ssh-keys/k/<fp>                  the key record — auth lookup: 1 GET by
//	                                 fingerprint (no LIST on the SSH hot path)
//	ssh-keys/u/<principal>/<fp>      the per-principal listing entry (UI)
//
// <fp> is the SHA256 fingerprint with path-unsafe characters folded
// (`+`→`-`, `/`→`_`), so the SSH-auth GET is a single flat key.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/sshd"
	"git.packden.us/crueber/walhub/internal/store"
	gossh "golang.org/x/crypto/ssh"
)

// SSHKeyRegistry is the store-backed registry. One instance per process; safe
// for concurrent use (every method is a store round trip with CAS semantics).
type SSHKeyRegistry struct {
	st   store.ObjectStore
	auth *AuthService
	log  *slog.Logger
}

// NewSSHKeyRegistry builds a registry over the given store and auth service
// (the auth service resolves a key's principal rights at lookup time,
// mode-aware — see PrincipalForName).
func NewSSHKeyRegistry(st store.ObjectStore, auth *AuthService, log *slog.Logger) *SSHKeyRegistry {
	if log == nil {
		log = slog.Default()
	}
	return &SSHKeyRegistry{st: st, auth: auth, log: log}
}

// fpID folds a fingerprint into a path-safe object id.
func fpID(fingerprint string) string {
	r := strings.NewReplacer("SHA256:", "", "+", "-", "/", "_", " ", "")
	return r.Replace(fingerprint)
}

func keyDocKey(fpID string) string             { return "ssh-keys/k/" + fpID }
func userDocKey(principal, fpID string) string { return "ssh-keys/u/" + principal + "/" + fpID }

// LookupByFingerprint resolves one key at SSH-auth time: a single GET (Law 4:
// no LIST on the hot path) followed by the principal's rights resolution
// (mode-aware: none → anon-all; oidc → admission lists; token → token flags).
// Unknown fingerprints surface api.ErrKeyNotFound; the sshd auth callback
// turns that into a clean permission-denied.
func (r *SSHKeyRegistry) LookupByFingerprint(ctx context.Context, fingerprint string) (sshd.KeyEntry, error) {
	rec, err := r.get(ctx, fpID(fingerprint))
	if err != nil {
		return sshd.KeyEntry{}, err
	}
	p, err := r.auth.PrincipalForName(rec.Principal)
	if err != nil {
		r.log.Warn("ssh key principal denied", "principal", rec.Principal, "err", err)
		return sshd.KeyEntry{}, err
	}
	return sshd.KeyEntry{Principal: p.Name, Write: p.Write, Admin: p.Admin}, nil
}

// List returns the calling principal's keys, newest first.
func (r *SSHKeyRegistry) List(ctx context.Context, principal string) ([]api.SshKeyRecord, error) {
	prefix := "ssh-keys/u/" + principal + "/"
	var out []api.SshKeyRecord
	err := r.st.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		res, err := r.st.Get(ctx, m.Key, store.GetOptions{})
		if err != nil {
			return err
		}
		obj, ok := res.(store.Object)
		if !ok {
			return nil
		}
		defer obj.Body.Close()
		var rec api.SshKeyRecord
		if err := json.NewDecoder(obj.Body).Decode(&rec); err != nil {
			return nil // a torn index entry: skip, the k-doc is the truth
		}
		out = append(out, rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Get returns one of the principal's keys by id (ownership checked).
func (r *SSHKeyRegistry) Get(ctx context.Context, principal, id string) (api.SshKeyRecord, error) {
	rec, err := r.get(ctx, id)
	if err != nil {
		return api.SshKeyRecord{}, err
	}
	if rec.Principal != principal {
		return api.SshKeyRecord{}, api.ErrKeyForbidden
	}
	return rec, nil
}

// Add parses and registers a new key for the principal. Duplicate
// fingerprints are refused (the fingerprint is the identity, 409 at the API).
func (r *SSHKeyRegistry) Add(ctx context.Context, principal, keyLine, title string) (api.SshKeyRecord, error) {
	pub, _, _, _, err := gossh.ParseAuthorizedKey([]byte(strings.TrimSpace(keyLine)))
	if err != nil {
		return api.SshKeyRecord{}, fmt.Errorf("%w: %v", api.ErrKeyBadKeyLine, err)
	}
	fp := gossh.FingerprintSHA256(pub)
	id := fpID(fp)
	rec := api.SshKeyRecord{
		Principal:   principal,
		Fingerprint: fp,
		ID:          id,
		Key:         strings.TrimSpace(keyLine),
		Title:       strings.TrimSpace(title),
		CreatedAt:   time.Now().UTC(),
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return api.SshKeyRecord{}, err
	}
	if _, err := r.st.Put(ctx, keyDocKey(id), store.PutBody{Bytes: body},
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		if isPrecondition(err) {
			return api.SshKeyRecord{}, api.ErrKeyDuplicate
		}
		return api.SshKeyRecord{}, err
	}
	// the listing entry: a pure index of the k-doc, safe to overwrite
	if _, err := r.st.Put(ctx, userDocKey(principal, id), store.PutBody{Bytes: body},
		store.PutOptions{Mode: store.PutOverwrite, ContentType: "application/json"}); err != nil {
		// roll the k-doc back so a failed add leaves nothing behind
		_ = r.st.Delete(ctx, keyDocKey(id), "")
		return api.SshKeyRecord{}, err
	}
	return rec, nil
}

// Delete removes the principal's key (both objects). Deleting another
// principal's key is refused here — the API checks ownership via Get.
func (r *SSHKeyRegistry) Delete(ctx context.Context, principal, id string) error {
	if _, err := r.Get(ctx, principal, id); err != nil {
		return err
	}
	if err := r.st.Delete(ctx, keyDocKey(id), ""); err != nil {
		return err
	}
	return r.st.Delete(ctx, userDocKey(principal, id), "")
}

// get loads one key record by id.
func (r *SSHKeyRegistry) get(ctx context.Context, id string) (api.SshKeyRecord, error) {
	res, err := r.st.Get(ctx, keyDocKey(id), store.GetOptions{})
	if err != nil {
		if isNotFoundStore(err) {
			return api.SshKeyRecord{}, api.ErrKeyNotFound
		}
		return api.SshKeyRecord{}, err
	}
	obj, ok := res.(store.Object)
	if !ok {
		return api.SshKeyRecord{}, api.ErrKeyNotFound
	}
	defer obj.Body.Close()
	var rec api.SshKeyRecord
	if err := json.NewDecoder(obj.Body).Decode(&rec); err != nil {
		return api.SshKeyRecord{}, err
	}
	return rec, nil
}

// isPrecondition maps the store's 412-style failure onto ErrKeyDuplicate.
func isPrecondition(err error) bool {
	var se *store.StoreError
	return errors.As(err, &se) && se.Kind == store.ErrKindPreconditionFailed
}

// isNotFoundStore maps store misses onto the registry sentinel.
func isNotFoundStore(err error) bool {
	var se *store.StoreError
	return errors.As(err, &se) && se.Kind == store.ErrKindNotFound
}
