package notify

// Forgejo #522: the watch guard — SetWatch(on) refuses with 403 when the
// repo's watch flag is off (the toggle binds everyone, no admin bypass);
// unwatch always works; existing watchers and fan-out are untouched;
// re-enabling restores. The hook fails open (nil or declining → allow):
// display metadata must never break a write on a transient settings read.

import (
	"context"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
)

func TestSetWatchDisabledRefuses(t *testing.T) {
	off := config.ResolvedFeatures{Issues: true, Pulls: true, Releases: true, Forks: true, Watch: false, Star: true}
	on := config.AllFeatures()
	tests := []struct {
		name    string
		hook    func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool)
		wantErr error // nil = watch allowed
	}{
		{name: "nil hook allows (fail-open)", hook: nil, wantErr: nil},
		{name: "declining hook allows (fail-open)", hook: func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool) {
			return config.ResolvedFeatures{}, false
		}, wantErr: nil},
		{name: "watch enabled allows", hook: func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool) {
			return on, true
		}, wantErr: nil},
		{name: "watch disabled refuses", hook: func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool) {
			return off, true
		}, wantErr: ErrForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			x := newHarness(t)
			seedRepo(t, x, "acme", "repo")
			x.svc.Features = tt.hook
			st, err := x.svc.SetWatch(ctx(), "amy@example.com", "acme", "repo", true)
			if tt.wantErr == nil {
				if err != nil || !st.Watching || st.Watchers != 1 {
					t.Fatalf("watch = %+v, %v; want watching/1", st, err)
				}
				return
			}
			if !isErr(err, tt.wantErr) {
				t.Fatalf("watch = %+v, %v; want %v", st, err, tt.wantErr)
			}
			if got := statusFor(err); got != 403 {
				t.Fatalf("status = %d, want 403", got)
			}
			// No record minted: the refusal is side-effect free (a
			// later re-enable watches cleanly).
			if raw, _, _ := x.svc.getJSON(ctx(), WatchingKey("amy@example.com", "acme", "repo")); raw != nil {
				t.Fatal("refused watch minted a record")
			}
		})
	}
}

func TestUnwatchAlwaysWorksWhenDisabled(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "acme", "repo")
	// Watch while enabled, then disable: the existing watcher is
	// retained and counted.
	st, err := x.svc.SetWatch(ctx(), "amy@example.com", "acme", "repo", true)
	if err != nil || !st.Watching || st.Watchers != 1 {
		t.Fatalf("watch = %+v, %v", st, err)
	}
	off := config.ResolvedFeatures{Issues: true, Pulls: true, Releases: true, Forks: true, Watch: false, Star: true}
	x.svc.Features = func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool) {
		return off, true
	}
	// The retained watcher still reads back (fan-out unaffected).
	if got := x.svc.GetWatch(ctx(), "amy@example.com", "acme", "repo"); !got.Watching || got.Watchers != 1 {
		t.Fatalf("getwatch = %+v; want watching/1 retained", got)
	}
	// Unwatch always works, even while disabled.
	st, err = x.svc.SetWatch(ctx(), "amy@example.com", "acme", "repo", false)
	if err != nil || st.Watching || st.Watchers != 0 {
		t.Fatalf("unwatch = %+v, %v; want not-watching/0", st, err)
	}
	// Re-enabling restores the affordance with counts intact (0 now —
	// the unwatch above stands).
	x.svc.Features = func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool) {
		return config.AllFeatures(), true
	}
	st, err = x.svc.SetWatch(ctx(), "amy@example.com", "acme", "repo", true)
	if err != nil || !st.Watching || st.Watchers != 1 {
		t.Fatalf("re-enabled watch = %+v, %v; want watching/1", st, err)
	}
}
