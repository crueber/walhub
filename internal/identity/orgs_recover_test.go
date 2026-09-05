package identity

// Regression tests for Forgejo issue #75: a failed org creation must never
// leave an irrecoverable ownerless namespace. Creation is
// atomic-or-recoverable: the members-seed failure rolls the org.json
// reservation back, and any ownerless residue heals through the resume path
// on retry (including the crash-between-writes case).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store"
)

// membersFailOnce fails the first Put of the given members.json key, then
// delegates: a deterministic injection of the #75 members-write failure.
type membersFailOnce struct {
	store.ObjectStore
	key  string
	err  error
	once bool
	mu   sync.Mutex
}

func (f *membersFailOnce) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if key == f.key && !f.once {
		f.once = true
		return store.ObjectMeta{}, f.err
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

// TestCreateOrgMembersFailureRollsBack: the issue's core scenario. The
// members write fails; the reservation must be rolled back (no org.json
// residue) and a retry must succeed with the creator as owner. Pre-fix the
// retry 409s with "org already exists" and no owner is ever bound.
func TestCreateOrgMembersFailureRollsBack(t *testing.T) {
	inner := store.NewMemory()
	s := New(&membersFailOnce{ObjectStore: inner, key: MembersKey("acme"), err: errBoom}, config.Defaults())
	s.Now = testClock
	ctx := context.Background()

	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); !errors.Is(err, errBoom) {
		t.Fatalf("seed failure must surface the original error, got: %v", err)
	}
	if got, err := s.GetOrg(ctx, "acme"); err != nil || got != nil {
		t.Fatalf("reservation must roll back: GetOrg = %+v, %v", got, err)
	}

	// Retry on the healed store: the fault fired once, so this exercises the
	// plain re-create after rollback.
	s.Store = inner
	o, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com")
	if err != nil {
		t.Fatalf("retry must succeed, got: %v", err)
	}
	if o.Org != "acme" {
		t.Errorf("retry org = %+v", o)
	}
	m, err := s.GetMembers(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Members) != 1 || m.Members[0].Principal != "alice@example.com" || m.Members[0].Role != OrgOwner {
		t.Errorf("retry must bind the creator as owner: %+v", m.Members)
	}
}

// TestCreateOrgFailedRollbackHealsOnRetry: when even the rollback delete
// fails, the original error still surfaces and the orphaned reservation
// heals through the resume path on retry.
func TestCreateOrgFailedRollbackHealsOnRetry(t *testing.T) {
	inner := store.NewMemory()
	s := New(&membersFailOnce{ObjectStore: inner, key: MembersKey("acme"), err: errBoom}, config.Defaults())
	s.Now = testClock
	s.Store = &membersFailOnce{ObjectStore: &deleteFailStore{ObjectStore: inner, err: errDeleteBoom}, key: MembersKey("acme"), err: errBoom}
	ctx := context.Background()

	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); !errors.Is(err, errBoom) {
		t.Fatalf("must return the seed error, not the rollback's, got: %v", err)
	}
	// Rollback failed: the ownerless reservation is still there (the bug's
	// residue), so the retry must take the resume path.
	if got, _ := s.GetOrg(ctx, "acme"); got == nil {
		t.Fatal("rollback-failure fixture needs the reservation present")
	}
	s.Store = inner
	o, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com")
	if err != nil {
		t.Fatalf("retry must heal the orphaned reservation, got: %v", err)
	}
	if o.Org != "acme" {
		t.Errorf("healed org = %+v", o)
	}
	if !s.isOrgOwner(ctx, "acme", "alice@example.com") {
		t.Error("healed org must have the creator as owner")
	}
}

var errDeleteBoom = errors.New("delete boom")

// deleteFailStore fails every Delete: the rollback cannot run.
type deleteFailStore struct {
	store.ObjectStore
	err error
}

func (d *deleteFailStore) Delete(ctx context.Context, key string, v store.Version) error {
	return d.err
}

// crashOrg writes an org.json reservation with no members.json: the
// crash-between-writes residue.
func crashOrg(t *testing.T, s *Service, org string) {
	t.Helper()
	now := testClock().Format("2006-01-02T15:04:05Z07:00")
	o := &Org{Version: 1, Org: org, DisplayName: "Crash", CreatedAt: now, UpdatedAt: now}
	if _, err := store.PutBytes(context.Background(), s.Store, OrgKey(org), encodeOrg(o),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
}

// TestCreateOrgResumeAfterCrash: a reservation without members (process died
// between the writes) heals on retry with the same code path, keeping the
// reserved profile.
func TestCreateOrgResumeAfterCrash(t *testing.T) {
	s := testService()
	ctx := context.Background()
	crashOrg(t, s, "acme")

	o, err := s.CreateOrg(ctx, "acme", "Ignored", "", "alice@example.com")
	if err != nil {
		t.Fatalf("retry on ownerless org must heal, got: %v", err)
	}
	if o.DisplayName != "Crash" {
		t.Errorf("heal must keep the reserved profile, got %+v", o)
	}
	if !s.isOrgOwner(ctx, "acme", "alice@example.com") {
		t.Error("healed org must have the creator as owner")
	}
}

// TestCreateOrgResumeOwnerlessRoster: members.json present but with no owner
// (only a plain member) heals by binding the creator as owner and keeps the
// existing entries.
func TestCreateOrgResumeOwnerlessRoster(t *testing.T) {
	s := testService()
	ctx := context.Background()
	crashOrg(t, s, "acme")
	now := testClock().Format("2006-01-02T15:04:05Z07:00")
	stale := &Members{Version: 1, Members: []Member{{Principal: "sam@example.com", Role: OrgMember, JoinedAt: now}}, UpdatedAt: now}
	if _, err := store.PutBytes(ctx, s.Store, MembersKey("acme"), encodeMembers(stale),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateOrg(ctx, "acme", "Ignored", "", "alice@example.com"); err != nil {
		t.Fatalf("ownerless roster must heal, got: %v", err)
	}
	m, err := s.GetMembers(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Members) != 2 {
		t.Fatalf("heal must preserve entries: %+v", m.Members)
	}
	if !isRosterOwner(m, "alice@example.com") {
		t.Errorf("creator must be owner: %+v", m.Members)
	}
}

// TestCreateOrgConflictKeepsRoster: a genuinely owned org still 409s for a
// rival creator and the roster is untouched (no steal).
func TestCreateOrgConflictKeepsRoster(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOrg(ctx, "acme", "Rival", "", "bob@example.com"); !errors.Is(err, ErrConflict) {
		t.Fatalf("rival create must 409, got: %v", err)
	}
	m, err := s.GetMembers(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Members) != 1 || m.Members[0].Principal != "alice@example.com" {
		t.Errorf("roster must be untouched: %+v", m.Members)
	}
}

// TestCreateOrgIdempotentForOwner: repeating a create as the bound owner
// succeeds (at-least-once callers converge instead of 409ing forever).
func TestCreateOrgIdempotentForOwner(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	o, err := s.CreateOrg(ctx, "acme", "Acme", "", "ALICE@example.com")
	if err != nil {
		t.Fatalf("owner re-create must succeed, got: %v", err)
	}
	if o.Org != "acme" {
		t.Errorf("org = %+v", o)
	}
	m, _ := s.GetMembers(ctx, "acme")
	if len(m.Members) != 1 {
		t.Errorf("idempotent retry must not duplicate the owner: %+v", m.Members)
	}
}

// TestCreateOrgStaleMembersConflict: members.json predating the reservation
// (out-of-band residue owned by someone else) 409s without adopting the
// caller, and the foreign roster is untouched.
func TestCreateOrgStaleMembersConflict(t *testing.T) {
	s := testService()
	ctx := context.Background()
	now := testClock().Format("2006-01-02T15:04:05Z07:00")
	foreign := &Members{Version: 1, Members: []Member{{Principal: "bob@example.com", Role: OrgOwner, JoinedAt: now}}, UpdatedAt: now}
	if _, err := store.PutBytes(ctx, s.Store, MembersKey("acme"), encodeMembers(foreign),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale foreign roster must 409, got: %v", err)
	}
	m, _ := s.GetMembers(ctx, "acme")
	if len(m.Members) != 1 || m.Members[0].Principal != "bob@example.com" {
		t.Errorf("foreign roster must be untouched: %+v", m.Members)
	}
}

// TestCreateOrgStaleMembersSelfHeals: the same stale-members residue heals
// when the reserver IS the bound owner.
func TestCreateOrgStaleMembersSelfHeals(t *testing.T) {
	s := testService()
	ctx := context.Background()
	now := testClock().Format("2006-01-02T15:04:05Z07:00")
	mine := &Members{Version: 1, Members: []Member{{Principal: "alice@example.com", Role: OrgOwner, JoinedAt: now}}, UpdatedAt: now}
	if _, err := store.PutBytes(ctx, s.Store, MembersKey("acme"), encodeMembers(mine),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatalf("bound owner must succeed, got: %v", err)
	}
}

// TestCreateOrgResumeCorruptRoster: a corrupt members.json surfaces the
// parse error honestly instead of 409ing or healing over it.
func TestCreateOrgResumeCorruptRoster(t *testing.T) {
	s := testService()
	ctx := context.Background()
	crashOrg(t, s, "acme")
	if _, err := store.PutBytes(ctx, s.Store, MembersKey("acme"), []byte("{x"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("corrupt roster must surface, got: %v", err)
	}
}

// TestCreateOrgConcurrentRaceOneWinner: N concurrent creates for one name
// elect exactly one winner; losers get 409 and write nothing, and the roster
// holds exactly the winner as sole owner.
func TestCreateOrgConcurrentRaceOneWinner(t *testing.T) {
	s := testService()
	ctx := context.Background()
	const n = 16
	var wg sync.WaitGroup
	wins := make(chan string, n)
	fails := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			creator := fmt.Sprintf("racer%02d@example.com", i)
			if _, err := s.CreateOrg(ctx, "acme", "Acme", "", creator); err != nil {
				fails <- err
				return
			}
			wins <- creator
		}(i)
	}
	wg.Wait()
	close(wins)
	close(fails)
	var winners []string
	for w := range wins {
		winners = append(winners, w)
	}
	nConflict := 0
	for err := range fails {
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("loser must get 409, got: %v", err)
		}
		nConflict++
	}
	if len(winners) != 1 {
		t.Fatalf("exactly one winner, got %d: %v", len(winners), winners)
	}
	if nConflict != n-1 {
		t.Fatalf("every loser must 409: %d conflicts for %d racers", nConflict, n)
	}
	m, err := s.GetMembers(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Members) != 1 || m.Members[0].Principal != winners[0] || m.Members[0].Role != OrgOwner {
		t.Errorf("roster must hold exactly the winner as owner: %+v (winner %q)", m.Members, winners[0])
	}
}

// TestCreateOrgRollbackSkipsDeleteUnderWinner: a members seed that fails
// while a concurrent create already healed a roster under this reservation
// must NOT roll the reservation back from under the winner. The caller
// arbitrates read-only (409 for a non-owner) with org.json intact, and the
// winner's subsequent re-create still succeeds.
func TestCreateOrgRollbackSkipsDeleteUnderWinner(t *testing.T) {
	inner := store.NewMemory()
	ctx := context.Background()
	now := testClock().Format("2006-01-02T15:04:05Z07:00")
	foreign := &Members{Version: 1, Members: []Member{{Principal: "bob@example.com", Role: OrgOwner, JoinedAt: now}}, UpdatedAt: now}
	if _, err := store.PutBytes(ctx, inner, MembersKey("acme"), encodeMembers(foreign),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	s := New(&membersFailOnce{ObjectStore: inner, key: MembersKey("acme"), err: errBoom}, config.Defaults())
	s.Now = testClock

	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-owner must 409, got: %v", err)
	}
	if got, err := s.GetOrg(ctx, "acme"); err != nil || got == nil {
		t.Fatalf("reservation must survive: GetOrg = %+v, %v", got, err)
	}
	m, err := s.GetMembers(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Members) != 1 || m.Members[0].Principal != "bob@example.com" {
		t.Errorf("winner roster must be untouched: %+v", m.Members)
	}

	s.Store = inner
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "bob@example.com"); err != nil {
		t.Fatalf("winner re-create must succeed, got: %v", err)
	}
}
