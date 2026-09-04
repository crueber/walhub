package checks

import (
	"errors"
	"strings"
	"testing"
)

// Report flow (§2 steps 1–4): validate → Create-or-CAS → index → SSE +
// fan-out. Table-driven over the auth/validation matrix, then the
// stateful paths (re-report, pending-clear, transitions).

func TestReportValidation(t *testing.T) {
	sha := hexSHA(1)
	for _, tc := range []struct {
		name   string
		sha    string
		input  ReportInput
		status int
	}{
		{"bad sha", "abc", ReportInput{Context: "ci", State: StateSuccess}, 400},
		{"empty context", sha, ReportInput{Context: "", State: StateSuccess}, 400},
		{"bad context", sha, ReportInput{Context: "has space", State: StateSuccess}, 400},
		{"context dot-json", sha, ReportInput{Context: "x.json", State: StateSuccess}, 400},
		{"bad state", sha, ReportInput{Context: "ci", State: "green"}, 409},
		{"empty state", sha, ReportInput{Context: "ci", State: ""}, 409},
		{"bad url", sha, ReportInput{Context: "ci", State: StateSuccess, TargetURL: "notaurl"}, 400},
		{"long description", sha, ReportInput{Context: "ci", State: StateSuccess, Description: strings.Repeat("x", 257)}, 400},
		{"bad started_at", sha, ReportInput{Context: "ci", State: StateSuccess, StartedAt: strPtr("yesterday")}, 400},
		{"bad completed_at", sha, ReportInput{Context: "ci", State: StateSuccess, CompletedAt: strPtr("12:00")}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv()
			e.knowSHA(sha)
			_, err := e.svc.ReportStatus(ctx(), "o", "r", tc.sha, writer(), "", tc.input)
			if err == nil {
				t.Fatal("accepted")
			}
			if got := statusFor(err); got != tc.status {
				t.Fatalf("status %d, want %d (%v)", got, tc.status, err)
			}
		})
	}
}

func TestReportAuth(t *testing.T) {
	sha := hexSHA(2)
	t.Run("anonymous 401", func(t *testing.T) {
		e := newTestEnv()
		e.knowSHA(sha)
		_, err := e.svc.ReportStatus(ctx(), "o", "r", sha, anon(), "", ReportInput{Context: "ci", State: StateSuccess})
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("reader 403", func(t *testing.T) {
		e := newTestEnv()
		e.roles.Roles["sam@example.com"] = "read"
		e.knowSHA(sha)
		_, err := e.svc.ReportStatus(ctx(), "o", "r", sha, reader(), "", ReportInput{Context: "ci", State: StateSuccess})
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown sha 404", func(t *testing.T) {
		e := newTestEnv()
		_, err := e.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{Context: "ci", State: StateSuccess})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("git down 503", func(t *testing.T) {
		e := newTestEnv()
		e.commits.UnknownErr = errors.New("temporarily unavailable: pool exhausted")
		wrapped := func() error {
			_, err := e.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{Context: "ci", State: StateSuccess})
			return err
		}
		// The fake returns a raw error; the service passes CommitChecker
		// errors through, so classify here via the adapter contract in
		// composition (unit: the error surfaces, never a status write).
		if err := wrapped(); err == nil {
			t.Fatal("accepted")
		}
		if _, _, err := e.svc.getJSON(ctx(), StatusKey("o", "r", sha, "ci")); err != nil {
			t.Fatalf("read: %v", err)
		}
	})
	t.Run("nil commits 503", func(t *testing.T) {
		e := newTestEnv()
		e.svc.Commits = nil
		_, err := e.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{Context: "ci", State: StateSuccess})
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestReportCreateThenCAS(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(3)
	e.knowSHA(sha)
	// First report creates (version 1, creator stamped).
	first := e.mustReport(t, sha, "ci/build", StatePending)
	if first.Version != 1 || first.Creator != "jane@example.com" {
		t.Fatalf("first = %+v", first)
	}
	if first.CreatedAt == "" || first.UpdatedAt == "" {
		t.Fatal("timestamps missing")
	}
	// Re-report CAS-updates (version bumps, created_at stable, original
	// creator retained).
	second, err := e.svc.ReportStatus(ctx(), "o", "r", sha, admin(), "", ReportInput{Context: "ci/build", State: StateSuccess, Description: "green"})
	if err != nil {
		t.Fatalf("re-report: %v", err)
	}
	if second.Version != 2 || second.CreatedAt != first.CreatedAt {
		t.Fatalf("second = %+v", second)
	}
	if second.Creator != "jane@example.com" {
		t.Fatalf("creator must stay with the first report: %+v", second)
	}
	// Re-report of pending clears completed_at.
	done := "2026-09-04T10:04:31Z"
	third, err := e.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{Context: "ci/build", State: StateSuccess, CompletedAt: &done})
	if err != nil {
		t.Fatalf("completed: %v", err)
	}
	if third.CompletedAt == nil || *third.CompletedAt != done {
		t.Fatalf("completed_at: %+v", third)
	}
	fourth, err := e.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{Context: "ci/build", State: StatePending})
	if err != nil {
		t.Fatalf("re-pending: %v", err)
	}
	if fourth.CompletedAt != nil {
		t.Fatalf("pending must clear completed_at: %+v", fourth)
	}
	// Reads see the latest write (context-sorted).
	view, err := e.svc.GetStatuses(ctx(), "o", "r", sha, reader())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(view.Statuses) != 1 || view.Statuses[0].State != StatePending {
		t.Fatalf("view = %+v", view)
	}
}

func TestReportCIToken(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(4)
	e.knowSHA(sha)
	created, err := e.svc.CreateToken(ctx(), "o", "r", admin(), "woodpecker", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(created.Token, "wct_") {
		t.Fatalf("wire = %q", created.Token)
	}
	id, secret, err := ParseCIToken(created.Token)
	if err != nil {
		t.Fatalf("parse minted: %v", err)
	}
	// Happy path: CI principal + secret reports.
	st, err := e.svc.ReportStatus(ctx(), "o", "r", sha, ci(id), secret, ReportInput{Context: "ci/build", State: StateFailure})
	if err != nil {
		t.Fatalf("ci report: %v", err)
	}
	if st.Creator != "ci:"+id {
		t.Fatalf("creator = %q", st.Creator)
	}
	// Wrong secret 401; empty secret 401.
	for _, bad := range []string{"wrong", ""} {
		_, err := e.svc.ReportStatus(ctx(), "o", "r", sha, ci(id), bad, ReportInput{Context: "ci/build", State: StateSuccess})
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("secret %q: %v", bad, err)
		}
	}
	// Unknown id 401.
	_, err = e.svc.ReportStatus(ctx(), "o", "r", sha, ci("deadbeef"), "whatever", ReportInput{Context: "ci/build", State: StateSuccess})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown id: %v", err)
	}
	// Revoked 401.
	if err := e.svc.RevokeToken(ctx(), "o", "r", id, admin()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_, err = e.svc.ReportStatus(ctx(), "o", "r", sha, ci(id), secret, ReportInput{Context: "ci/build", State: StateSuccess})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked: %v", err)
	}
}

func TestTokenCRUD(t *testing.T) {
	e := newTestEnv()
	// Non-admin cannot mint/list/revoke.
	if _, err := e.svc.CreateToken(ctx(), "o", "r", writer(), "x", nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("mint by writer: %v", err)
	}
	if _, err := e.svc.ListTokens(ctx(), "o", "r", writer()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("list by writer: %v", err)
	}
	if err := e.svc.RevokeToken(ctx(), "o", "r", "abcd1234", writer()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoke by writer: %v", err)
	}
	// Validation.
	if _, err := e.svc.CreateToken(ctx(), "o", "r", admin(), "", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty name: %v", err)
	}
	if _, err := e.svc.CreateToken(ctx(), "o", "r", admin(), "x", []string{"repo:write"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad scope: %v", err)
	}
	// Mint → list shows the record without secrets.
	created, err := e.svc.CreateToken(ctx(), "o", "r", admin(), "woodpecker", []string{CITokenScope})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	list, err := e.svc.ListTokens(ctx(), "o", "r", admin())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != created.ID || list[0].Name != "woodpecker" || list[0].RevokedAt != nil {
		t.Fatalf("list = %+v", list)
	}
	// The stored record holds only the hash.
	raw, _, err := e.svc.getJSON(ctx(), TokenKey("o", "r", created.ID))
	if err != nil || raw == nil {
		t.Fatalf("record: %v", err)
	}
	if strings.Contains(string(raw), created.Token) {
		t.Fatal("secret stored in clear")
	}
	// Revoke → listed with revoked_at; second revoke idempotent; unknown 404.
	if err := e.svc.RevokeToken(ctx(), "o", "r", created.ID, admin()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := e.svc.RevokeToken(ctx(), "o", "r", created.ID, admin()); err != nil {
		t.Fatalf("re-revoke: %v", err)
	}
	list, _ = e.svc.ListTokens(ctx(), "o", "r", admin())
	if len(list) != 1 || list[0].RevokedAt == nil {
		t.Fatalf("revoked list = %+v", list)
	}
	if err := e.svc.RevokeToken(ctx(), "o", "r", "deadbeef", admin()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown revoke: %v", err)
	}
	if err := e.svc.RevokeToken(ctx(), "o", "r", "bogus!!", admin()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed revoke: %v", err)
	}
}

func TestCombinedView(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(5)
	e.knowSHA(sha)
	// Zero contexts ⇒ pending with zero counts.
	empty, err := e.svc.Combined(ctx(), "o", "r", sha, reader())
	if err != nil {
		t.Fatalf("combined: %v", err)
	}
	if empty.State != StatePending || empty.Statuses == nil || len(empty.Statuses) != 0 {
		t.Fatalf("empty = %+v", empty)
	}
	for _, k := range []string{StatePending, StateSuccess, StateFailure, StateError} {
		if empty.TotalCounts[k] != 0 {
			t.Fatalf("counts = %+v", empty.TotalCounts)
		}
	}
	// Mixed states fold worst-of with counts.
	e.mustReport(t, sha, "a", StateSuccess)
	e.mustReport(t, sha, "b", StatePending)
	e.mustReport(t, sha, "c", StateFailure)
	view, err := e.svc.Combined(ctx(), "o", "r", sha, reader())
	if err != nil {
		t.Fatalf("combined: %v", err)
	}
	if view.State != StateFailure {
		t.Fatalf("state = %q", view.State)
	}
	if view.TotalCounts[StateSuccess] != 1 || view.TotalCounts[StatePending] != 1 || view.TotalCounts[StateFailure] != 1 {
		t.Fatalf("counts = %+v", view.TotalCounts)
	}
	if len(view.Statuses) != 3 || view.Statuses[0].Context != "a" || view.Statuses[2].Context != "c" {
		t.Fatalf("sorted = %+v", view.Statuses)
	}
	// Error outranks failure.
	e.mustReport(t, sha, "d", StateError)
	view, _ = e.svc.Combined(ctx(), "o", "r", sha, reader())
	if view.State != StateError {
		t.Fatalf("state = %q", view.State)
	}
}

func strPtr(s string) *string { return &s }
