package pulls

// Forgejo #522: the fork guard — POST …/forks (StartFork) refuses with
// 403 when the source repo's forks flag is off. The toggle binds the
// source repo's policy (existing forks and the network listing are
// untouched; re-enabling restores). The hook fails open (nil or
// declining → allow): display metadata must never break a write on a
// transient settings read.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
)

func TestStartForkDisabledRefuses(t *testing.T) {
	off := config.ResolvedFeatures{Issues: true, Pulls: true, Releases: true, Forks: false, Watch: true, Star: true}
	on := config.AllFeatures()
	tests := []struct {
		name    string
		hook    func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool)
		wantErr error // nil = guard passes (the taken-name precheck proves it)
	}{
		{name: "nil hook allows (fail-open)", hook: nil, wantErr: nil},
		{name: "declining hook allows (fail-open)", hook: func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool) {
			return config.ResolvedFeatures{}, false
		}, wantErr: nil},
		{name: "forks enabled allows", hook: func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool) {
			return on, true
		}, wantErr: nil},
		{name: "forks disabled refuses", hook: func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool) {
			return off, true
		}, wantErr: ErrForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv()
			e.roles.Roles["jane@example.com"] = "write"
			e.svc.Features = tt.hook
			// Seed the taken-name precheck: reaching ErrConflict
			// proves the features guard passed (it runs first).
			raw, _ := json.Marshal(&ForkDoc{Parent: "x/y", ForkedAt: "t", Version: 1})
			if err := e.svc.putCreate(ctx(), ForkKey("o", "taken"), raw); err != nil {
				t.Fatalf("seed: %v", err)
			}
			_, _, err := e.svc.StartFork(ctx(), "o", "r", writer(), ForkInput{Name: "taken"})
			if tt.wantErr == nil {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("err = %v; want ErrConflict (guard passed, name taken)", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v; want %v", err, tt.wantErr)
			}
			if got := statusFor(err); got != 403 {
				t.Fatalf("status = %d, want 403", got)
			}
		})
	}
}

func TestStartForkDisabledBindsWriter(t *testing.T) {
	// The guard fires after the role gate (policy precedes syntax, but
	// auth still precedes policy): a writer on a forks-disabled repo
	// gets the features 403, while a role-less caller still gets the
	// role denial first.
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	off := config.ResolvedFeatures{Issues: true, Pulls: true, Releases: true, Forks: false, Watch: true, Star: true}
	e.svc.Features = func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool) {
		return off, true
	}
	if _, _, err := e.svc.StartFork(ctx(), "o", "r", writer(), ForkInput{Name: "c"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("writer fork on disabled repo: %v", err)
	}
}
