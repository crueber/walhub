// hostkey_test.go — Forgejo #625: known_hosts parse/merge/dedupe,
// fingerprint display, view fields, and the record CAS.
package pushmirror

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fixtureHostLine builds a known_hosts line for host from a fresh
// deploy keypair and returns the line plus the keypair (the line's
// fingerprint must equal the keypair fingerprint — same wire blob).
func fixtureHostLine(t *testing.T, host string) (line string, key *GeneratedKey) {
	t.Helper()
	key, err := GenerateKeypair("harvest-test")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(key.PublicKey, " ")
	if len(parts) < 2 || parts[0] != "ssh-ed25519" {
		t.Fatalf("public line = %q", key.PublicKey)
	}
	return host + " " + parts[0] + " " + parts[1], key
}

func TestParseKnownHostLineSkipsNoise(t *testing.T) {
	for _, bad := range []string{
		"", "   ", "# a comment", "   # indented comment",
		"@cert-authority example.com ssh-ed25519 AAAA",
		"@revoked example.com ssh-ed25519 AAAA",
		"onlyone", "two fields",
		"example.com ssh-ed25519 not-base64!!!",
		"example.com ssh-ed25519",
	} {
		if _, ok := parseKnownHostLine(bad); ok {
			t.Errorf("parseKnownHostLine(%q) ok, want skip", bad)
		}
	}
	line, _ := fixtureHostLine(t, "example.com")
	l, ok := parseKnownHostLine(line + " trailing comment")
	if !ok || l.hosts != "example.com" || l.keyType != "ssh-ed25519" || l.key == "" {
		t.Errorf("parse = %+v,%v", l, ok)
	}
	// Hashed-host tokens (|1|…) carry no spaces — one opaque field.
	if _, ok := parseKnownHostLine("|1|salt|hash ssh-ed25519 " + l.key); !ok {
		t.Error("hashed-host line rejected")
	}
}

func TestFingerprintMatchesKeygenHelper(t *testing.T) {
	line, key := fixtureHostLine(t, "example.com")
	fp, ok := FingerprintKnownHostKey(line)
	if !ok {
		t.Fatal("valid line rejected")
	}
	if fp != key.Fingerprint {
		t.Errorf("known_hosts fingerprint = %q, keygen fingerprint = %q (same blob, must match)", fp, key.Fingerprint)
	}
	if !strings.HasPrefix(fp, "SHA256:") || len(fp) != len("SHA256:")+43 {
		t.Errorf("fingerprint shape = %q", fp)
	}
	if _, ok := FingerprintKnownHostKey("# comment"); ok {
		t.Error("comment fingerprinted")
	}
	if _, ok := FingerprintKnownHostKey("example.com ssh-ed25519 !!!"); ok {
		t.Error("bad blob fingerprinted")
	}
}

func TestKnownHostsFingerprintsDedupes(t *testing.T) {
	line, key := fixtureHostLine(t, "example.com")
	other, otherKey := fixtureHostLine(t, "mirror.example")
	got := KnownHostsFingerprints("# comment\n\n" + line + "\n" + line + " dup-comment\n" + other + "\n")
	want := key.Fingerprint + "," + otherKey.Fingerprint
	if got != want {
		t.Errorf("fingerprints = %q, want %q", got, want)
	}
	if KnownHostsFingerprints("# nothing\n\n") != "" {
		t.Error("empty trust must fingerprint empty")
	}
}

func TestMergeKnownHostsAppendsAndDedupes(t *testing.T) {
	line, _ := fixtureHostLine(t, "example.com")
	other, _ := fixtureHostLine(t, "mirror.example")

	// Empty stored: learned becomes the trust (trailing newline kept).
	merged, added := MergeKnownHosts("", line+"\n")
	if !added || strings.TrimSpace(merged) != strings.TrimSpace(line) {
		t.Errorf("merge into empty = %q,%v", merged, added)
	}
	// Exact re-learn: no-op (comment-insensitive — same key bytes).
	merged2, added2 := MergeKnownHosts(merged, line+" re-learned-comment\n")
	if added2 || merged2 != merged {
		t.Errorf("re-learn must be a no-op: %q,%v", merged2, added2)
	}
	// New host appends; stored block (comments included) preserved verbatim.
	stored := "# operator pin\n" + line + "\n"
	merged3, added3 := MergeKnownHosts(stored, other+"\n")
	if !added3 || !strings.HasPrefix(merged3, stored) || !strings.Contains(merged3, strings.TrimSpace(other)) {
		t.Errorf("append merge = %q,%v", merged3, added3)
	}
	// Empty learned: no-op.
	if m, a := MergeKnownHosts(stored, "\n# nothing\n"); a || m != stored {
		t.Errorf("empty learned must be a no-op: %q,%v", m, a)
	}
}

func TestMergeKnownHostsConflictKeepsOperatorPin(t *testing.T) {
	// Same host+keytype, different key bytes: the stored (operator)
	// line wins, the learned line is dropped — never overwrite.
	pinned, _ := fixtureHostLine(t, "example.com")
	learned, _ := fixtureHostLine(t, "example.com")
	merged, added := MergeKnownHosts(pinned+"\n", learned+"\n")
	if added {
		t.Error("conflicting learn must not count as added")
	}
	if strings.Contains(merged, strings.Fields(learned)[2]) {
		t.Error("learned key replaced the operator pin")
	}
	if strings.TrimSpace(merged) != strings.TrimSpace(pinned) {
		t.Errorf("stored pin not preserved: %q", merged)
	}
	// Different key TYPES for the same host are independent trust.
	rsaLine := "example.com ssh-rsa " + strings.Fields(learned)[2]
	merged2, added2 := MergeKnownHosts(pinned+"\n", rsaLine+"\n")
	if !added2 || !strings.Contains(merged2, "ssh-rsa") {
		t.Errorf("cross-type learn must append: %q,%v", merged2, added2)
	}
}

func TestRecordHostKeyTrustLearnThenSteady(t *testing.T) {
	ctx := context.Background()
	st := testStore()
	line, key := fixtureHostLine(t, "example.com")
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	if err := SaveSecret(ctx, st, "o", "r", &Secret{AuthKind: AuthSSH, SSHPrivateKey: "k"}); err != nil {
		t.Fatal(err)
	}
	fp, added, err := RecordHostKeyTrust(ctx, st, "o", "r", line+"\n", now)
	if err != nil || !added || fp != key.Fingerprint {
		t.Fatalf("record = %q,%v,%v", fp, added, err)
	}
	sec, ver1, err := LoadSecret(ctx, st, "o", "r")
	if err != nil || sec == nil {
		t.Fatal(err)
	}
	if !strings.Contains(sec.SSHKnownHosts, strings.TrimSpace(line)) {
		t.Errorf("learned line not in secret: %q", sec.SSHKnownHosts)
	}
	if sec.SSHKnownHostsAcceptedAt != now.Format(time.RFC3339) {
		t.Errorf("accepted_at = %q", sec.SSHKnownHostsAcceptedAt)
	}
	// Second identical harvest: steady state, no write (version stable).
	later := now.Add(time.Hour)
	fp2, added2, err := RecordHostKeyTrust(ctx, st, "o", "r", line+"\n", later)
	if err != nil || added2 || fp2 != fp {
		t.Fatalf("steady record = %q,%v,%v", fp2, added2, err)
	}
	_, ver2, _ := LoadSecret(ctx, st, "o", "r")
	if ver1 != ver2 {
		t.Error("steady-state harvest must not rewrite the sidecar")
	}
	sec2, _, _ := LoadSecret(ctx, st, "o", "r")
	if sec2.SSHKnownHostsAcceptedAt != now.Format(time.RFC3339) {
		t.Error("first-accepted-at must be preserved, not re-stamped")
	}
}

func TestRecordHostKeyTrustNoops(t *testing.T) {
	ctx := context.Background()
	st := testStore()
	// Empty learned: no-op even with a secret present.
	if err := SaveSecret(ctx, st, "o", "r", &Secret{AuthKind: AuthSSH, SSHPrivateKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if fp, added, err := RecordHostKeyTrust(ctx, st, "o", "r", "\n", time.Now()); err != nil || added || fp != "" {
		t.Errorf("empty learn = %q,%v,%v", fp, added, err)
	}
	// Absent secret: no-op, no error (non-SSH configs never reach here).
	if fp, added, err := RecordHostKeyTrust(ctx, st, "o", "missing", "h ssh-ed25519 AAAA\n", time.Now()); err != nil || added || fp != "" {
		t.Errorf("missing secret = %q,%v,%v", fp, added, err)
	}
}

func TestViewOfHostKeyStatus(t *testing.T) {
	now := time.Now()
	doc := &Doc{Version: 1, UpstreamURL: "ssh://example.com/o/r.git", AuthKind: AuthSSH}
	// No secret: no status.
	if v := ViewOf(doc, nil, now); v.HostKeyFingerprint != "" || v.HostKeyAcceptedAt != "" {
		t.Errorf("nil-secret view = %+v", v)
	}
	line, key := fixtureHostLine(t, "example.com")
	sec := &Secret{AuthKind: AuthSSH, SSHPrivateKey: "k",
		SSHKnownHosts: line + "\n", SSHKnownHostsAcceptedAt: "2026-09-16T12:00:00Z"}
	v := ViewOf(doc, sec, now)
	if v.HostKeyFingerprint != key.Fingerprint || v.HostKeyAcceptedAt != "2026-09-16T12:00:00Z" {
		t.Errorf("view = %+v", v)
	}
	// Pre-#625 pinned sidecar (no stamp): fingerprint surfaces, stamp empty.
	sec2 := &Secret{AuthKind: AuthSSH, SSHPrivateKey: "k", SSHKnownHosts: line + "\n"}
	v2 := ViewOf(doc, sec2, now)
	if v2.HostKeyFingerprint != key.Fingerprint || v2.HostKeyAcceptedAt != "" {
		t.Errorf("pinned-only view = %+v", v2)
	}
	// The view is presence-style: key material never appears on the wire.
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), strings.Fields(line)[2]) {
		t.Error("view leaks key material")
	}
}

func TestScrubIgnoresHostKeySurfaces(t *testing.T) {
	// Fingerprints are display-safe: scrubbing must pass them through
	// (narration interpolates the fingerprint, never the line).
	line, key := fixtureHostLine(t, "example.com")
	fp, _ := FingerprintKnownHostKey(line)
	if fp != key.Fingerprint {
		t.Fatal("fixture mismatch")
	}
	msg := "learned host key " + fp + " (pinned for future syncs)"
	if scrubText(msg) != msg {
		t.Errorf("scrub mangled the fingerprint narration: %q", scrubText(msg))
	}
}
