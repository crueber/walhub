// Package identity implements docs/features/01_identity_permissions.md:
// users, orgs, teams, repo roles (access.json), invitations, and the
// require_read visibility gate. It registers Seam 1 routes (both lanes),
// the Seam 3 team:/role: group expansion, the Seam 5 access-bootstrap task
// kind, and the Seam 7 `walhub access get/put` CLI surface (in cmd/walhub).
//
// Bucket layout (all keys bucket-relative; the store prefix is applied by
// the store layer):
//
//	users/<principal>/profile.json            CAS'd user profile
//	users/<principal>/invitations/index.json  CAS'd inbox index (pending invites)
//	orgs/<org>/org.json                       CAS'd org profile (Create reserves the name)
//	orgs/<org>/members.json                   CAS'd roster [{principal, role, joined_at}]
//	orgs/<org>/teams/<slug>.json              CAS'd team (Create reserves the slug)
//	orgs/<org>/invitations/<id>.json          Create-only immutable invite
//	repos/<o>/<r>/access.json                 CAS'd role bindings + visibility
//	repos/<o>/<r>/meta/invitations/<id>.json  Create-only immutable invite
//
// ### Concurrency
//
// Hazard: two writers mutating one CAS'd object (members.json, access.json,
// a team, the inbox index) losing an update on blind PUT. Avoidance: every
// mutation is a CAS Update(version) loop with retry-on-412 re-read, bounded
// at 5 attempts, then 409 — the 13_concurrency.md §3 discipline. CAS is the
// lock; there is no lock object, no .lock sidecar, and no new in-process
// mutex beyond the read-cache guards below. Hazard: a role check racing a
// demotion on the push path. Avoidance: resolution reads access.json fresh
// per request (conditional GET, control-plane class); a request in flight
// completes under the bindings it started with — revocation latency is one
// in-flight request, staleness bounded by the request, never a TTL.
package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// Role is one rung of the P6 ladder.
type Role string

// Roles, ordered read < triage < write < maintain < admin.
const (
	RoleRead     Role = "read"
	RoleTriage   Role = "triage"
	RoleWrite    Role = "write"
	RoleMaintain Role = "maintain"
	RoleAdmin    Role = "admin"
)

// rank orders roles for comparison.
func (r Role) rank() int {
	switch r {
	case RoleRead:
		return 1
	case RoleTriage:
		return 2
	case RoleWrite:
		return 3
	case RoleMaintain:
		return 4
	case RoleAdmin:
		return 5
	}
	return 0
}

// validRole reports whether r is a repo role.
func validRole(r string) bool { return Role(r).rank() > 0 }

// atLeast reports whether r grants at least want.
func (r Role) atLeast(want Role) bool { return r.rank() >= want.rank() }

// OrgRole is an org roster role: owner or member.
type OrgRole string

// Org roster roles.
const (
	OrgOwner  OrgRole = "owner"
	OrgMember OrgRole = "member"
)

func validOrgRole(r string) bool { return r == string(OrgOwner) || r == string(OrgMember) }

// Visibility gates anonymous reads.
type Visibility string

// Visibilities.
const (
	VisibilityPublic  Visibility = "public"
	VisibilityPrivate Visibility = "private"
)

func validVisibility(v string) bool {
	return v == string(VisibilityPublic) || v == string(VisibilityPrivate)
}

var (
	orgRe  = regexp.MustCompile(`^[a-z0-9-]{1,39}$`)
	slugRe = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
)

// ValidOrg reports whether org is a legal org slug.
func ValidOrg(org string) bool { return orgRe.MatchString(org) }

// ValidSlug reports whether slug is a legal team slug.
func ValidSlug(slug string) bool { return slugRe.MatchString(slug) }

// ValidPrincipal reports whether p is a usable principal name (an email).
func ValidPrincipal(p string) bool {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" || len(p) > 254 || strings.Contains(p, "/") {
		return false
	}
	if _, err := mail.ParseAddress(p); err != nil {
		return false
	}
	return true
}

// normPrincipal lowercases and trims a principal name.
func normPrincipal(p string) string { return strings.ToLower(strings.TrimSpace(p)) }

// encodePrincipal percent-encodes the principal for one key segment (@ →
// %40), keeping the one-segment rule.
func encodePrincipal(p string) string {
	return strings.ReplaceAll(normPrincipal(p), "@", "%40")
}

// decodePrincipal reverses encodePrincipal.
func decodePrincipal(seg string) string {
	return strings.ReplaceAll(seg, "%40", "@")
}

// Key helpers (bucket-relative).

// ProfileKey returns users/<principal>/profile.json.
func ProfileKey(principal string) string {
	return "users/" + encodePrincipal(principal) + "/profile.json"
}

// InboxKey returns users/<principal>/invitations/index.json.
func InboxKey(principal string) string {
	return "users/" + encodePrincipal(principal) + "/invitations/index.json"
}

// OrgKey returns orgs/<org>/org.json.
func OrgKey(org string) string { return "orgs/" + org + "/org.json" }

// MembersKey returns orgs/<org>/members.json.
func MembersKey(org string) string { return "orgs/" + org + "/members.json" }

// TeamKey returns orgs/<org>/teams/<slug>.json.
func TeamKey(org, slug string) string { return "orgs/" + org + "/teams/" + slug + ".json" }

// TeamPrefix returns orgs/<org>/teams/.
func TeamPrefix(org string) string { return "orgs/" + org + "/teams/" }

// OrgInviteKey returns orgs/<org>/invitations/<id>.json.
func OrgInviteKey(org, id string) string { return "orgs/" + org + "/invitations/" + id + ".json" }

// OrgInvitePrefix returns orgs/<org>/invitations/.
func OrgInvitePrefix(org string) string { return "orgs/" + org + "/invitations/" }

// AccessKey returns repos/<o>/<r>/access.json.
func AccessKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/access.json"
}

// RepoInviteKey returns repos/<o>/<r>/meta/invitations/<id>.json.
func RepoInviteKey(owner, repo, id string) string {
	return "repos/" + owner + "/" + repo + "/meta/invitations/" + id + ".json"
}

// RepoInvitePrefix returns repos/<o>/<r>/meta/invitations/.
func RepoInvitePrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/meta/invitations/"
}

// Service is the identity store client: all bucket I/O for users, orgs,
// teams, access.json, and invitations. Construct with New; Cfg may be nil
// (anonymous_read defaults open, matching api.Env.gate).
type Service struct {
	Store store.ObjectStore
	Cfg   *config.Config
	Now   func() time.Time
	Rand  io.Reader

	// Repos lists all [owner, repo] pairs (for org-delete guards, team
	// binding cleanup, and the access-bootstrap sweep). Defaults to a
	// delimiter listing over repos/; composition may inject the registry.
	Repos func(ctx context.Context) ([][2]string, error)

	access *accessCache
	teams  *teamCache

	// Stream publishes the 08 §4 "access" frame after an access.json CAS
	// commit (nil = no-op; composition wires the repo bus). Synchronous
	// post-commit fan-out per P8; the access doc is the backfill truth.
	Stream func(ctx context.Context, repo string)
}

// New builds a Service over st.
func New(st store.ObjectStore, cfg *config.Config) *Service {
	s := &Service{
		Store:  st,
		Cfg:    cfg,
		Now:    time.Now,
		Rand:   rand.Reader,
		access: newAccessCache(512),
		teams:  newTeamCache(512),
	}
	s.Repos = s.listRepos
	return s
}

// listRepos enumerates [owner, repo] pairs via delimiter listings
// (collaboration/admin paths only — never a git hot path).
func (s *Service) listRepos(ctx context.Context) ([][2]string, error) {
	var owners []string
	if err := s.Store.ListPrefixes(ctx, "repos/", func(p string) error {
		owners = append(owners, strings.TrimSuffix(strings.TrimPrefix(p, "repos/"), "/"))
		return nil
	}); err != nil {
		return nil, err
	}
	var out [][2]string
	for _, o := range owners {
		var repos []string
		if err := s.Store.ListPrefixes(ctx, "repos/"+o+"/", func(p string) error {
			repos = append(repos, strings.TrimSuffix(strings.TrimPrefix(p, "repos/"+o+"/"), "/"))
			return nil
		}); err != nil {
			return nil, err
		}
		for _, r := range repos {
			out = append(out, [2]string{o, r})
		}
	}
	return out, nil
}

// nowUTC is the clock, UTC.
func (s *Service) nowUTC() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

// anonymousRead is the host-wide lever (06 §8.8).
func (s *Service) anonymousRead() bool {
	if s.Cfg == nil {
		return true
	}
	return s.Cfg.Server.Auth.AnonymousRead
}

// principalOf resolves the request principal: the injected auth.Principal,
// else the mode default (mode none → everyone is anon with write+admin).
func (s *Service) principalOf(ctx context.Context) auth.Principal {
	if p, ok := ctx.Value(principalKey{}).(auth.Principal); ok {
		return p
	}
	if s.Cfg != nil && s.Cfg.Server.Auth.Mode == "none" {
		return auth.None()
	}
	return auth.Anonymous()
}

// Authenticator resolves the request principal through Seam 2 (the
// server's AuthService, injected by composition). Identity never mints
// principals and never reads credentials; a nil Authenticator falls back
// to the mode default.
type Authenticator func(r *http.Request) (auth.Principal, *auth.AuthError)

// randomHex returns n random bytes as hex.
func (s *Service) randomHex(n int) (string, error) {
	rd := s.Rand
	if rd == nil {
		rd = rand.Reader
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(rd, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// casUpdate is the canonical CAS loop (13 §3): read, apply, Update(version),
// retry-on-412 re-read, bounded at 5 attempts, then 409. f receives the
// current body (nil when absent) and version ("" when absent) and returns the
// replacement body (nil deletes nothing — deletion has its own path), whether
// to write, and an error (ErrConflict-surfaced or validation). On success it
// returns the written meta.
func (s *Service) casUpdate(ctx context.Context, key string, f func(cur []byte, ver store.Version) ([]byte, bool, error)) (store.ObjectMeta, error) {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		cur, meta, err := store.GetBytes(ctx, s.Store, key, store.GetOptions{})
		if err != nil {
			if store.IsNotFound(err) {
				cur, meta = nil, store.ObjectMeta{}
			} else {
				return store.ObjectMeta{}, err
			}
		}
		next, write, ferr := f(cur, meta.Version)
		if ferr != nil {
			return store.ObjectMeta{}, ferr
		}
		if !write {
			return meta, nil
		}
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/json"}
		if meta.Version == "" {
			opts.Mode = store.PutCreate
		}
		m, perr := store.PutBytes(ctx, s.Store, key, next, opts)
		if perr == nil {
			return m, nil
		}
		if !store.IsPreconditionFailed(perr) {
			return store.ObjectMeta{}, perr
		}
	}
	return store.ObjectMeta{}, fmt.Errorf("%w: %s changed concurrently; reload and retry", ErrConflict, key)
}
