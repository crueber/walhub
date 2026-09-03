package api

// sshkeys.go — the /api/v1/ssh-keys surface (17_ssh.md §3): authenticated
// users manage their own SSH public keys; auth "none" manages the anon
// principal's keys. The registry (internal/server) owns the store layout.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Sentinel errors the registry returns; the handlers map them onto statuses.
var (
	ErrKeyNotFound     = errors.New("ssh key not found")
	ErrKeyDuplicate    = errors.New("ssh key already registered")
	ErrKeyForbidden    = errors.New("ssh key belongs to another principal")
	ErrKeyBadKeyLine   = errors.New("not a valid authorized_keys line")
	ErrKeyBadPrincipal = errors.New("invalid principal name")
)

// SSHKeyStore is the consumer-side seam the server implements (14_extensibility.md):
// one interface, three operations, ownership enforced by the implementation.
type SSHKeyStore interface {
	List(ctx context.Context, principal string) ([]SshKeyRecord, error)
	Add(ctx context.Context, principal, key, title string) (SshKeyRecord, error)
	Get(ctx context.Context, principal, id string) (SshKeyRecord, error)
	Delete(ctx context.Context, principal, id string) error
}

// SshKeyRecord is one registered public key as the API returns it.
type SshKeyRecord struct {
	Principal   string    `json:"principal"`
	Fingerprint string    `json:"fingerprint"`
	ID          string    `json:"id"` // path-safe fingerprint: use for DELETE
	Key         string    `json:"key"`
	Title       string    `json:"title,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// sshKeysNotConfigured — the registry is nil only when the caller skipped it.
func (h *handlers) sshKeysNotConfigured(w http.ResponseWriter) {
	writePlain(w, http.StatusServiceUnavailable, "ssh key registry not configured")
}

// GET /api/v1/ssh-keys — the calling principal's keys.
func (h *handlers) sshKeysList(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthRead) {
		return
	}
	if h.env.SSHKeys == nil {
		h.sshKeysNotConfigured(w)
		return
	}
	p := h.env.PrincipalOf(r)
	keys, err := h.env.SSHKeys.List(r.Context(), p.Name)
	if err != nil {
		writePlain(w, http.StatusInternalServerError, err.Error())
		return
	}
	if keys == nil {
		keys = []SshKeyRecord{}
	}
	writeCached(w, r, ccNoStore, "", http.StatusOK, keys)
}

// POST /api/v1/ssh-keys — register a public key for the calling principal.
func (h *handlers) sshKeysAdd(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthRead) {
		return
	}
	if h.env.SSHKeys == nil {
		h.sshKeysNotConfigured(w)
		return
	}
	var req struct {
		Key   string `json:"key"`
		Title string `json:"title"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writePlain(w, http.StatusBadRequest, "body must be JSON {key, title}")
		return
	}
	if strings.TrimSpace(req.Key) == "" {
		writePlain(w, http.StatusBadRequest, "key is required (an authorized_keys line)")
		return
	}
	p := h.env.PrincipalOf(r)
	rec, err := h.env.SSHKeys.Add(r.Context(), p.Name, req.Key, req.Title)
	if err != nil {
		switch {
		case errors.Is(err, ErrKeyDuplicate):
			writePlain(w, http.StatusConflict, err.Error())
		case errors.Is(err, ErrKeyBadKeyLine), errors.Is(err, ErrKeyBadPrincipal):
			writePlain(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeCached(w, r, ccNoStore, "", http.StatusCreated, rec)
}

// DELETE /api/v1/ssh-keys/{id} — remove one of the calling principal's keys.
func (h *handlers) sshKeysDelete(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthRead) {
		return
	}
	if h.env.SSHKeys == nil {
		h.sshKeysNotConfigured(w)
		return
	}
	id := r.PathValue("fp")
	if strings.TrimSpace(id) == "" {
		writePlain(w, http.StatusBadRequest, "key id is required")
		return
	}
	p := h.env.PrincipalOf(r)
	if err := h.env.SSHKeys.Delete(r.Context(), p.Name, id); err != nil {
		switch {
		case errors.Is(err, ErrKeyNotFound):
			writePlain(w, http.StatusNotFound, "no such key")
		case errors.Is(err, ErrKeyForbidden):
			writePlain(w, http.StatusForbidden, "that key belongs to another principal")
		default:
			writePlain(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeCached(w, r, ccNoStore, "", http.StatusOK, map[string]string{"deleted": id})
}

// isKeyFormatError — a malformed authorized_keys line is a client mistake.
func isKeyFormatError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "authorized_keys line")
}
