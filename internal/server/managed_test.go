package server

import (
	"context"
	"io"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
)

// Server-managed refs (docs/features/03 §3: refs/pull/**) are refused in
// the push pipeline before ingest, on every transport: the report carries
// plain `ng` lines naming the built-in rule, and refused-only pushes never
// reach connectivity or the WAL funnel.
func TestPushPipelineManagedRefs(t *testing.T) {
	zero := strings.Repeat("0", 40)
	oid := "1111111111111111111111111111111111111111"

	managedOnly := func(t *testing.T) {
		t.Helper()
		eng := &fakeEngine{exists: true, placement: Placement{Serve: true}}
		s := sshGateServer(t, eng, nil)
		root := t.TempDir()
		ctx := context.WithValue(context.Background(), repoRootKey{}, root)
		// Pure-delete push of a managed ref: no pack bytes needed.
		body := git.Pkt(oid + " " + zero + " refs/pull/42/head\x00report-status\n")
		body = append(body, git.Flush()...)

		var out strings.Builder
		id := mustRepoID(t, "o/r")
		if err := s.SSHReceivePack(ctx, id, "ada", strings.NewReader(string(body)), &out, io.Discard); err != nil {
			t.Fatalf("receive: %v", err)
		}
		report := out.String()
		if !strings.Contains(report, "unpack ok") {
			t.Fatalf("report = %q", report)
		}
		if !strings.Contains(report, "ng refs/pull/42/head rejected by rule 'pull-refs-managed'") {
			t.Fatalf("managed refusal missing: %q", report)
		}
		if eng.published != 0 {
			t.Fatalf("refused-only push must not publish (publishes = %d)", eng.published)
		}
	}

	mixed := func(t *testing.T) {
		t.Helper()
		eng := &fakeEngine{exists: true, placement: Placement{Serve: true}}
		s := sshGateServer(t, eng, nil)
		root := t.TempDir()
		ctx := context.WithValue(context.Background(), repoRootKey{}, root)
		body := git.Pkt(oid + " " + zero + " refs/pull/42/head\x00report-status\n")
		body = append(body, git.Pkt(oid+" "+zero+" refs/heads/topic\n")...)
		body = append(body, git.Flush()...)

		var out strings.Builder
		id := mustRepoID(t, "o/r")
		if err := s.SSHReceivePack(ctx, id, "ada", strings.NewReader(string(body)), &out, io.Discard); err != nil {
			t.Fatalf("receive: %v", err)
		}
		report := out.String()
		if !strings.Contains(report, "ng refs/pull/42/head rejected by rule 'pull-refs-managed'") {
			t.Fatalf("managed refusal missing: %q", report)
		}
		if eng.published != 1 {
			t.Fatalf("allowed subset must publish (publishes = %d)", eng.published)
		}
	}

	t.Run("managed-only", managedOnly)
	t.Run("mixed", mixed)
}

// The managed-namespace predicate is a pure string rule (no feature
// import in core): pin its boundary.
func TestIsManagedRefBoundary(t *testing.T) {
	for ref, want := range map[string]bool{
		"refs/pull/1/head":  true,
		"refs/pull/99/head": true,
		"refs/pull/":        true,
		"refs/pull":         false,
		"refs/heads/pull/1": false,
		"refs/heads/main":   false,
		"refs/tags/v1":      false,
	} {
		if got := git.IsManagedRef(ref); got != want {
			t.Fatalf("IsManagedRef(%q) = %v, want %v", ref, got, want)
		}
	}
	if git.ManagedRefReason() != "rejected by rule 'pull-refs-managed'" {
		t.Fatalf("reason = %q", git.ManagedRefReason())
	}
}
