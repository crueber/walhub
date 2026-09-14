package config

// Forgejo #522: the [features] settings section — six per-repo feature
// flags (issues, pulls, releases, forks, watch, star), each an *enabled*
// flag defaulting all-on. Table-driven parse/resolve/merge coverage in the
// load_test.go style (subtests named by case).

import (
	"strings"
	"testing"
)

func TestParseRepoSettingsFeatures(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		check   func(t *testing.T, rs *RepoSettings)
	}{
		{
			name:    "absent section stays nil",
			payload: "[bundles]\nmin_commits = 5\n",
			check: func(t *testing.T, rs *RepoSettings) {
				t.Helper()
				if rs.Features != nil {
					t.Fatalf("features = %+v, want nil", rs.Features)
				}
				if got := rs.Features.Resolve(); got != AllFeatures() {
					t.Fatalf("resolve nil = %+v, want all-on", got)
				}
			},
		},
		{
			name:    "empty section resolves all-on",
			payload: "[features]\n",
			check: func(t *testing.T, rs *RepoSettings) {
				t.Helper()
				if rs.Features == nil {
					t.Fatal("features section must decode non-nil")
				}
				if got := rs.Features.Resolve(); got != AllFeatures() {
					t.Fatalf("resolve = %+v, want all-on", got)
				}
			},
		},
		{
			name:    "explicit disables resolve off, others stay on",
			payload: "[features]\nissues = false\npulls = false\nreleases = false\nforks = false\nwatch = false\nstar = false\n",
			check: func(t *testing.T, rs *RepoSettings) {
				t.Helper()
				got := rs.Features.Resolve()
				want := ResolvedFeatures{}
				if got != want {
					t.Fatalf("resolve = %+v, want all-off", got)
				}
			},
		},
		{
			name:    "partial section mixes explicit and default",
			payload: "description = \"meta\"\n[features]\nissues = false\nstar = true\n",
			check: func(t *testing.T, rs *RepoSettings) {
				t.Helper()
				got := rs.Features.Resolve()
				if got.Issues {
					t.Error("issues must resolve off")
				}
				if !got.Star || !got.Pulls || !got.Releases || !got.Forks || !got.Watch {
					t.Fatalf("resolve = %+v, want only issues off", got)
				}
				if rs.Description != "meta" {
					t.Fatalf("description = %q, want coexisting key intact", rs.Description)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs, err := ParseRepoSettings([]byte(tt.payload))
			if err != nil {
				t.Fatalf("ParseRepoSettings: %v", err)
			}
			tt.check(t, rs)
		})
	}
}

func TestParseRepoSettingsFeaturesRejects(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr string
	}{
		{name: "unknown key in features", payload: "[features]\nteleport = true\n", wantErr: "unknown key"},
		{name: "wrong type", payload: "[features]\nissues = \"no\"\n", wantErr: "repo settings"},
		{name: "bare features key", payload: "features = true\n", wantErr: "repo settings"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRepoSettings([]byte(tt.payload))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestRepoSettingsFeaturesMergeIgnored(t *testing.T) {
	// Features ride the doc (persistence, revisioning, admin writes) but
	// never merge into the host config — the Description precedent: Merge
	// and ValidateAgainst both succeed, and the merged config is the host
	// config on every merged section.
	rs, err := ParseRepoSettings([]byte("[features]\nissues = false\n[bundles]\nmin_commits = 5\n"))
	if err != nil {
		t.Fatalf("ParseRepoSettings: %v", err)
	}
	base := Defaults()
	merged, err := rs.Merge(base)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if merged.Bundles.MinCommits != 5 {
		t.Fatalf("merged bundles = %+v, want settings value", merged.Bundles)
	}
	if err := rs.ValidateAgainst(base); err != nil {
		t.Fatalf("ValidateAgainst with features: %v", err)
	}
}

func TestFeaturesOf(t *testing.T) {
	tests := []struct {
		name string
		body string
		want ResolvedFeatures
	}{
		{name: "empty body is all-on", body: "", want: AllFeatures()},
		{name: "whitespace body is all-on", body: "  \n", want: AllFeatures()},
		{name: "no section is all-on", body: "description = \"hi\"\n", want: AllFeatures()},
		{name: "corrupt body fails open to all-on", body: "[broken\n", want: AllFeatures()},
		{
			name: "unknown key fails open to all-on",
			body: "[features]\nteleport = true\n",
			want: AllFeatures(),
		},
		{
			name: "partial disable",
			body: "[features]\nissues = false\nstar = false\n",
			want: ResolvedFeatures{Issues: false, Pulls: true, Releases: true, Forks: true, Watch: true, Star: false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FeaturesOf([]byte(tt.body)); got != tt.want {
				t.Fatalf("FeaturesOf = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestResolvedFeaturesETagBits(t *testing.T) {
	// Fixed order (issues, pulls, releases, forks, watch, star), six chars,
	// always — the summary ETag suffix is stable and greppable.
	if got := AllFeatures().ETagBits(); got != "111111" {
		t.Fatalf("all-on = %q", got)
	}
	if got := (ResolvedFeatures{}).ETagBits(); got != "000000" {
		t.Fatalf("all-off = %q", got)
	}
	got := ResolvedFeatures{Issues: true, Pulls: false, Releases: true, Forks: true, Watch: false, Star: true}.ETagBits()
	if got != "101101" {
		t.Fatalf("mixed = %q, want 101101", got)
	}
}
