package config

// Forgejo #586: the [review] settings section — the per-repo
// allow_self_approval knob (default ON: fresh repos behave like GitHub).
// Table-driven parse/resolve/merge coverage in the load_test.go style
// (subtests named by case), following the #522 [features] precedent:
// pointers keep "unset" distinct from "explicitly disabled".

import (
	"strings"
	"testing"
)

func TestParseRepoSettingsReview(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		check   func(t *testing.T, rs *RepoSettings)
	}{
		{
			name:    "absent section stays nil and resolves allowed",
			payload: "[bundles]\nmin_commits = 5\n",
			check: func(t *testing.T, rs *RepoSettings) {
				t.Helper()
				if rs.Review != nil {
					t.Fatalf("review = %+v, want nil", rs.Review)
				}
				if !rs.Review.Allowed() {
					t.Fatal("nil section must resolve allowed (fresh repos behave like GitHub)")
				}
			},
		},
		{
			name:    "empty section resolves allowed",
			payload: "[review]\n",
			check: func(t *testing.T, rs *RepoSettings) {
				t.Helper()
				if rs.Review == nil {
					t.Fatal("review section must decode non-nil")
				}
				if !rs.Review.Allowed() {
					t.Fatal("empty section must resolve allowed")
				}
			},
		},
		{
			name:    "explicit true stays allowed",
			payload: "[review]\nallow_self_approval = true\n",
			check: func(t *testing.T, rs *RepoSettings) {
				t.Helper()
				if !rs.Review.Allowed() {
					t.Fatal("explicit true must resolve allowed")
				}
			},
		},
		{
			name:    "explicit false denies",
			payload: "description = \"meta\"\n[review]\nallow_self_approval = false\n",
			check: func(t *testing.T, rs *RepoSettings) {
				t.Helper()
				if rs.Review.Allowed() {
					t.Fatal("explicit false must resolve denied")
				}
				if rs.Description != "meta" {
					t.Fatalf("description = %q, want coexisting key intact", rs.Description)
				}
			},
		},
		{
			name:    "coexists with features and merged sections",
			payload: "[features]\nissues = false\n[review]\nallow_self_approval = false\n[bundles]\nmin_commits = 5\n",
			check: func(t *testing.T, rs *RepoSettings) {
				t.Helper()
				if rs.Review.Allowed() {
					t.Fatal("explicit false must resolve denied")
				}
				if rs.Features.Resolve().Issues {
					t.Fatal("features section must survive beside [review]")
				}
				if rs.Bundles == nil || rs.Bundles.MinCommits != 5 {
					t.Fatalf("bundles = %+v, want settings value", rs.Bundles)
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

func TestParseRepoSettingsReviewRejects(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr string
	}{
		{name: "unknown key in review", payload: "[review]\nteleport = true\n", wantErr: "unknown key"},
		{name: "wrong type", payload: "[review]\nallow_self_approval = \"no\"\n", wantErr: "repo settings"},
		{name: "bare review key", payload: "review = true\n", wantErr: "repo settings"},
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

func TestRepoSettingsReviewMergeIgnored(t *testing.T) {
	// Review rides the doc (persistence, revisioning, admin writes) but
	// never merges into the host config — the Description/Features
	// precedent: Merge and ValidateAgainst both succeed, and the merged
	// config is the host config on every merged section.
	rs, err := ParseRepoSettings([]byte("[review]\nallow_self_approval = false\n[bundles]\nmin_commits = 5\n"))
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
		t.Fatalf("ValidateAgainst with review: %v", err)
	}
}

func TestAllowSelfApprovalOf(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "empty body defaults allowed", body: "", want: true},
		{name: "whitespace body defaults allowed", body: "  \n", want: true},
		{name: "no section defaults allowed", body: "description = \"hi\"\n", want: true},
		{name: "corrupt body fails open to allowed", body: "[broken\n", want: true},
		{name: "unknown key fails open to allowed", body: "[review]\nteleport = true\n", want: true},
		{name: "explicit true", body: "[review]\nallow_self_approval = true\n", want: true},
		{name: "explicit false", body: "[review]\nallow_self_approval = false\n", want: false},
		{
			name: "explicit false beside other sections",
			body: "[features]\nissues = false\n[review]\nallow_self_approval = false\n",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AllowSelfApprovalOf([]byte(tt.body)); got != tt.want {
				t.Fatalf("AllowSelfApprovalOf = %v, want %v", got, tt.want)
			}
		})
	}
}
