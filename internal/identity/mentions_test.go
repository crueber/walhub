package identity

import (
	"testing"
)

func TestParseMentions(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		users []string
		teams []string
	}{
		{name: "empty", body: "", users: []string{}, teams: []string{}},
		{name: "plain", body: "no mentions here", users: []string{}, teams: []string{}},
		{
			name:  "user",
			body:  "ping @Carol@Example.COM please",
			users: []string{"carol@example.com"}, teams: []string{},
		},
		{
			name:  "team",
			body:  "cc @Acme/Backend for review",
			users: []string{}, teams: []string{"acme/backend"},
		},
		{
			name:  "dedup-sorted",
			body:  "@zed@example.com and @amy@example.com and @zed@example.com",
			users: []string{"amy@example.com", "zed@example.com"}, teams: []string{},
		},
		{
			name:  "trailing-punct",
			body:  "see @acme/backend.",
			users: []string{}, teams: []string{"acme/backend"},
		},
		{
			name:  "fence-skipped",
			body:  "hi @amy@example.com\n```\n@zed@example.com\n```\nafter",
			users: []string{"amy@example.com"}, teams: []string{},
		},
		{
			name:  "inline-skipped",
			body:  "use `@zed@example.com` not @amy@example.com",
			users: []string{"amy@example.com"}, teams: []string{},
		},
		{
			name:  "email-addr-not-mention",
			body:  "mail me at jane@example.com",
			users: []string{}, teams: []string{},
		},
		{
			name:  "invalid-team-shape",
			body:  "hi @UPPER/has here and @/empty",
			users: []string{}, teams: []string{"upper/has"},
		},
		{
			name:  "at-start-and-end",
			body:  "@amy@example.com leads, thanks @zed@example.com",
			users: []string{"amy@example.com", "zed@example.com"}, teams: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			users, teams := ParseMentions(tc.body)
			if len(users) != len(tc.users) || len(teams) != len(tc.teams) {
				t.Fatalf("ParseMentions(%q) = %v/%v, want %v/%v", tc.body, users, teams, tc.users, tc.teams)
			}
			for i := range users {
				if users[i] != tc.users[i] {
					t.Fatalf("ParseMentions(%q) users = %v, want %v", tc.body, users, tc.users)
				}
			}
			for i := range teams {
				if teams[i] != tc.teams[i] {
					t.Fatalf("ParseMentions(%q) teams = %v, want %v", tc.body, teams, tc.teams)
				}
			}
		})
	}
}

func TestParseMentionsCap(t *testing.T) {
	body := ""
	for i := 0; i < 70; i++ {
		if body != "" {
			body += " "
		}
		body += "@user" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "@example.com"
	}
	users, _ := ParseMentions(body)
	if len(users) != MaxMentionsPerBody {
		t.Fatalf("capped at %d, got %d", MaxMentionsPerBody, len(users))
	}
}
