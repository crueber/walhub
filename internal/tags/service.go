package tags

import (
	"context"
	"fmt"
	"strings"

	walgit "git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/policy"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// This file owns mutation policy: the P6 write gate, tag-name validation,
// the annotated 422, the policy.json create check, sha resolution, and the
// CAS create publish. Wire mapping lives in http.go.

// RoleService is the narrow P6 surface this package consumes (same shape as
// internal/releases and internal/pulls: satisfied by *identity.Service;
// tests substitute a fake).
type RoleService interface {
	// Resolve returns the max repo role for p (P6 verbatim).
	Resolve(ctx context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc)
	// CheckRead is the require_read gate (unused here; create is write-only).
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

// GitRunner verifies a sha names a commit (exact argv in git.go; never a Go
// git library). Tests substitute a fake.
type GitRunner interface {
	// CommitExists resolves sha to its commit id (peeled `^{commit}`).
	// Unresolvable ⇒ ErrNotFound-class error (→ 404 unknown revision).
	CommitExists(ctx context.Context, dir, sha string) (string, error)
}

// RepoDirs resolves a repo to its synced local git dir (production syncs
// the WAL handle to serve level and returns the bare repo path).
type RepoDirs interface {
	Dir(ctx context.Context, repo string) (string, error)
}

// RefPublisher publishes the tag create through the normal WAL publish path
// (doc 05 CAS ladder). The production adapter funnels through
// RepoHandle.Publish with a single-update REF_UPDATE txn (old-oid zero =
// create); a present tag fails in verify and surfaces as an
// ErrConflict-class error (→ 409), never a force.
type RefPublisher interface {
	// CreateTag creates refs/tags/<name> → sha (CAS create).
	CreateTag(ctx context.Context, repo, name, sha string, meta map[string]string) error
}

// Service is the tags store client: validation, policy, and the publish
// funnel. Construct with New; Git/Dirs/Refs may be nil in tests that
// exercise pure-validation paths (nil Git/Dirs makes sha-touching ops 503;
// nil Refs makes publishing 503; nil Roles falls back to principal flags).
type Service struct {
	Store store.ObjectStore
	Roles RoleService
	Git   GitRunner
	Dirs  RepoDirs
	Refs  RefPublisher
}

// New builds a Service over st.
func New(st store.ObjectStore, roles RoleService) *Service {
	return &Service{Store: st, Roles: roles}
}

// Tag is the create response (§253: the created lightweight tag).
type Tag struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
	Ref  string `json:"ref"`
}

// CreateInput is the POST body: name + sha required; a non-empty message
// requests an annotated tag (422 in v1 — lightweight-only).
type CreateInput struct {
	Name    string `json:"name"`
	SHA     string `json:"sha"`
	Message string `json:"message"`
}

// PolicyKey renders repos/<o>/<r>/policy.json (owned by internal/policy;
// this package reads it for the create check only).
func PolicyKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/policy.json"
}

// CreateTag validates and publishes refs/tags/<name> → sha as one WAL
// REF_UPDATE create. Error classes: 401 anonymous, 403 non-writer or
// policy-denied, 400 bad name/sha, 404 unknown sha, 409 tag exists, 422
// annotated request, 503 git/publish unwired.
func (s *Service) CreateTag(ctx context.Context, owner, repo string, p auth.Principal, in CreateInput) (*Tag, error) {
	if err := s.requireRole(ctx, owner, repo, p, string(identity.RoleWrite)); err != nil {
		return nil, err
	}
	name, err := validateTagName(in.Name)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Message) != "" {
		return nil, fmt.Errorf("%w: annotated tags are not yet supported; create a lightweight tag (omit message)", ErrUnsupported)
	}
	sha := strings.TrimSpace(in.SHA)
	if sha == "" {
		return nil, fmt.Errorf("%w: sha must not be empty", ErrInvalid)
	}
	resolved, err := s.resolveCommit(ctx, owner, repo, sha)
	if err != nil {
		return nil, err
	}
	if err := s.checkPolicy(ctx, owner, repo, p, "refs/tags/"+name); err != nil {
		return nil, err
	}
	meta := map[string]string{"principal": p.Name}
	if s.Refs == nil {
		return nil, fmt.Errorf("%w: tag publish unavailable", ErrUnavailable)
	}
	if perr := s.Refs.CreateTag(ctx, repoName(owner, repo), name, resolved, meta); perr != nil {
		if isErr(perr, errConflict) {
			return nil, fmt.Errorf("%w: tag %q already exists", ErrConflict, name)
		}
		return nil, perr
	}
	return &Tag{Name: name, SHA: resolved, Ref: "refs/tags/" + name}, nil
}

// validateTagName enforces the git refname rules pushes enforce, applied to
// the full refs/tags/<name> form: no ~^:?*[ no whitespace/control bytes,
// no "..", no "@{", no leading/trailing slashes. Slashes inside the name
// are legal (refs/tags/a/b); the rule names itself in the error.
func validateTagName(name string) (string, error) {
	t := strings.TrimSpace(name)
	if t == "" {
		return "", fmt.Errorf("%w: tag must not be empty", ErrInvalid)
	}
	if len(t) > MaxTagLen {
		return "", fmt.Errorf("%w: tag exceeds %d bytes", ErrInvalid, MaxTagLen)
	}
	if err := walgit.ValidateRefName("refs/tags/" + t); err != nil {
		return "", fmt.Errorf("%w: invalid ref name %q: %v", ErrInvalid, t, err)
	}
	return t, nil
}

// resolveCommit verifies sha names a commit in the repo. Unwired git ⇒ 503;
// unresolvable ⇒ 404 unknown revision.
func (s *Service) resolveCommit(ctx context.Context, owner, repo, sha string) (string, error) {
	if s.Git == nil || s.Dirs == nil {
		return "", fmt.Errorf("%w: commit resolution unavailable", ErrUnavailable)
	}
	dir, err := s.Dirs.Dir(ctx, repoName(owner, repo))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	resolved, err := s.Git.CommitExists(ctx, dir, sha)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// checkPolicy evaluates policy.json for (principal, ref, create) because the
// tag publish is server-side, NOT a receive-pack push (the merge-task
// precedent): protect rules deny the create exactly as they would deny a
// push (`rejected by rule '<name>'`). Missing policy.json is allow-all; an
// unparseable file fails closed (refused, never "skip policy").
func (s *Service) checkPolicy(ctx context.Context, owner, repo string, p auth.Principal, ref string) error {
	raw, _, err := store.GetBytes(ctx, s.Store, PolicyKey(owner, repo), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			raw = nil
		} else {
			return err
		}
	}
	var doc *policy.Document
	if raw == nil {
		doc, err = policy.Parse([]byte(`{"version":1,"rules":[]}`))
	} else {
		doc, err = policy.Parse(raw)
	}
	if err != nil {
		return fmt.Errorf("%w: policy.json unparseable: %v", ErrCorrupt, err)
	}
	v := policy.EvaluateProtect(ctx, doc, policy.Request{Principal: p.Name, Ref: ref, Op: policy.OpCreate})
	if !v.Allow {
		return fmt.Errorf("%w: rejected by rule '%s'", ErrForbidden, v.Rule)
	}
	return nil
}

// repoName renders "owner/repo".
func repoName(owner, repo string) string { return owner + "/" + repo }

// --- role helpers (P6; same shape as internal/releases) ----------------------

// roleRank orders role names on the P6 ladder read < triage < write <
// maintain < admin.
func roleRank(role string) int {
	switch identity.Role(strings.ToLower(role)) {
	case identity.RoleRead:
		return 1
	case identity.RoleTriage:
		return 2
	case identity.RoleWrite:
		return 3
	case identity.RoleMaintain:
		return 4
	case identity.RoleAdmin:
		return 5
	}
	return 0
}

// roleOf resolves the actor's repo role ("" when none). Host admin/write
// flags short-circuit through identity's own resolution.
func (s *Service) roleOf(ctx context.Context, owner, repo string, p auth.Principal) string {
	if s.Roles == nil {
		if p.Admin {
			return string(identity.RoleAdmin)
		}
		if p.Write {
			return string(identity.RoleWrite)
		}
		if p.Anonymous {
			return ""
		}
		return string(identity.RoleRead)
	}
	role, _ := s.Roles.Resolve(ctx, owner, repo, p)
	return string(role)
}

// requireRole enforces a minimum repo role: host admin always passes;
// anonymous failures are 401, authenticated-but-insufficient are 403.
func (s *Service) requireRole(ctx context.Context, owner, repo string, p auth.Principal, want string) error {
	if p.Admin {
		return nil
	}
	got := s.roleOf(ctx, owner, repo, p)
	if roleRank(got) >= roleRank(want) {
		return nil
	}
	if p.Anonymous {
		return fmt.Errorf("%w", ErrUnauthorized)
	}
	return fmt.Errorf("%w: need %s", ErrForbidden, want)
}
