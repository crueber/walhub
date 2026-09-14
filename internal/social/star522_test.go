package social

// Forgejo #522: the star guard — PUT …/star refuses with 403 when the
// repo's star flag is off (the toggle binds everyone, no admin bypass);
// DELETE (unstar) always works; existing counts are untouched;
// re-enabling restores. The hook fails open (nil or declining → allow):
// display metadata must never break a write on a transient settings read.

import (
	"context"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

func TestStarDisabledRefuses(t *testing.T) {
	off := config.ResolvedFeatures{Issues: true, Pulls: true, Releases: true, Forks: true, Watch: true, Star: false}
	on := config.AllFeatures()
	tests := []struct {
		name    string
		hook    func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool)
		wantErr error // nil = star allowed
	}{
		{name: "nil hook allows (fail-open)", hook: nil, wantErr: nil},
		{name: "declining hook allows (fail-open)", hook: func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool) {
			return config.ResolvedFeatures{}, false
		}, wantErr: nil},
		{name: "star enabled allows", hook: func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool) {
			return on, true
		}, wantErr: nil},
		{name: "star disabled refuses", hook: func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool) {
			return off, true
		}, wantErr: ErrForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			x := newHarness(t)
			seedRepo(t, x, "o", "r")
			x.svc.Features = tt.hook
			n, err := x.svc.Star(ctx(), jane(), "o", "r")
			if tt.wantErr == nil {
				if err != nil || n != 1 {
					t.Fatalf("star = %d, %v; want 1, nil", n, err)
				}
				return
			}
			if !isErr(err, tt.wantErr) {
				t.Fatalf("star = %d, %v; want %v", n, err, tt.wantErr)
			}
			if got := statusFor(err); got != 403 {
				t.Fatalf("status = %d, want 403", got)
			}
			// No record minted, no counter created: the refusal is
			// side-effect free (a later re-enable stars cleanly).
			if raw, _, _ := x.svc.getJSON(ctx(), StarKey("jane", "o", "r")); raw != nil {
				t.Fatal("refused star minted a record")
			}
			if raw, _, _ := x.svc.getJSON(ctx(), SocialKey("o", "r")); raw != nil {
				t.Fatal("refused star created social.json")
			}
		})
	}
}

func TestStarDisabledBindsEveryone(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "o", "r")
	off := config.ResolvedFeatures{Issues: true, Pulls: true, Releases: true, Forks: true, Watch: true, Star: false}
	x.svc.Features = func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool) {
		return off, true
	}
	// Admins and already-starred re-PUTs refuse alike: the PUT is the
	// "new star" affordance, and a uniform refusal keeps the rule one
	// line (the reason names the toggle, not the caller).
	if _, err := x.svc.Star(ctx(), auth.Principal{Name: "root", Admin: true}, "o", "r"); !isErr(err, ErrForbidden) {
		t.Fatalf("admin star: %v", err)
	}
	if _, err := x.svc.Star(ctx(), jane(), "o", "r"); !isErr(err, ErrForbidden) {
		t.Fatalf("first star: %v", err)
	}
	if _, err := x.svc.Star(ctx(), jane(), "o", "r"); !isErr(err, ErrForbidden) {
		t.Fatalf("re-star: %v", err)
	}
}

func TestUnstarAlwaysWorksWhenDisabled(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "o", "r")
	// Star while enabled, then disable: the existing star is retained
	// and counted.
	if n, err := x.svc.Star(ctx(), jane(), "o", "r"); err != nil || n != 1 {
		t.Fatalf("star = %d, %v", n, err)
	}
	off := config.ResolvedFeatures{Issues: true, Pulls: true, Releases: true, Forks: true, Watch: true, Star: false}
	x.svc.Features = func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool) {
		return off, true
	}
	// Counts stay: the toggle never deletes or recounts.
	if d, err := x.svc.Counts(ctx(), jane(), "o", "r"); err != nil || d.Stars != 1 {
		t.Fatalf("counts = %+v, %v; want 1 star retained", d, err)
	}
	// Unstar always works, even while disabled.
	if n, err := x.svc.Unstar(ctx(), jane(), "o", "r"); err != nil || n != 0 {
		t.Fatalf("unstar = %d, %v; want 0, nil", n, err)
	}
	// Re-enabling restores the affordance with counts intact (0 now —
	// the unstar above stands).
	x.svc.Features = func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool) {
		return config.AllFeatures(), true
	}
	if n, err := x.svc.Star(ctx(), jane(), "o", "r"); err != nil || n != 1 {
		t.Fatalf("re-enabled star = %d, %v; want 1, nil", n, err)
	}
}
