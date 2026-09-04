package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// Invitation kinds: org (join with owner|member) and repo (collaborator
// with a repo role).
const (
	InviteOrg  = "org"
	InviteRepo = "repo"
)

// Invitation is a Create-only immutable capability token. State transitions
// are delete-on-transition: the issuer-side object is DELETED on terminal
// state and acceptance evidence is the resulting binding, not the object.
type Invitation struct {
	Version   int    `json:"version"`
	ID        string `json:"id"`
	Token     string `json:"token"`
	Kind      string `json:"kind"`
	Org       string `json:"org"`
	Repo      string `json:"repo,omitempty"`
	Role      string `json:"role"`
	Subject   string `json:"subject"`
	InvitedBy string `json:"invited_by"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

// InboxEntry is one row of the pending-invites index.
type InboxEntry struct {
	ID        string `json:"id"`
	Org       string `json:"org,omitempty"`
	Repo      string `json:"repo,omitempty"`
	Role      string `json:"role"`
	InvitedBy string `json:"invited_by"`
	CreatedAt string `json:"created_at"`
}

// Inbox is users/<principal>/invitations/index.json: the P4-style hot
// window of pending invites.
type Inbox struct {
	Version   int          `json:"version"`
	Entries   []InboxEntry `json:"entries"`
	UpdatedAt string       `json:"updated_at"`
}

func encodeInvite(inv *Invitation) []byte {
	raw, _ := json.Marshal(inv)
	return raw
}

func parseInvite(raw []byte) (*Invitation, error) {
	var inv Invitation
	if err := json.Unmarshal(raw, &inv); err != nil {
		return nil, fmt.Errorf("%w: invitation: %v", ErrInvalid, err)
	}
	return &inv, nil
}

func encodeInbox(in *Inbox) []byte {
	raw, _ := json.Marshal(in)
	return raw
}

func parseInbox(raw []byte) (*Inbox, error) {
	var in Inbox
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("%w: inbox: %v", ErrInvalid, err)
	}
	if in.Entries == nil {
		in.Entries = []InboxEntry{}
	}
	return &in, nil
}

// inviteKeys returns the issuer-side key for an invite.
func inviteKeys(inv *Invitation) string {
	if inv.Kind == InviteOrg {
		return OrgInviteKey(inv.Org, inv.ID)
	}
	org, repo, _ := strings.Cut(inv.Repo, "/")
	return RepoInviteKey(org, repo, inv.ID)
}

// CreateOrgInvite records an org invitation (owner-only, checked by the
// handler). Returns id + accept URL path.
func (s *Service) CreateOrgInvite(ctx context.Context, org, email, role, invitedBy string, ttl time.Duration) (*Invitation, error) {
	email = normPrincipal(email)
	if !ValidPrincipal(email) {
		return nil, fmt.Errorf("%w: invalid email %q", ErrInvalid, email)
	}
	if !validOrgRole(role) {
		return nil, fmt.Errorf("%w: invalid org role %q", ErrInvalid, role)
	}
	if m, _, err := s.getMembers(ctx, org); err != nil {
		return nil, err
	} else if m == nil {
		return nil, fmt.Errorf("%w: unknown org %q", ErrNotFound, org)
	}
	id, err := s.randomHex(8)
	if err != nil {
		return nil, err
	}
	token, err := s.randomHex(16)
	if err != nil {
		return nil, err
	}
	now := s.nowUTC()
	inv := &Invitation{
		Version: 1, ID: id, Token: token, Kind: InviteOrg, Org: org,
		Role: role, Subject: email, InvitedBy: normPrincipal(invitedBy),
		State: "pending", CreatedAt: now.Format(time.RFC3339),
		ExpiresAt: now.Add(ttl).Format(time.RFC3339),
	}
	if _, err := store.PutBytes(ctx, s.Store, OrgInviteKey(org, id), encodeInvite(inv),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		return nil, err
	}
	if err := s.inboxAdd(ctx, email, InboxEntry{ID: id, Org: org, Role: role, InvitedBy: inv.InvitedBy, CreatedAt: inv.CreatedAt}); err != nil {
		return nil, err
	}
	return inv, nil
}

// CreateRepoInvite records a repo collaborator invitation (admin-only,
// checked by the handler).
func (s *Service) CreateRepoInvite(ctx context.Context, owner, repo, subject string, role Role, invitedBy string, ttl time.Duration) (*Invitation, error) {
	subject = normPrincipal(subject)
	if !ValidPrincipal(subject) {
		return nil, fmt.Errorf("%w: invalid subject %q", ErrInvalid, subject)
	}
	if !validRole(string(role)) {
		return nil, fmt.Errorf("%w: invalid role %q", ErrInvalid, string(role))
	}
	id, err := s.randomHex(8)
	if err != nil {
		return nil, err
	}
	token, err := s.randomHex(16)
	if err != nil {
		return nil, err
	}
	now := s.nowUTC()
	inv := &Invitation{
		Version: 1, ID: id, Token: token, Kind: InviteRepo, Org: owner,
		Repo: owner + "/" + repo, Role: string(role), Subject: subject,
		InvitedBy: normPrincipal(invitedBy), State: "pending",
		CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(ttl).Format(time.RFC3339),
	}
	if _, err := store.PutBytes(ctx, s.Store, RepoInviteKey(owner, repo, id), encodeInvite(inv),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		return nil, err
	}
	if err := s.inboxAdd(ctx, subject, InboxEntry{ID: id, Repo: owner + "/" + repo, Role: string(role), InvitedBy: inv.InvitedBy, CreatedAt: inv.CreatedAt}); err != nil {
		return nil, err
	}
	return inv, nil
}

// inboxAdd appends an entry to the invitee's inbox index (CAS; P8 fan-out
// shape — a crash here drops one inbox entry while the issuer-side object
// stays the truth).
func (s *Service) inboxAdd(ctx context.Context, principal string, e InboxEntry) error {
	_, err := s.casUpdate(ctx, InboxKey(principal), func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var in *Inbox
		if cur == nil {
			in = &Inbox{Version: 1, Entries: []InboxEntry{}}
		} else {
			var perr error
			in, perr = parseInbox(cur)
			if perr != nil {
				return nil, false, perr
			}
			in.Version++
		}
		for _, old := range in.Entries {
			if old.ID == e.ID {
				return nil, false, nil
			}
		}
		in.Entries = append(in.Entries, e)
		in.UpdatedAt = s.nowUTC().Format(time.RFC3339)
		return encodeInbox(in), true, nil
	})
	return err
}

// inboxRemove drops an entry from the inbox index (CAS).
func (s *Service) inboxRemove(ctx context.Context, principal, id string) error {
	_, err := s.casUpdate(ctx, InboxKey(principal), func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		in, perr := parseInbox(cur)
		if perr != nil {
			return nil, false, perr
		}
		kept := in.Entries[:0]
		for _, e := range in.Entries {
			if e.ID != id {
				kept = append(kept, e)
			}
		}
		in.Entries = kept
		in.Version++
		in.UpdatedAt = s.nowUTC().Format(time.RFC3339)
		return encodeInbox(in), true, nil
	})
	return err
}

// MyInvites returns the caller's pending invites (no-store).
func (s *Service) MyInvites(ctx context.Context, principal string) ([]InboxEntry, error) {
	raw, _, err := store.GetBytes(ctx, s.Store, InboxKey(principal), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return []InboxEntry{}, nil
		}
		return nil, err
	}
	in, perr := parseInbox(raw)
	if perr != nil {
		return nil, perr
	}
	return in.Entries, nil
}

// findInvite locates an invite by id across org and repo families. The
// invitee's inbox entry names the family (org or repo scope), so at most
// two exact-key probes run — never a LIST.
func (s *Service) findInvite(ctx context.Context, principal, id string) (*Invitation, error) {
	raw, _, err := store.GetBytes(ctx, s.Store, InboxKey(principal), store.GetOptions{})
	if err != nil && !store.IsNotFound(err) {
		return nil, err
	}
	var scopes []InboxEntry
	if err == nil {
		in, perr := parseInbox(raw)
		if perr != nil {
			return nil, perr
		}
		for _, e := range in.Entries {
			if e.ID == id {
				scopes = append(scopes, e)
			}
		}
	}
	if len(scopes) == 0 {
		return nil, fmt.Errorf("%w: invitation no longer pending", ErrConflict)
	}
	for _, e := range scopes {
		var keys []string
		if e.Org != "" {
			keys = append(keys, OrgInviteKey(e.Org, id))
		}
		if e.Repo != "" {
			if org, repo, ok := strings.Cut(e.Repo, "/"); ok {
				keys = append(keys, RepoInviteKey(org, repo, id))
			}
		}
		for _, k := range keys {
			raw, _, gerr := store.GetBytes(ctx, s.Store, k, store.GetOptions{})
			if gerr != nil {
				if store.IsNotFound(gerr) {
					continue
				}
				return nil, gerr
			}
			inv, perr := parseInvite(raw)
			if perr != nil {
				return nil, perr
			}
			if exp, _ := time.Parse(time.RFC3339, inv.ExpiresAt); !exp.IsZero() && s.nowUTC().After(exp) {
				return nil, fmt.Errorf("%w: invitation expired", ErrConflict)
			}
			return inv, nil
		}
	}
	return nil, fmt.Errorf("%w: invitation no longer pending", ErrConflict)
}

// PreviewInvite renders the signed-link preview for an authenticated
// caller: the invite subject OR the link token authorizes the preview
// (anonymous callers are rejected at the handler — they log in first); the
// binding write always follows the authed POST. The token is redacted.
func (s *Service) PreviewInvite(ctx context.Context, principal, id, token string) (*Invitation, error) {
	inv, err := s.findInvite(ctx, principal, id)
	if err != nil {
		// Token-preview path: the invitee may not be authenticated yet; fall
		// back to a direct probe is impossible without scope, so surface the
		// lookup failure. Callers pass the authed principal for the POST.
		return nil, err
	}
	if normPrincipal(inv.Subject) == normPrincipal(principal) {
		cpy := *inv
		cpy.Token = ""
		return &cpy, nil
	}
	if token != "" && token == inv.Token {
		cpy := *inv
		cpy.Token = ""
		return &cpy, nil
	}
	return nil, fmt.Errorf("%w: not your invitation", ErrForbidden)
}

// AcceptInvite writes the binding first (org members.json CAS / access.json
// CAS loop), then DELETEs the invite object — idempotent for the loser (a
// second accept sees absent → done). A cancelled-vs-accept race is decided
// by which CAS lands first; the loser gets 409.
func (s *Service) AcceptInvite(ctx context.Context, principal, id string) (string, error) {
	principal = normPrincipal(principal)
	inv, err := s.findInvite(ctx, principal, id)
	if err != nil {
		return "", err
	}
	if normPrincipal(inv.Subject) != principal {
		return "", fmt.Errorf("%w: not your invitation", ErrForbidden)
	}
	switch inv.Kind {
	case InviteOrg:
		if !validOrgRole(inv.Role) {
			return "", fmt.Errorf("%w: invalid org role %q", ErrInvalid, inv.Role)
		}
		if _, serr := s.SetMember(ctx, inv.Org, principal, OrgRole(inv.Role)); serr != nil {
			return "", serr
		}
	case InviteRepo:
		org, repo, ok := strings.Cut(inv.Repo, "/")
		if !ok {
			return "", fmt.Errorf("%w: bad invite repo %q", ErrInvalid, inv.Repo)
		}
		if !validRole(inv.Role) {
			return "", fmt.Errorf("%w: invalid role %q", ErrInvalid, inv.Role)
		}
		if _, _, aerr := s.GetAccess(ctx, org, repo); aerr != nil {
			return "", aerr
		}
		if berr := s.addBinding(ctx, org, repo, "user:"+principal, Role(inv.Role)); berr != nil {
			return "", berr
		}
	default:
		return "", fmt.Errorf("%w: unknown invite kind %q", ErrInvalid, inv.Kind)
	}
	_ = s.Store.Delete(ctx, inviteKeys(inv), "")
	_ = s.inboxRemove(ctx, principal, id)
	if _, err := s.EnsureProfile(ctx, principal); err != nil {
		return "", err
	}
	return inv.Kind, nil
}

// addBinding inserts one binding into access.json via the CAS loop.
func (s *Service) addBinding(ctx context.Context, owner, repo, subject string, role Role) error {
	var done bool
	_, err := s.casUpdate(ctx, AccessKey(owner, repo), func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var doc *AccessDoc
		if cur == nil {
			doc = SynthesizeDefault(owner)
			doc.RoleBindings = append(doc.RoleBindings, AccessBinding{Subject: subject, Role: role})
		} else {
			var perr error
			doc, perr = parseAccess(cur)
			if perr != nil {
				return nil, false, perr
			}
			for i := range doc.RoleBindings {
				if doc.RoleBindings[i].Subject == subject {
					if doc.RoleBindings[i].Role.atLeast(role) {
						done = true
						return nil, false, nil
					}
					doc.RoleBindings[i].Role = role
					done = true
					break
				}
			}
			if !done {
				doc.RoleBindings = append(doc.RoleBindings, AccessBinding{Subject: subject, Role: role})
			}
		}
		vis, bindings, verr := normalizeAccess(string(doc.Visibility), doc.RoleBindings)
		if verr != nil {
			return nil, false, verr
		}
		doc.Visibility = vis
		doc.RoleBindings = bindings
		doc.Version++
		doc.UpdatedAt = s.nowUTC().Format(time.RFC3339)
		done = true
		return encodeAccess(doc), true, nil
	})
	if err != nil {
		return err
	}
	s.access.invalidate(owner, repo)
	return nil
}

// CancelInvite deletes the invite object (invitee decline or issuer cancel)
// and drops the inbox entry. The caller authorizes: invitee or an
// org-owner/repo-admin (checked by the handler via canCancel).
func (s *Service) CancelInvite(ctx context.Context, principal, id string) (*Invitation, error) {
	inv, err := s.findInvite(ctx, principal, id)
	if err != nil {
		return nil, err
	}
	if derr := s.Store.Delete(ctx, inviteKeys(inv), ""); derr != nil && !store.IsNotFound(derr) {
		return nil, derr
	}
	_ = s.inboxRemove(ctx, normPrincipal(inv.Subject), id)
	return inv, nil
}

// ListRepoInvites returns pending repo invites (admin-only, checked by the
// handler) via LIST over the repo invitation prefix — a collaboration page,
// not a git hot path. n caps the page (P5; default 100, max 1000, like teams).
func (s *Service) ListRepoInvites(ctx context.Context, owner, repo string, n int) ([]*Invitation, error) {
	if n <= 0 || n > 1000 {
		n = 100
	}
	var out []*Invitation
	if err := s.Store.List(ctx, RepoInvitePrefix(owner, repo), "", func(m store.ObjectMeta) error {
		if len(out) >= n {
			return nil
		}
		raw, _, gerr := store.GetBytes(ctx, s.Store, m.Key, store.GetOptions{})
		if gerr != nil {
			if store.IsNotFound(gerr) {
				return nil
			}
			return gerr
		}
		inv, perr := parseInvite(raw)
		if perr != nil {
			return nil
		}
		out = append(out, inv)
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	if out == nil {
		out = []*Invitation{}
	}
	return out, nil
}

// ListOrgInvites returns pending org invites (owner-only, checked by the
// handler). n caps the page (P5; default 100, max 1000, like teams).
func (s *Service) ListOrgInvites(ctx context.Context, org string, n int) ([]*Invitation, error) {
	if n <= 0 || n > 1000 {
		n = 100
	}
	var out []*Invitation
	if err := s.Store.List(ctx, OrgInvitePrefix(org), "", func(m store.ObjectMeta) error {
		if len(out) >= n {
			return nil
		}
		raw, _, gerr := store.GetBytes(ctx, s.Store, m.Key, store.GetOptions{})
		if gerr != nil {
			if store.IsNotFound(gerr) {
				return nil
			}
			return gerr
		}
		inv, perr := parseInvite(raw)
		if perr != nil {
			return nil
		}
		out = append(out, inv)
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	if out == nil {
		out = []*Invitation{}
	}
	return out, nil
}
