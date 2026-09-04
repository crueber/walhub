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

	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// AccessBinding is one role binding: subject user:<email> or
// team:<org>/<slug>, role on the P6 ladder.
type AccessBinding struct {
	Subject string `json:"subject"`
	Role    Role   `json:"role"`
}

// AccessDoc is repos/<o>/<r>/access.json: visibility + role bindings.
// Version is the CAS token; PUTs must carry the version they read.
type AccessDoc struct {
	Version      int             `json:"version"`
	Visibility   Visibility      `json:"visibility"`
	RoleBindings []AccessBinding `json:"role_bindings"`
	UpdatedAt    string          `json:"updated_at"`
}

// accessFile is the stored shape (version carried explicitly for CAS).
type accessFile struct {
	Version     int             `json:"version"`
	Visibility  string          `json:"visibility"`
	RoleBinding []AccessBinding `json:"role_bindings"`
	UpdatedAt   string          `json:"updated_at"`
}

// validSubject reports whether subject is user:<email> or team:<org>/<slug>.
func validSubject(sub string) error {
	if rest, ok := strings.CutPrefix(sub, "user:"); ok {
		if !ValidPrincipal(rest) {
			return fmt.Errorf("%w: invalid user subject %q", ErrInvalid, sub)
		}
		return nil
	}
	if rest, ok := strings.CutPrefix(sub, "team:"); ok {
		org, slug, ok := strings.Cut(rest, "/")
		if !ok || !ValidOrg(org) || !ValidSlug(slug) {
			return fmt.Errorf("%w: invalid team subject %q", ErrInvalid, sub)
		}
		return nil
	}
	return fmt.Errorf("%w: subject must be user:<email> or team:<org>/<slug>, got %q", ErrInvalid, sub)
}

// normalizeAccess validates a full access document body: visibility, roles,
// subjects, one binding per subject; returns the sorted bindings.
func normalizeAccess(vis string, bindings []AccessBinding) (Visibility, []AccessBinding, error) {
	if !validVisibility(vis) {
		return "", nil, fmt.Errorf("%w: visibility must be public|private, got %q", ErrInvalid, vis)
	}
	seen := map[string]bool{}
	out := make([]AccessBinding, 0, len(bindings))
	for _, b := range bindings {
		if err := validSubject(b.Subject); err != nil {
			return "", nil, err
		}
		if !validRole(string(b.Role)) {
			return "", nil, fmt.Errorf("%w: invalid role %q", ErrInvalid, string(b.Role))
		}
		key := strings.ToLower(b.Subject)
		if strings.HasPrefix(key, "user:") {
			b.Subject = "user:" + normPrincipal(strings.TrimPrefix(b.Subject, "user:"))
			key = strings.ToLower(b.Subject)
		}
		if seen[key] {
			return "", nil, fmt.Errorf("%w: duplicate binding for subject %q", ErrInvalid, b.Subject)
		}
		seen[key] = true
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	return Visibility(vis), out, nil
}

// SynthesizeDefault returns the §10 legacy default for a repo with no
// access.json: public, with the owner namespace as admin when the owner is
// an email principal. Org-owned repos need no binding (org-owner resolution
// covers them); a non-email, non-org owner namespace yields empty bindings
// with host flags (P6 step 3) still applying. Reads synthesize without
// writing; the access-bootstrap task materializes.
func SynthesizeDefault(owner string) *AccessDoc {
	doc := &AccessDoc{
		Version:      0,
		Visibility:   VisibilityPublic,
		RoleBindings: []AccessBinding{},
	}
	if ValidPrincipal(owner) {
		doc.RoleBindings = append(doc.RoleBindings, AccessBinding{Subject: "user:" + normPrincipal(owner), Role: RoleAdmin})
	}
	return doc
}

// GetAccess reads access.json with a conditional GET per call (control-plane,
// sub-second): the cached CAS version revalidates — NotModified is an LRU
// hit (no body), a new version refreshes the entry. Missing synthesizes the
// §10 legacy default without writing (never cached: a later bootstrap
// Create must be visible on the next probe).
func (s *Service) GetAccess(ctx context.Context, owner, repo string) (*AccessDoc, store.Version, error) {
	key := AccessKey(owner, repo)
	var known store.Version
	var cached *AccessDoc
	if e, ok := s.access.get(owner, repo); ok {
		known, cached = e.ver, e.doc
	}
	res, err := s.Store.Get(ctx, key, store.GetOptions{IfNoneMatch: known})
	if err != nil {
		if store.IsNotFound(err) {
			return SynthesizeDefault(owner), "", nil
		}
		return nil, "", err
	}
	switch r := res.(type) {
	case store.NotModified:
		return cached, known, nil
	case store.Object:
		defer r.Body.Close()
		raw, rerr := io.ReadAll(r.Body)
		if rerr != nil {
			return nil, "", store.NewRetryable(key, rerr)
		}
		doc, perr := parseAccess(raw)
		if perr != nil {
			return nil, "", perr
		}
		s.access.set(owner, repo, r.Meta.Version, doc)
		return doc, r.Meta.Version, nil
	}
	return nil, "", fmt.Errorf("identity: unknown GetResult for %s", key)
}

func parseAccess(raw []byte) (*AccessDoc, error) {
	var f accessFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%w: access.json: %v", ErrInvalid, err)
	}
	vis, bindings, err := normalizeAccess(f.Visibility, f.RoleBinding)
	if err != nil {
		return nil, err
	}
	return &AccessDoc{Version: f.Version, Visibility: vis, RoleBindings: bindings, UpdatedAt: f.UpdatedAt}, nil
}

func encodeAccess(doc *AccessDoc) []byte {
	raw, _ := json.Marshal(accessFile{
		Version:     doc.Version,
		Visibility:  string(doc.Visibility),
		RoleBinding: nonNilBindings(doc.RoleBindings),
		UpdatedAt:   doc.UpdatedAt,
	})
	return raw
}

func nonNilBindings(b []AccessBinding) []AccessBinding {
	if b == nil {
		return []AccessBinding{}
	}
	return b
}

// PutAccess replaces access.json via CAS: the caller passes the version it
// read (0 with no version = create-or-synthesize path per §10). On 412 the
// loop re-reads and re-applies; bounded at 5, then 409. Returns the new doc.
func (s *Service) PutAccess(ctx context.Context, owner, repo string, base store.Version, vis Visibility, bindings []AccessBinding) (*AccessDoc, error) {
	nvis, nbindings, err := normalizeAccess(string(vis), bindings)
	if err != nil {
		return nil, err
	}
	var result *AccessDoc
	_, cerr := s.casUpdate(ctx, AccessKey(owner, repo), func(cur []byte, ver store.Version) ([]byte, bool, error) {
		if base != "" && cur != nil && ver != base {
			return nil, false, fmt.Errorf("%w: access.json changed under you; reload", ErrConflict)
		}
		nextVer := 1
		if cur != nil {
			prev, perr := parseAccess(cur)
			if perr != nil {
				return nil, false, perr
			}
			nextVer = prev.Version + 1
		}
		result = &AccessDoc{Version: nextVer, Visibility: nvis, RoleBindings: nbindings, UpdatedAt: s.nowUTC().Format(time.RFC3339)}
		return encodeAccess(result), true, nil
	})
	if cerr != nil {
		return nil, cerr
	}
	s.access.invalidate(owner, repo)
	if s.Stream != nil {
		s.Stream(ctx, owner+"/"+repo)
	}
	return result, nil
}

// Resolve returns the max role for principal on owner/repo per P6 verbatim:
// (1) access.json bindings (direct + team expansion), max of matches;
// (2) org ownership of the owning org; (3) the auth principal's
// write/admin flags; (4) anonymous → read iff public. Resolution is
// memoized per request by the caller; no lock is involved — two bucket
// GETs worst case plus one teams GET per referenced team, bounded by the
// binding list length.
func (s *Service) Resolve(ctx context.Context, owner, repo string, p auth.Principal) (Role, *AccessDoc) {
	doc, _, err := s.GetAccess(ctx, owner, repo)
	if err != nil {
		doc = SynthesizeDefault(owner)
	}
	best := Role("")
	if !p.Anonymous {
		name := normPrincipal(p.Name)
		for _, b := range doc.RoleBindings {
			var match bool
			if sub, ok := strings.CutPrefix(b.Subject, "user:"); ok {
				match = normPrincipal(sub) == name
			} else if team, ok := strings.CutPrefix(b.Subject, "team:"); ok {
				match = s.inTeam(ctx, team, name)
			}
			if match && b.Role.rank() > best.rank() {
				best = b.Role
			}
		}
		if best == "" && s.isOrgOwner(ctx, owner, name) {
			best = RoleAdmin
		}
	}
	if best == "" {
		switch {
		case p.Admin:
			best = RoleAdmin
		case p.Write:
			best = RoleWrite
		}
	}
	if best == "" && p.Anonymous && doc.Visibility == VisibilityPublic {
		best = RoleRead
	}
	return best, doc
}

// isOrgOwner reports whether principal is an owner in orgs/<org>/members.json.
func (s *Service) isOrgOwner(ctx context.Context, org, principal string) bool {
	m, _, err := s.getMembers(ctx, org)
	if err != nil || m == nil {
		return false
	}
	for _, e := range m.Members {
		if normPrincipal(e.Principal) == normPrincipal(principal) && e.Role == OrgOwner {
			return true
		}
	}
	return false
}

// inTeam reports whether principal is in orgs/<org>/teams/<slug>.json.
func (s *Service) inTeam(ctx context.Context, team, principal string) bool {
	org, slug, ok := strings.Cut(team, "/")
	if !ok {
		return false
	}
	t, _, err := s.GetTeam(ctx, org, slug)
	if err != nil || t == nil {
		return false
	}
	for _, m := range t.Members {
		if normPrincipal(m) == normPrincipal(principal) {
			return true
		}
	}
	return false
}

// --- access LRU (01 §4.1: in-process LRU stamped by the CAS version) --------
//
// A changed version invalidates lazily: entries carry the store Version they
// were read at. Mutation paths invalidate explicitly; a concurrent writer on
// another instance surfaces on the next conditional revalidation — staleness
// is bounded by one request, exactly the ref→sha LRU pattern (07 §5).

type accessEntry struct {
	ver store.Version
	doc *AccessDoc
}

type accessCache struct {
	mu     sync.Mutex
	cap    int
	data   map[string]*accessEntry
	hits   uint64
	misses uint64
}

func newAccessCache(cap int) *accessCache {
	return &accessCache{cap: cap, data: map[string]*accessEntry{}}
}

func accessKey(owner, repo string) string { return owner + "/" + repo }

func (c *accessCache) get(owner, repo string) (*accessEntry, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.data[accessKey(owner, repo)]
	if ok {
		c.hits++
	} else {
		c.misses++
	}
	return e, ok
}

func (c *accessCache) set(owner, repo string, ver store.Version, doc *AccessDoc) {
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
	c.data[accessKey(owner, repo)] = &accessEntry{ver: ver, doc: doc}
}

func (c *accessCache) invalidate(owner, repo string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, accessKey(owner, repo))
}

// Hits/Misses expose LRU behavior for the EVIDENCE harness.
func (c *accessCache) stats() (hits, misses uint64) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}
