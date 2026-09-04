package identity

import (
	"context"
	"errors"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store"
)

func TestProfiles(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if p, err := s.GetProfile(ctx, "ghost@example.com"); err != nil || p != nil {
		t.Errorf("GetProfile ghost: %v %+v", err, p)
	}
	if _, err := s.EnsureProfile(ctx, "not-an-email"); !errors.Is(err, ErrInvalid) {
		t.Errorf("EnsureProfile bad principal: %v", err)
	}
	p, err := s.EnsureProfile(ctx, "Jane@Example.com")
	if err != nil {
		t.Fatalf("EnsureProfile: %v", err)
	}
	if p.Principal != "jane@example.com" || p.Version != 1 {
		t.Errorf("bad profile: %+v", p)
	}
	// Idempotent.
	p2, err := s.EnsureProfile(ctx, "jane@example.com")
	if err != nil || p2.Version != 1 {
		t.Errorf("EnsureProfile re-run: %v %+v", err, p2)
	}
	// Put.
	upd, err := s.PutProfile(ctx, "jane@example.com", "Jane", "bio")
	if err != nil {
		t.Fatalf("PutProfile: %v", err)
	}
	if upd.DisplayName != "Jane" || upd.Bio != "bio" || upd.Version != 2 {
		t.Errorf("PutProfile broken: %+v", upd)
	}
	// Put creates when absent.
	nu, err := s.PutProfile(ctx, "new@example.com", "New", "")
	if err != nil || nu.Version != 1 {
		t.Errorf("PutProfile create: %v %+v", err, nu)
	}
	// Corrupt profile.
	if _, err := store.PutBytes(ctx, s.Store, ProfileKey("bad@x.c"), []byte("{x"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetProfile(ctx, "bad@x.c"); !errors.Is(err, ErrInvalid) {
		t.Errorf("corrupt profile: %v", err)
	}
	if _, err := s.PutProfile(ctx, "bad@x.c", "x", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("PutProfile corrupt: %v", err)
	}
	sErr := New(&errStore{ObjectStore: store.NewMemory(), getErr: errBoom}, config.Defaults())
	if _, err := sErr.GetProfile(ctx, "x@y.z"); !errors.Is(err, errBoom) {
		t.Errorf("GetProfile error: %v", err)
	}
	if _, err := sErr.EnsureProfile(ctx, "x@y.z"); !errors.Is(err, errBoom) {
		t.Errorf("EnsureProfile error: %v", err)
	}
	// EnsureProfile losing the Create race re-reads.
	mem := store.NewMemory()
	if _, err := store.PutBytes(ctx, mem, ProfileKey("race@x.c"), encodeProfile(&Profile{Version: 1, Principal: "race@x.c"}),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	sRace := New(&errStore{ObjectStore: mem, putErr: store.NewPrecondition("k", "")}, config.Defaults())
	sRace.Now = testClock
	if p, err := sRace.EnsureProfile(ctx, "race@x.c"); err != nil || p == nil {
		t.Errorf("EnsureProfile race: %v %+v", err, p)
	}
	sRace2 := New(&errStore{ObjectStore: store.NewMemory(), putErr: errBoom}, config.Defaults())
	if _, err := sRace2.EnsureProfile(ctx, "fresh@x.c"); !errors.Is(err, errBoom) {
		t.Errorf("EnsureProfile put error: %v", err)
	}
}
