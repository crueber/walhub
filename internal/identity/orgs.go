package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// readBody reads a store object body.
func readBody(r io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, store.NewRetryable("", err)
	}
	return raw, nil
}

// Org is orgs/<org>/org.json.
type Org struct {
	Version     int    `json:"version"`
	Org         string `json:"org"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// Member is one roster row.
type Member struct {
	Principal string  `json:"principal"`
	Role      OrgRole `json:"role"`
	JoinedAt  string  `json:"joined_at"`
}

// Members is orgs/<org>/members.json: one object, human-rate.
type Members struct {
	Version   int      `json:"version"`
	Members   []Member `json:"members"`
	UpdatedAt string   `json:"updated_at"`
}

// Team is orgs/<org>/teams/<slug>.json.
type Team struct {
	Version     int      `json:"version"`
	Org         string   `json:"org"`
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Members     []string `json:"members"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

// --- orgs ------------------------------------------------------------------

func encodeOrg(o *Org) []byte {
	raw, _ := json.Marshal(o)
	return raw
}

func parseOrg(raw []byte) (*Org, error) {
	var o Org
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, fmt.Errorf("%w: org.json: %v", ErrInvalid, err)
	}
	return &o, nil
}

// CreateOrg reserves the org namespace (Create of org.json) and seeds
// members.json with the creator as owner. Creation is atomic-or-recoverable:
//
//   - If the members seed fails, the org.json reservation is rolled back
//     (best-effort version-guarded delete) and the original error is
//     returned, so a retry starts clean.
//   - If org.json already exists, the call resumes instead of blindly
//     409ing: when the existing org is ownerless (members.json missing, or a
//     roster with no owner — the crash-between-writes residue) the creator
//     is bound as owner via CAS; when the creator is already an owner the
//     create is idempotent success; otherwise it is 409 "org already exists"
//     and the existing roster is untouched.
//
// ### Concurrency
//
// Hazard: two concurrent creates for one name racing the members seed, or a
// retry racing the in-flight winner's seed, stealing ownership or double
// winning. Avoidance: the namespace (org.json Create) admits exactly one
// reserver; the owner binding is decided by members.json CAS — the seed's
// 412 path re-reads and succeeds only when the caller is actually the bound
// owner (else 409), and the resume path adds the caller as owner only to an
// ownerless roster (CAS loop, bounded per casUpdate) while a roster that
// already has owners is never rewritten. Exactly one creator observes
// success with sole ownership; every loser gets a clean 409 and writes
// nothing. No in-process locks: store CAS is the lock (13 §3).
func (s *Service) CreateOrg(ctx context.Context, org, displayName, description, creator string) (*Org, error) {
	if !ValidOrg(org) {
		return nil, fmt.Errorf("%w: invalid org %q (lowercase [a-z0-9-], 1-39 chars)", ErrInvalid, org)
	}
	now := s.nowUTC().Format(time.RFC3339)
	o := &Org{Version: 1, Org: org, DisplayName: displayName, Description: description, CreatedAt: now, UpdatedAt: now}
	orgMeta, err := store.PutBytes(ctx, s.Store, OrgKey(org), encodeOrg(o),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	if err != nil {
		if store.IsPreconditionFailed(err) {
			return s.resumeOrgCreate(ctx, org, creator)
		}
		return nil, err
	}
	m := &Members{Version: 1, Members: []Member{{Principal: normPrincipal(creator), Role: OrgOwner, JoinedAt: now}}, UpdatedAt: now}
	if _, err := store.PutBytes(ctx, s.Store, MembersKey(org), encodeMembers(m),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		if store.IsPreconditionFailed(err) {
			return s.confirmOrgOwner(ctx, org, o, creator)
		}
		// Roll back the reservation: version-guarded so an org recreated
		// after a completed rollback is never deleted. Best-effort — a
		// failed rollback still leaves the resume path to heal the
		// ownerless reservation, and the original error (never the
		// rollback's) is returned. A roster already exists only if a
		// concurrent create healed under this reservation and won the
		// namespace: never delete under the winner, arbitrate read-only
		// instead. (A heal landing between this check and the delete can
		// still orphan the winner's roster; that residue converges through
		// resumeOrgCreate on retry — cross-object CAS does not exist.)
		if m, _, merr := s.getMembers(ctx, org); merr == nil && m != nil {
			return s.confirmOrgOwner(ctx, org, o, creator)
		}
		_ = s.Store.Delete(ctx, OrgKey(org), orgMeta.Version)
		return nil, err
	}
	return o, nil
}

// confirmOrgOwner resolves a 412 on the initial members seed: the seed lost
// a race (a concurrent resume healed first, or a stale members.json predates
// the reservation). Read-only arbitration — the loser writes nothing:
// success iff the caller is the bound owner, else 409. Any ownerless residue
// this leaves behind converges through resumeOrgCreate on retry.
func (s *Service) confirmOrgOwner(ctx context.Context, org string, o *Org, creator string) (*Org, error) {
	m, _, err := s.getMembers(ctx, org)
	if err != nil {
		return nil, err
	}
	if m != nil && isRosterOwner(m, creator) {
		return o, nil
	}
	return nil, fmt.Errorf("%w: org already exists", ErrConflict)
}

// resumeOrgCreate completes a create whose org.json reservation already
// exists: idempotent success when the caller is already an owner, a CAS heal
// binding the caller as owner when the org is ownerless, else 409 leaving
// the existing roster untouched.
func (s *Service) resumeOrgCreate(ctx context.Context, org, creator string) (*Org, error) {
	creator = normPrincipal(creator)
	now := s.nowUTC().Format(time.RFC3339)
	_, err := s.casUpdate(ctx, MembersKey(org), func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			// Crash-between-writes residue (or a rolled-back-then-retried
			// reservation): bind the creator as owner.
			m := &Members{Version: 1, Members: []Member{{Principal: creator, Role: OrgOwner, JoinedAt: now}}, UpdatedAt: now}
			return encodeMembers(m), true, nil
		}
		m, perr := parseMembers(cur)
		if perr != nil {
			return nil, false, perr
		}
		if isRosterOwner(m, creator) {
			return nil, false, nil
		}
		if rosterHasOwner(m) {
			return nil, false, fmt.Errorf("%w: org already exists", ErrConflict)
		}
		m.Members = append(m.Members, Member{Principal: creator, Role: OrgOwner, JoinedAt: now})
		m.Version++
		m.UpdatedAt = now
		sortMembers(m)
		return encodeMembers(m), true, nil
	})
	if err != nil {
		return nil, err
	}
	got, gerr := s.GetOrg(ctx, org)
	if gerr != nil {
		return nil, gerr
	}
	if got == nil {
		return nil, fmt.Errorf("%w: unknown org %q", ErrNotFound, org)
	}
	return got, nil
}

// rosterHasOwner reports whether any roster entry holds the owner role.
func rosterHasOwner(m *Members) bool {
	for _, e := range m.Members {
		if e.Role == OrgOwner {
			return true
		}
	}
	return false
}

// isRosterOwner reports whether principal holds the owner role.
func isRosterOwner(m *Members, principal string) bool {
	principal = normPrincipal(principal)
	for _, e := range m.Members {
		if normPrincipal(e.Principal) == principal && e.Role == OrgOwner {
			return true
		}
	}
	return false
}

// GetOrg reads org.json; nil when absent.
func (s *Service) GetOrg(ctx context.Context, org string) (*Org, error) {
	raw, _, err := store.GetBytes(ctx, s.Store, OrgKey(org), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseOrg(raw)
}

// PutOrg edits the org profile (CAS).
func (s *Service) PutOrg(ctx context.Context, org, displayName, description string) (*Org, error) {
	var result *Org
	_, err := s.casUpdate(ctx, OrgKey(org), func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown org %q", ErrNotFound, org)
		}
		prev, perr := parseOrg(cur)
		if perr != nil {
			return nil, false, perr
		}
		prev.Version++
		prev.DisplayName = displayName
		prev.Description = description
		prev.UpdatedAt = s.nowUTC().Format(time.RFC3339)
		result = prev
		return encodeOrg(prev), true, nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ListOrgs returns sorted org names via delimiter listing (collaboration
// page, not a git hot path; P5).
func (s *Service) ListOrgs(ctx context.Context) ([]string, error) {
	var out []string
	if err := s.Store.ListPrefixes(ctx, "orgs/", func(p string) error {
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(p, "orgs/"), "/"))
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// --- members ---------------------------------------------------------------

func encodeMembers(m *Members) []byte {
	raw, _ := json.Marshal(m)
	return raw
}

func parseMembers(raw []byte) (*Members, error) {
	var m Members
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%w: members.json: %v", ErrInvalid, err)
	}
	if m.Members == nil {
		m.Members = []Member{}
	}
	return &m, nil
}

func sortMembers(m *Members) {
	sort.Slice(m.Members, func(i, j int) bool { return m.Members[i].Principal < m.Members[j].Principal })
}

// getMembers reads members.json; nil when the org does not exist.
func (s *Service) getMembers(ctx context.Context, org string) (*Members, store.Version, error) {
	raw, meta, err := store.GetBytes(ctx, s.Store, MembersKey(org), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	m, perr := parseMembers(raw)
	if perr != nil {
		return nil, "", perr
	}
	return m, meta.Version, nil
}

// GetMembers reads the roster; nil when the org does not exist.
func (s *Service) GetMembers(ctx context.Context, org string) (*Members, error) {
	m, _, err := s.getMembers(ctx, org)
	return m, err
}

// SetMember adds or re-roles a roster entry (CAS). Role must be
// owner|member.
func (s *Service) SetMember(ctx context.Context, org, principal string, role OrgRole) (*Members, error) {
	principal = normPrincipal(principal)
	if !ValidPrincipal(principal) {
		return nil, fmt.Errorf("%w: invalid principal %q", ErrInvalid, principal)
	}
	if !validOrgRole(string(role)) {
		return nil, fmt.Errorf("%w: invalid org role %q", ErrInvalid, string(role))
	}
	var result *Members
	_, err := s.casUpdate(ctx, MembersKey(org), func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown org %q", ErrNotFound, org)
		}
		m, perr := parseMembers(cur)
		if perr != nil {
			return nil, false, perr
		}
		found := false
		for i := range m.Members {
			if normPrincipal(m.Members[i].Principal) == principal {
				m.Members[i].Principal = principal
				m.Members[i].Role = role
				found = true
			}
		}
		if !found {
			m.Members = append(m.Members, Member{Principal: principal, Role: role, JoinedAt: s.nowUTC().Format(time.RFC3339)})
		}
		m.Version++
		m.UpdatedAt = s.nowUTC().Format(time.RFC3339)
		sortMembers(m)
		result = m
		return encodeMembers(m), true, nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// RemoveMember drops a roster entry (CAS); removing the last owner is 409.
func (s *Service) RemoveMember(ctx context.Context, org, principal string) (*Members, error) {
	principal = normPrincipal(principal)
	var result *Members
	_, err := s.casUpdate(ctx, MembersKey(org), func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown org %q", ErrNotFound, org)
		}
		m, perr := parseMembers(cur)
		if perr != nil {
			return nil, false, perr
		}
		kept := m.Members[:0]
		removedOwner := false
		for _, e := range m.Members {
			if normPrincipal(e.Principal) == principal {
				if e.Role == OrgOwner {
					removedOwner = true
				}
				continue
			}
			kept = append(kept, e)
		}
		if removedOwner {
			owners := 0
			for _, e := range kept {
				if e.Role == OrgOwner {
					owners++
				}
			}
			if owners == 0 {
				return nil, false, fmt.Errorf("%w: cannot remove the last owner", ErrConflict)
			}
		}
		m.Members = kept
		m.Version++
		m.UpdatedAt = s.nowUTC().Format(time.RFC3339)
		sortMembers(m)
		result = m
		return encodeMembers(m), true, nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// --- teams -----------------------------------------------------------------

func encodeTeam(t *Team) []byte {
	raw, _ := json.Marshal(t)
	return raw
}

func parseTeam(raw []byte) (*Team, error) {
	var t Team
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("%w: team: %v", ErrInvalid, err)
	}
	if t.Members == nil {
		t.Members = []string{}
	}
	return &t, nil
}

// CreateTeam reserves the team slug (Create; lost race is 409).
func (s *Service) CreateTeam(ctx context.Context, org, slug, name, description string) (*Team, error) {
	if !ValidSlug(slug) {
		return nil, fmt.Errorf("%w: invalid team slug %q", ErrInvalid, slug)
	}
	if m, _, merr := s.getMembers(ctx, org); merr != nil {
		return nil, merr
	} else if m == nil {
		return nil, fmt.Errorf("%w: unknown org %q", ErrNotFound, org)
	}
	now := s.nowUTC().Format(time.RFC3339)
	t := &Team{Version: 1, Org: org, Slug: slug, Name: name, Description: description, Members: []string{}, CreatedAt: now, UpdatedAt: now}
	if _, err := store.PutBytes(ctx, s.Store, TeamKey(org, slug), encodeTeam(t),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		if store.IsPreconditionFailed(err) {
			return nil, fmt.Errorf("%w: team already exists", ErrConflict)
		}
		return nil, err
	}
	return t, nil
}

// GetTeam reads a team with the same conditional-GET discipline as
// access.json: the cached version revalidates per call. Nil when absent.
func (s *Service) GetTeam(ctx context.Context, org, slug string) (*Team, store.Version, error) {
	key := TeamKey(org, slug)
	var known store.Version
	var cached *Team
	if e, ok := s.teams.get(org, slug); ok {
		known, cached = e.ver, e.team
	}
	res, err := s.Store.Get(ctx, key, store.GetOptions{IfNoneMatch: known})
	if err != nil {
		if store.IsNotFound(err) {
			// Evict any stale entry: the team is gone and the cached
			// roster must not survive it (the entry is ignored on this
			// path, but lingering garbage is a leak, not a cache).
			s.teams.invalidate(org, slug)
			return nil, "", nil
		}
		return nil, "", err
	}
	switch r := res.(type) {
	case store.NotModified:
		return cached, known, nil
	case store.Object:
		defer r.Body.Close()
		raw, rerr := readBody(r.Body)
		if rerr != nil {
			return nil, "", rerr
		}
		t, perr := parseTeam(raw)
		if perr != nil {
			return nil, "", perr
		}
		s.teams.set(org, slug, r.Meta.Version, t)
		return t, r.Meta.Version, nil
	}
	return nil, "", fmt.Errorf("identity: unknown GetResult for %s", key)
}

// PutTeam edits name/description (CAS).
func (s *Service) PutTeam(ctx context.Context, org, slug, name, description string) (*Team, error) {
	var result *Team
	_, err := s.casUpdate(ctx, TeamKey(org, slug), func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown team %q", ErrNotFound, org+"/"+slug)
		}
		t, perr := parseTeam(cur)
		if perr != nil {
			return nil, false, perr
		}
		t.Version++
		t.Name = name
		t.Description = description
		t.UpdatedAt = s.nowUTC().Format(time.RFC3339)
		result = t
		return encodeTeam(t), true, nil
	})
	if err != nil {
		return nil, err
	}
	s.teams.invalidate(org, slug)
	return result, nil
}

// ListTeams returns sorted teams via LIST over orgs/<org>/teams/*.json
// (collaboration page, not a git hot path; P5). n caps the page.
func (s *Service) ListTeams(ctx context.Context, org string, n int) ([]*Team, error) {
	if n <= 0 || n > 1000 {
		n = 100
	}
	var out []*Team
	var ferr error
	if err := s.Store.List(ctx, TeamPrefix(org), "", func(m store.ObjectMeta) error {
		if len(out) >= n || !strings.HasSuffix(m.Key, ".json") {
			return nil
		}
		raw, _, gerr := store.GetBytes(ctx, s.Store, m.Key, store.GetOptions{})
		if gerr != nil {
			if store.IsNotFound(gerr) {
				return nil
			}
			// Keep listing past unreadable entries; the error surfaces
			// after the pass so one corrupt object cannot hide the rest.
			ferr = gerr
			return nil
		}
		t, perr := parseTeam(raw)
		if perr != nil {
			return nil
		}
		out = append(out, t)
		return nil
	}); err != nil {
		return nil, err
	}
	if ferr != nil {
		return nil, ferr
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	if out == nil {
		out = []*Team{}
	}
	return out, nil
}

// SetTeamMember adds a principal to a team (CAS).
func (s *Service) SetTeamMember(ctx context.Context, org, slug, principal string) (*Team, error) {
	principal = normPrincipal(principal)
	if !ValidPrincipal(principal) {
		return nil, fmt.Errorf("%w: invalid principal %q", ErrInvalid, principal)
	}
	var result *Team
	_, err := s.casUpdate(ctx, TeamKey(org, slug), func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown team %q", ErrNotFound, org+"/"+slug)
		}
		t, perr := parseTeam(cur)
		if perr != nil {
			return nil, false, perr
		}
		for _, m := range t.Members {
			if normPrincipal(m) == principal {
				result = t
				return nil, false, nil
			}
		}
		t.Members = append(t.Members, principal)
		sort.Strings(t.Members)
		t.Version++
		t.UpdatedAt = s.nowUTC().Format(time.RFC3339)
		result = t
		return encodeTeam(t), true, nil
	})
	if err != nil {
		return nil, err
	}
	s.teams.invalidate(org, slug)
	return result, nil
}

// RemoveTeamMember drops a principal from a team (CAS).
func (s *Service) RemoveTeamMember(ctx context.Context, org, slug, principal string) (*Team, error) {
	principal = normPrincipal(principal)
	var result *Team
	_, err := s.casUpdate(ctx, TeamKey(org, slug), func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown team %q", ErrNotFound, org+"/"+slug)
		}
		t, perr := parseTeam(cur)
		if perr != nil {
			return nil, false, perr
		}
		kept := t.Members[:0]
		for _, m := range t.Members {
			if normPrincipal(m) != principal {
				kept = append(kept, m)
			}
		}
		t.Members = kept
		t.Version++
		t.UpdatedAt = s.nowUTC().Format(time.RFC3339)
		result = t
		return encodeTeam(t), true, nil
	})
	if err != nil {
		return nil, err
	}
	s.teams.invalidate(org, slug)
	return result, nil
}

// DeleteOrg removes org.json, members.json, teams, and org invitations.
// Owner-only (checked by the handler). Refuses with 409 + count while any
// repo is owned by the org.
func (s *Service) DeleteOrg(ctx context.Context, org string) error {
	if !ValidOrg(org) {
		return fmt.Errorf("%w: invalid org %q", ErrInvalid, org)
	}
	repos, err := s.Repos(ctx)
	if err != nil {
		return err
	}
	owned := 0
	for _, rr := range repos {
		if rr[0] == org {
			owned++
		}
	}
	if owned > 0 {
		return fmt.Errorf("%w: org owns %d repos; transfer or delete them first", ErrConflict, owned)
	}
	// List teams BEFORE deleting anything: a LIST failure aborts with the
	// org intact instead of half-deleted with leaked team objects.
	teams, terr := s.ListTeams(ctx, org, 1000)
	if terr != nil {
		return terr
	}
	for _, key := range []string{OrgKey(org), MembersKey(org)} {
		if derr := s.Store.Delete(ctx, key, ""); derr != nil && !store.IsNotFound(derr) {
			return derr
		}
	}
	for _, t := range teams {
		_ = s.Store.Delete(ctx, TeamKey(org, t.Slug), "")
	}
	var invErr error
	_ = s.Store.List(ctx, OrgInvitePrefix(org), "", func(m store.ObjectMeta) error {
		if derr := s.Store.Delete(ctx, m.Key, ""); derr != nil && !store.IsNotFound(derr) && invErr == nil {
			invErr = derr
		}
		return nil
	})
	return invErr
}

// DeleteTeam removes the team object and strips its team:<org>/<slug>
// bindings from every access.json that references it: repos are enumerated
// via the registry lister and each access.json is probed by exact key
// (never a LIST sweep of repo contents); affected repos get one sequential
// CAS each. Unconditional: a team with no bindings deletes one object.
func (s *Service) DeleteTeam(ctx context.Context, org, slug string) error {
	subject := "team:" + org + "/" + slug
	repos, err := s.Repos(ctx)
	if err != nil {
		return err
	}
	for _, rr := range repos {
		owner, repo := rr[0], rr[1]
		_, cerr := s.casUpdate(ctx, AccessKey(owner, repo), func(cur []byte, _ store.Version) ([]byte, bool, error) {
			if cur == nil {
				return nil, false, nil
			}
			doc, perr := parseAccess(cur)
			if perr != nil {
				// Unreadable access.json: leave it for fsck, keep sweeping.
				return nil, false, nil
			}
			kept := make([]AccessBinding, 0, len(doc.RoleBindings))
			found := false
			for _, b := range doc.RoleBindings {
				if b.Subject == subject {
					found = true
					continue
				}
				kept = append(kept, b)
			}
			if !found {
				return nil, false, nil
			}
			doc.RoleBindings = kept
			doc.Version++
			doc.UpdatedAt = s.nowUTC().Format(time.RFC3339)
			return encodeAccess(doc), true, nil
		})
		if cerr != nil {
			// 412s retry inside casUpdate (bounded, then 409): a concurrent
			// admin edit surfaces honestly instead of silently dropping the
			// strip for this repo.
			return cerr
		}
		s.access.invalidate(owner, repo)
	}
	if err := s.Store.Delete(ctx, TeamKey(org, slug), ""); err != nil && !store.IsNotFound(err) {
		return err
	}
	s.teams.invalidate(org, slug)
	return nil
}

// --- team LRU (same version-stamped discipline as access.json) --------------

type teamEntry struct {
	ver  store.Version
	team *Team
}

type teamCache struct {
	mu   sync.Mutex
	cap  int
	data map[string]*teamEntry
}

func newTeamCache(cap int) *teamCache { return &teamCache{cap: cap, data: map[string]*teamEntry{}} }

func teamCacheKey(org, slug string) string { return org + "/" + slug }

func (c *teamCache) get(org, slug string) (*teamEntry, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.data[teamCacheKey(org, slug)]
	return e, ok
}

func (c *teamCache) set(org, slug string, ver store.Version, t *Team) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.data) >= c.cap {
		for k := range c.data {
			delete(c.data, k)
			break
		}
	}
	c.data[teamCacheKey(org, slug)] = &teamEntry{ver: ver, team: t}
}

func (c *teamCache) invalidate(org, slug string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, teamCacheKey(org, slug))
}
