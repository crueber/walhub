package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/policy"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// This file owns the read/write checks surface: report, per-sha reads,
// the combined view, the paged index, CI token CRUD, index compaction,
// the open-PR head lookup for §8 fan-out, and the require_checks
// merge-time gate (the pulls ChecksGate seam entry point).

// --- roles (P6, same ladder as internal/issues and internal/pulls) -----------

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

// requireRead enforces the read gate (identity require_read hook when
// wired: public visibility or role ≥ read).
func (s *Service) requireRead(ctx context.Context, owner, repo string, p auth.Principal) error {
	if p.Admin || p.Write {
		return nil
	}
	if s.Roles == nil {
		if p.Anonymous {
			return fmt.Errorf("%w", ErrUnauthorized)
		}
		return nil
	}
	if aerr := s.Roles.CheckRead(ctx, owner, repo, p); aerr != nil {
		switch aerr.Kind {
		case auth.ErrForbidden:
			return fmt.Errorf("%w: %s", ErrForbidden, aerr.Why)
		case auth.ErrUnavailable:
			return fmt.Errorf("identity unavailable: %s", aerr.Why)
		default:
			return fmt.Errorf("%w: %s", ErrUnauthorized, aerr.Why)
		}
	}
	return nil
}

// requireAuthenticated rejects anonymous callers (reports and token
// management need a principal to attribute).
func requireAuthenticated(p auth.Principal) error {
	if p.Anonymous {
		return fmt.Errorf("%w", ErrUnauthorized)
	}
	return nil
}

// --- report ------------------------------------------------------------------

// ReportInput shapes POST …/checks/statuses/{sha} (§4): {context, state,
// target_url?, description?, started_at?, completed_at?}. Unknown keys are
// rejected at the HTTP layer before this is built.
type ReportInput struct {
	Context     string
	State       string
	TargetURL   string
	Description string
	StartedAt   *string
	CompletedAt *string
}

// ReportStatus records one CI line item (§2 steps 1–4): validate context,
// state, and sha (the sha MUST resolve to a commit via Commits, else
// 404) → Create-or-CAS the status record → CAS the index projection →
// broadcast the SSE check packet and, on failure/error transitions for
// open-PR heads, enqueue notifications — all synchronously post-CAS (P8).
//
// Auth: a ci:<id> principal (from a wct_ credential) verifies its secret
// against this repo's token record (mismatch/revoked = 401, valid but
// scopeless = 403); anyone else needs the repo write role. secret is the
// raw secret from the wct_ credential ("" when the caller used no CI
// token — a ci: principal with no secret is a 401).
func (s *Service) ReportStatus(ctx context.Context, owner, repo, sha string, p auth.Principal, secret string, in ReportInput) (*StatusDoc, error) {
	if err := requireAuthenticated(p); err != nil {
		return nil, err
	}
	fullSHA, err := normalizeSHA(sha)
	if err != nil {
		return nil, err
	}
	if err := ValidContext(in.Context); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if !validState(in.State) {
		return nil, fmt.Errorf("%w: state must be one of pending|success|failure|error, got %q", ErrInvalidState, in.State)
	}
	if err := validTargetURL(in.TargetURL); err != nil {
		return nil, err
	}
	if err := validDescription(in.Description); err != nil {
		return nil, err
	}
	started, err := parseOptTime(in.StartedAt, "started_at")
	if err != nil {
		return nil, err
	}
	completed, err := parseOptTime(in.CompletedAt, "completed_at")
	if err != nil {
		return nil, err
	}
	// Auth (05 §3): CI token (checks:write on this repo) OR repo write
	// role. The capability check is handler-side — the frozen Principal
	// is untouched.
	creator := ""
	if id, ok := IsCIPrincipal(p.Name); ok {
		tok, _, terr := s.loadToken(ctx, owner, repo, id)
		if terr != nil {
			return nil, terr
		}
		if tok == nil || secret == "" || !verifySecret(secret, tok.TokenHash) {
			return nil, fmt.Errorf("%w: invalid CI token", ErrUnauthorized)
		}
		if tok.RevokedAt != nil {
			return nil, fmt.Errorf("%w: CI token revoked", ErrUnauthorized)
		}
		if !hasScope(tok.Scopes, CITokenScope) {
			return nil, fmt.Errorf("%w: CI token lacks checks:write", ErrForbidden)
		}
		creator = normPrincipal(p.Name)
	} else {
		if err := s.requireRole(ctx, owner, repo, p, "write"); err != nil {
			return nil, err
		}
		creator = normPrincipal(p.Name)
	}
	// Step 1 (tail): the sha must resolve to a commit.
	if s.Commits == nil {
		return nil, fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	resolved, cerr := s.Commits.ResolveCommit(ctx, repoName(owner, repo), fullSHA)
	if cerr != nil {
		return nil, cerr
	}
	if resolved != "" {
		fullSHA = resolved
	}
	now := s.nowUTC().Format(dateTimeFmt)
	key := StatusKey(owner, repo, fullSHA, in.Context)
	// Step 2: Create-then-CAS (last-write-wins; old results are never
	// deleted — a re-run overwrites the same object).
	fresh := &StatusDoc{
		SHA: fullSHA, Context: in.Context, State: in.State,
		TargetURL: in.TargetURL, Description: in.Description,
		StartedAt: started, CompletedAt: completed,
		Creator: creator, CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	if in.State == StatePending {
		fresh.CompletedAt = nil // cleared on a re-report of pending
	}
	var prev *StatusDoc
	if err := s.putCreate(ctx, key, encodeStatus(fresh)); err != nil {
		if !store.IsPreconditionFailed(err) {
			return nil, err
		}
		// 412: the context already reported for this sha — retry as a
		// CAS Update (bounded 5, then 503).
		updated, perr := s.casUpdate(ctx, key, 5, func(cur []byte, ver store.Version) ([]byte, bool, error) {
			if cur == nil {
				return encodeStatus(fresh), true, nil
			}
			curDoc, perr := parseStatus(cur)
			if perr != nil {
				return nil, false, perr
			}
			prev = curDoc
			curDoc.State = in.State
			curDoc.TargetURL = in.TargetURL
			curDoc.Description = in.Description
			curDoc.StartedAt = started
			curDoc.CompletedAt = completed
			if in.State == StatePending {
				curDoc.CompletedAt = nil
			}
			curDoc.UpdatedAt = now
			curDoc.Version++
			return encodeStatus(curDoc), true, nil
		})
		if perr != nil {
			if isConflict(perr) {
				return nil, fmt.Errorf("%w: status %s for %s changed concurrently; retry", ErrUnavailable, in.Context, fullSHA)
			}
			return nil, perr
		}
		_ = updated
		raw, _, gerr := s.getJSON(ctx, key)
		if gerr != nil {
			return nil, gerr
		}
		fresh, _ = parseStatus(raw)
	}
	// Step 3: CAS the index projection (best-effort — a lost update costs
	// one stale table row until the next report; the per-context objects
	// are the backfill truth).
	written := s.updateIndex(ctx, owner, repo, fresh)
	if written > IndexSizeLimit {
		_, _ = s.CompactIndex(ctx, owner, repo)
	}
	// Step 4: broadcast + fan-out (synchronously post-CAS, P8).
	combined := s.combinedFor(ctx, owner, repo, fullSHA)
	s.stream(ctx, StreamEvent{
		Name: StreamName, Repo: repoName(owner, repo),
		SHA: fullSHA, Context: in.Context, State: fresh.State,
		CombinedState: combined, UpdatedAt: fresh.UpdatedAt,
	})
	if (fresh.State == StateFailure || fresh.State == StateError) &&
		(prev == nil || (prev.State != StateFailure && prev.State != StateError)) {
		s.notifyHeads(ctx, owner, repo, fresh)
	}
	return fresh, nil
}

// parseOptTime parses an optional RFC 3339 timestamp (nil/"" ⇒ nil).
func parseOptTime(v *string, field string) (*string, error) {
	if v == nil || strings.TrimSpace(*v) == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(*v))
	if err != nil {
		return nil, fmt.Errorf("%w: %s must be RFC 3339, got %q", ErrInvalid, field, *v)
	}
	out := t.UTC().Format(dateTimeFmt)
	return &out, nil
}

// hasScope reports whether scopes carries scope.
func hasScope(scopes []string, scope string) bool {
	for _, sc := range scopes {
		if sc == scope {
			return true
		}
	}
	return false
}

// isConflict reports a CAS-exhaustion error.
func isConflict(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "changed concurrently")
}

// --- reads -------------------------------------------------------------------

// StatusesView is GET …/checks/statuses/{sha}: {sha, statuses} context-sorted.
type StatusesView struct {
	SHA      string       `json:"sha"`
	Statuses []*StatusDoc `json:"statuses"`
}

// CombinedView is GET …/checks/{sha} (§5): worst-of state, per-state
// counts, and the context-sorted statuses.
type CombinedView struct {
	SHA         string         `json:"sha"`
	State       string         `json:"state"`
	TotalCounts map[string]int `json:"total_counts"`
	Statuses    []*StatusDoc   `json:"statuses"`
}

// ChecksPage is GET …/checks: the paged index projection.
type ChecksPage struct {
	Checks []IndexSHA `json:"checks"`
	More   bool       `json:"more"`
}

// GetStatuses reads one sha's statuses (read; no-store). LIST under
// checks/<sha>/ + bounded parallel GETs (cap 8). Zero contexts ⇒ empty
// list (no 404 — the combined view maps that to pending).
func (s *Service) GetStatuses(ctx context.Context, owner, repo, sha string, p auth.Principal) (*StatusesView, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	fullSHA, err := normalizeSHA(sha)
	if err != nil {
		return nil, err
	}
	statuses, err := s.loadStatuses(ctx, owner, repo, fullSHA)
	if err != nil {
		return nil, err
	}
	return &StatusesView{SHA: fullSHA, Statuses: statuses}, nil
}

// Combined aggregates worst-of over one sha's contexts (§5; read,
// no-store). Zero contexts ⇒ pending with zero counts.
func (s *Service) Combined(ctx context.Context, owner, repo, sha string, p auth.Principal) (*CombinedView, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	fullSHA, err := normalizeSHA(sha)
	if err != nil {
		return nil, err
	}
	statuses, err := s.loadStatuses(ctx, owner, repo, fullSHA)
	if err != nil {
		return nil, err
	}
	return combineView(fullSHA, statuses), nil
}

// combineView folds statuses into the §5 view (pure; shared by Combined
// and the report broadcast path).
func combineView(sha string, statuses []*StatusDoc) *CombinedView {
	states := make([]string, 0, len(statuses))
	counts := map[string]int{StatePending: 0, StateSuccess: 0, StateFailure: 0, StateError: 0}
	for _, st := range statuses {
		states = append(states, st.State)
		counts[st.State]++
	}
	if statuses == nil {
		statuses = []*StatusDoc{}
	}
	return &CombinedView{SHA: sha, State: combinedState(states), TotalCounts: counts, Statuses: statuses}
}

// loadStatuses lists one sha's status keys and fetches them with bounded
// parallelism (cap 8 concurrent GETs — a CAS retry loop IS the
// single-flight here; reports are CI-rate, never git-hot-path).
func (s *Service) loadStatuses(ctx context.Context, owner, repo, sha string) ([]*StatusDoc, error) {
	prefix := ChecksPrefix(owner, repo, sha)
	var keys []string
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, ".json") {
			keys = append(keys, m.Key)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(keys)
	out := make([]*StatusDoc, len(keys))
	if len(keys) == 0 {
		return []*StatusDoc{}, nil
	}
	const fanout = 8
	sem := make(chan struct{}, fanout)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for i, k := range keys {
		wg.Add(1)
		go func(i int, k string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				if firstErr == nil {
					firstErr = ctx.Err()
				}
				mu.Unlock()
				return
			}
			raw, _, err := s.getJSON(ctx, k)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			if raw == nil {
				return // raced a delete (none exist) — skip
			}
			doc, perr := parseStatus(raw)
			if perr != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = perr
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			out[i] = doc
			mu.Unlock()
		}(i, k)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	statuses := make([]*StatusDoc, 0, len(out))
	for _, doc := range out {
		if doc != nil {
			statuses = append(statuses, doc)
		}
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Context < statuses[j].Context })
	return statuses, nil
}

// combinedFor returns the combined state for the broadcast packet
// (best-effort: a read failure degrades to pending, never a 500 for the
// reporter — the write already committed).
func (s *Service) combinedFor(ctx context.Context, owner, repo, sha string) string {
	statuses, err := s.loadStatuses(ctx, owner, repo, sha)
	if err != nil {
		return StatePending
	}
	states := make([]string, 0, len(statuses))
	for _, st := range statuses {
		states = append(states, st.State)
	}
	return combinedState(states)
}

// ListChecks serves the paged index projection (read, no-store, P5
// paginated): name-cursor after, n default 50 max 200. Unknown cursor ⇒
// from the top. Absent index ⇒ empty page.
func (s *Service) ListChecks(ctx context.Context, owner, repo string, p auth.Principal, after string, n int) (*ChecksPage, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	ix, err := s.loadIndex(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	rows := ix.SHAs
	if rows == nil {
		rows = []IndexSHA{}
	}
	start := 0
	if after != "" {
		for i, row := range rows {
			if strings.EqualFold(row.SHA, after) {
				start = i + 1
				break
			}
		}
	}
	end := start + n
	more := false
	if end < len(rows) {
		more = true
	} else {
		end = len(rows)
	}
	page := rows[start:end]
	if page == nil {
		page = []IndexSHA{}
	}
	return &ChecksPage{Checks: page, More: more}, nil
}

// --- index (P4) ----------------------------------------------------------------

// loadIndex reads checks/index.json; (empty, nil) when absent.
func (s *Service) loadIndex(ctx context.Context, owner, repo string) (*IndexDoc, error) {
	raw, _, err := s.getJSON(ctx, IndexKey(owner, repo))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return &IndexDoc{SHAs: []IndexSHA{}}, nil
	}
	return parseIndex(raw)
}

// updateIndex upserts one sha's context row by its own CAS loop (P4).
// Bounded at 5 attempts, then it PROCEEDS WITHOUT the index update —
// LIST fallback covers reads, so staleness is a performance gap, never
// correctness. Returns the written byte size (for the inline compact
// trigger).
func (s *Service) updateIndex(ctx context.Context, owner, repo string, st *StatusDoc) int {
	key := IndexKey(owner, repo)
	for attempt := 0; attempt < 5; attempt++ {
		raw, ver, err := s.getJSON(ctx, key)
		if err != nil {
			return 0
		}
		var ix *IndexDoc
		if raw == nil {
			ix = &IndexDoc{SHAs: []IndexSHA{}}
		} else if ix, err = parseIndex(raw); err != nil {
			return 0
		}
		upsertIndexSHA(ix, st)
		ix.Version++
		out := encodeIndex(ix)
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
		if ver == "" {
			opts.Mode = store.PutCreate
		}
		if _, perr := store.PutBytes(ctx, s.Store, key, out, opts); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return 0
		}
		return len(out)
	}
	return 0
}

// upsertIndexSHA inserts or refreshes a sha's context row, newest-first.
// Context rows sort by name; the sha's state re-derives worst-of.
func upsertIndexSHA(ix *IndexDoc, st *StatusDoc) {
	row := IndexContext{Name: st.Context, State: st.State, UpdatedAt: st.UpdatedAt}
	at := -1
	for i, entry := range ix.SHAs {
		if strings.EqualFold(entry.SHA, st.SHA) {
			at = i
			break
		}
	}
	if at < 0 {
		ix.SHAs = append([]IndexSHA{{
			SHA: st.SHA, State: st.State, Contexts: []IndexContext{row}, UpdatedAt: st.UpdatedAt,
		}}, ix.SHAs...)
		at = 0
	} else {
		entry := &ix.SHAs[at]
		found := false
		for i, c := range entry.Contexts {
			if c.Name == row.Name {
				entry.Contexts[i] = row
				found = true
				break
			}
		}
		if !found {
			entry.Contexts = append(entry.Contexts, row)
		}
		sort.Slice(entry.Contexts, func(i, j int) bool { return entry.Contexts[i].Name < entry.Contexts[j].Name })
		entry.UpdatedAt = st.UpdatedAt
		states := make([]string, 0, len(entry.Contexts))
		for _, c := range entry.Contexts {
			states = append(states, c.State)
		}
		entry.State = combinedState(states)
		// Move the touched sha to the front (newest-first).
		touched := ix.SHAs[at]
		copy(ix.SHAs[1:at+1], ix.SHAs[:at])
		ix.SHAs[0] = touched
	}
	head := &ix.SHAs[0]
	if head.Contexts == nil {
		head.Contexts = []IndexContext{}
	}
}

// CompactIndex evicts the oldest sha rows while the object exceeds
// IndexSizeLimit and trims to the hot window (newest 500 shas),
// advancing nothing monotonically (shas fall out by recency; the
// per-context objects are the truth — compaction never deletes them).
// Returns true when it compacted. Best-effort: CAS exhaustion is
// dropped, the next report retries.
func (s *Service) CompactIndex(ctx context.Context, owner, repo string) (bool, error) {
	key := IndexKey(owner, repo)
	for attempt := 0; attempt < 5; attempt++ {
		raw, ver, err := s.getJSON(ctx, key)
		if err != nil {
			return false, err
		}
		if raw == nil {
			return false, nil
		}
		if len(raw) <= IndexSizeLimit {
			ix, perr := parseIndex(raw)
			if perr != nil {
				return false, perr
			}
			if len(ix.SHAs) <= IndexHotWindow {
				return false, nil
			}
		}
		ix, perr := parseIndex(raw)
		if perr != nil {
			return false, perr
		}
		trimmed := false
		if len(ix.SHAs) > IndexHotWindow {
			ix.SHAs = ix.SHAs[:IndexHotWindow]
			trimmed = true
		}
		for len(encodeIndex(ix)) > IndexSizeLimit && len(ix.SHAs) > 0 {
			ix.SHAs = ix.SHAs[:len(ix.SHAs)-1]
			trimmed = true
		}
		if !trimmed {
			return false, nil
		}
		ix.Version++
		if _, perr := store.PutBytes(ctx, s.Store, key, encodeIndex(ix),
			store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return false, perr
		}
		return true, nil
	}
	return false, nil
}

// --- CI tokens -----------------------------------------------------------------

// TokenCreated is the POST …/checks/tokens answer: the secret travels
// here once, never again.
type TokenCreated struct {
	ID     string   `json:"id"`
	Token  string   `json:"token"`
	Scopes []string `json:"scopes"`
}

// TokenView is one row of GET …/checks/tokens (no secrets, ever).
type TokenView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	CreatedBy string   `json:"created_by"`
	CreatedAt string   `json:"created_at"`
	RevokedAt *string  `json:"revoked_at,omitempty"`
}

// CreateToken mints a CI token (admin): {name, scopes?} → the record plus
// the once-shown wct_<id>.<secret>. Scopes default to [checks:write]; any
// other scope is rejected (granting repo-write or admin via a CI token is
// explicitly rejected). Id allocation retries on 412 collision.
func (s *Service) CreateToken(ctx context.Context, owner, repo string, p auth.Principal, name string, scopes []string) (*TokenCreated, error) {
	if err := requireAuthenticated(p); err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, repo, p, "admin"); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 100 {
		return nil, fmt.Errorf("%w: name is required (≤ 100 chars)", ErrInvalid)
	}
	if len(scopes) == 0 {
		scopes = []string{CITokenScope}
	}
	for _, sc := range scopes {
		if sc != CITokenScope {
			return nil, fmt.Errorf("%w: unsupported scope %q (v1: exactly [checks:write])", ErrInvalid, sc)
		}
	}
	now := s.nowUTC().Format(dateTimeFmt)
	who := normPrincipal(p.Name)
	for attempt := 0; attempt < 5; attempt++ {
		id, err := mintTokenID()
		if err != nil {
			return nil, fmt.Errorf("%w: mint: %v", ErrUnavailable, err)
		}
		secret, err := mintSecret()
		if err != nil {
			return nil, fmt.Errorf("%w: mint: %v", ErrUnavailable, err)
		}
		doc := &CITokenDoc{
			ID: id, Name: name, TokenHash: hashSecret(secret),
			Scopes:    append([]string{}, scopes...),
			CreatedBy: who, CreatedAt: now, Version: 1,
		}
		if err := s.putCreate(ctx, TokenKey(owner, repo, id), encodeToken(doc)); err != nil {
			if store.IsPreconditionFailed(err) {
				continue // id collision — mint again
			}
			return nil, err
		}
		return &TokenCreated{ID: id, Token: TokenPrefix + id + "." + secret, Scopes: append([]string{}, scopes...)}, nil
	}
	return nil, fmt.Errorf("%w: token id allocation collided; retry", ErrUnavailable)
}

// ListTokens lists token records without secrets (admin, no-store).
func (s *Service) ListTokens(ctx context.Context, owner, repo string, p auth.Principal) ([]TokenView, error) {
	if err := requireAuthenticated(p); err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, repo, p, "admin"); err != nil {
		return nil, err
	}
	var keys []string
	if err := s.Store.List(ctx, TokensPrefix(owner, repo), "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, ".json") {
			keys = append(keys, m.Key)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(keys)
	out := []TokenView{}
	for _, k := range keys {
		raw, _, err := s.getJSON(ctx, k)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		doc, perr := parseToken(raw)
		if perr != nil {
			return nil, perr
		}
		out = append(out, TokenView{
			ID: doc.ID, Name: doc.Name, Scopes: nonNilStr(doc.Scopes),
			CreatedBy: doc.CreatedBy, CreatedAt: doc.CreatedAt, RevokedAt: doc.RevokedAt,
		})
	}
	return out, nil
}

// RevokeToken sets revoked_at (admin). Revoked records are RETAINED so
// old credentials fail with 403/401, never absent-ambiguity. Idempotent:
// revoking twice is still 204. Unknown id ⇒ 404.
func (s *Service) RevokeToken(ctx context.Context, owner, repo, id string, p auth.Principal) error {
	if err := requireAuthenticated(p); err != nil {
		return err
	}
	if err := s.requireRole(ctx, owner, repo, p, "admin"); err != nil {
		return err
	}
	if !tokenIDRe.MatchString(strings.ToLower(strings.TrimSpace(id))) {
		return fmt.Errorf("%w: unknown CI token %q", ErrNotFound, id)
	}
	id = strings.ToLower(strings.TrimSpace(id))
	key := TokenKey(owner, repo, id)
	now := s.nowUTC().Format(dateTimeFmt)
	_, err := s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown CI token %q", ErrNotFound, id)
		}
		doc, perr := parseToken(cur)
		if perr != nil {
			return nil, false, perr
		}
		if doc.RevokedAt != nil {
			return nil, false, nil // already revoked — idempotent no-op
		}
		doc.RevokedAt = &now
		doc.Version++
		return encodeToken(doc), true, nil
	})
	return err
}

// loadToken reads one token record; (nil, "", nil) when absent.
func (s *Service) loadToken(ctx context.Context, owner, repo, id string) (*CITokenDoc, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, TokenKey(owner, repo, id))
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return nil, "", nil
	}
	doc, perr := parseToken(raw)
	if perr != nil {
		return nil, "", perr
	}
	return doc, ver, nil
}

// --- §8 fan-out ----------------------------------------------------------------

// openPRHead holds one open PR's head for the failure lookup.
type openPRHead struct {
	Num  int
	Head string
}

// notifyHeads enqueues check_reported for every OPEN PR whose head equals
// the reported sha (§8: bounded lookup at collaboration rate — the shared
// issues/index.json supplies open PR nums, one pr.json GET per row,
// capped; success/pending transitions emit nothing).
func (s *Service) notifyHeads(ctx context.Context, owner, repo string, st *StatusDoc) {
	heads, err := s.openPRHeads(ctx, owner, repo, 200)
	if err != nil {
		return // best-effort per P8 (the CAS committed; fan-out retries next report)
	}
	for _, h := range heads {
		if !strings.EqualFold(h.Head, st.SHA) {
			continue
		}
		s.emit(ctx, NotifyEvent{
			Repo: repoName(owner, repo), Class: NotifyClass, Actor: st.Creator,
			SHA: st.SHA, Context: st.Context, State: st.State,
			Description: st.Description, TargetURL: st.TargetURL, PR: h.Num,
		})
	}
}

// openPRHeads scans open PRs (kind:"pr" cards from the shared
// issues/index.json — 02 owns the card shape; 03 §2.1 owns the pr.json
// sidecar layout referenced here) and returns their head shas. Bounded
// at maxRows pr.json GETs (open PRs are human-scale; the cap keeps a
// pathological index from fanning out). Merged or closed PRs are
// skipped (only open-PR heads notify).
func (s *Service) openPRHeads(ctx context.Context, owner, repo string, maxRows int) ([]openPRHead, error) {
	raw, _, err := s.getJSON(ctx, "repos/"+owner+"/"+repo+"/issues/index.json")
	if err != nil || raw == nil {
		return nil, err
	}
	var ix struct {
		Open []struct {
			Num   int    `json:"num"`
			Kind  string `json:"kind"`
			State string `json:"state"`
		} `json:"open"`
	}
	if err := json.Unmarshal(raw, &ix); err != nil {
		return nil, fmt.Errorf("%w: issues/index.json: %v", ErrCorrupt, err)
	}
	var out []openPRHead
	for _, c := range ix.Open {
		if c.Kind != "pr" || c.State != "open" {
			continue
		}
		if len(out) >= maxRows {
			break
		}
		praw, _, perr := s.getJSON(ctx, fmt.Sprintf("repos/%s/%s/pulls/%06x/pr.json", owner, repo, c.Num))
		if perr != nil || praw == nil {
			continue
		}
		var pr struct {
			Head struct {
				SHA string `json:"sha"`
			} `json:"head"`
			Merged bool `json:"merged"`
		}
		if jerr := json.Unmarshal(praw, &pr); jerr != nil || pr.Merged || pr.Head.SHA == "" {
			continue
		}
		// Confirm the thread is still open (the index is a projection;
		// the header wins).
		traw, _, terr := s.getJSON(ctx, fmt.Sprintf("repos/%s/%s/issues/%06x/thread.json", owner, repo, c.Num))
		if terr != nil || traw == nil {
			continue
		}
		var th struct {
			State string `json:"state"`
			Kind  string `json:"kind"`
		}
		if jerr := json.Unmarshal(traw, &th); jerr != nil || th.Kind != "pr" || th.State != "open" {
			continue
		}
		out = append(out, openPRHead{Num: c.Num, Head: pr.Head.SHA})
	}
	return out, nil
}

// --- require_checks merge-time gate (pulls ChecksGate seam) --------------------

// GateTimeout bounds the gate's store reads inside the merge task's
// context; a blown deadline fails closed (same discipline as review's
// gate).
const GateTimeout = 15 * time.Second

// CheckRequiredChecks is the merge-time half of the require_checks
// extension (05 §6), consulted by 03's merge task at merge time ONLY
// (never the push path — statuses arrive asynchronously after a push).
// It resolves the PR head sha's combined view and refuses unless EVERY
// required context is present AND success. Refusal is the verbatim
// message: merge refused: required checks not green for <sha>: <ctx>
// (<state|missing>), … No matching rules carrying the gate ⇒ nil. An
// unparseable policy.json fails closed. Bypass: union covers matching
// rules the merger does NOT bypass (03 §5 step 4 — bypass lists apply
// unchanged; 05's "no bypass list" means require_checks carries no
// bypass FIELD of its own).
func (s *Service) CheckRequiredChecks(ctx context.Context, owner, repo, headSHA, baseRef, merger string) error {
	gctx, cancel := context.WithTimeout(ctx, GateTimeout)
	defer cancel()
	fullSHA, err := normalizeSHA(headSHA)
	if err != nil {
		return err
	}
	raw, _, err := s.getJSON(gctx, "repos/"+owner+"/"+repo+"/policy.json")
	if err != nil {
		return err
	}
	if raw == nil {
		return nil // no policy ⇒ no required-checks rules ⇒ pass
	}
	doc, perr := policy.Parse(raw)
	if perr != nil {
		return fmt.Errorf("%w: policy.json unparseable: %v", ErrConflict, perr)
	}
	required := requiredUnion(doc, baseRef, merger)
	if len(required) == 0 {
		return nil
	}
	statuses, err := s.loadStatuses(gctx, owner, repo, fullSHA)
	if err != nil {
		return err
	}
	byCtx := map[string]string{}
	for _, st := range statuses {
		byCtx[st.Context] = st.State
	}
	var offenders []string
	for _, name := range required {
		st, ok := byCtx[name]
		if !ok {
			offenders = append(offenders, name+" (missing)")
		} else if st != StateSuccess {
			offenders = append(offenders, name+" ("+st+")")
		}
	}
	if len(offenders) > 0 {
		return fmt.Errorf("merge refused: required checks not green for %s: %s", fullSHA, strings.Join(offenders, ", "))
	}
	return nil
}

// requiredUnion collects the union of require_checks contexts across
// matching protect rules the merger does not bypass (05 §6 combination
// rule), sorted for stable refusal messages.
func requiredUnion(doc *policy.Document, baseRef, merger string) []string {
	rules := policy.MatchingRules(doc, policy.Request{Principal: merger, Ref: baseRef, Op: policy.OpUpdate})
	seen := map[string]bool{}
	var out []string
	for _, r := range rules {
		pe := r.Protect()
		if pe == nil || len(pe.RequireChecks) == 0 {
			continue
		}
		if policy.Bypassed(pe.Bypass, merger, nil, doc.Roster()) {
			continue // bypassed rules contribute nothing (03 §5 step 4)
		}
		for _, c := range pe.RequireChecks {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	sort.Strings(out)
	return out
}
