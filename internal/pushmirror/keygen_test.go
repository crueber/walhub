// keygen_test.go — dep-free keypair generation + public-line validation.
package pushmirror

import (
	"strings"
	"testing"
)

func TestGenerateKeypair(t *testing.T) {
	k, err := GenerateKeypair("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(k.PrivatePEM, "BEGIN OPENSSH PRIVATE KEY") {
		t.Error("private PEM missing OpenSSH header")
	}
	if !strings.HasPrefix(k.PublicKey, "ssh-ed25519 ") {
		t.Errorf("public line = %q", k.PublicKey)
	}
	if !strings.HasPrefix(k.Fingerprint, "SHA256:") {
		t.Errorf("fingerprint = %q", k.Fingerprint)
	}
	fp, err := ParsePublicLine(k.PublicKey)
	if err != nil {
		t.Fatalf("generated public line rejected: %v", err)
	}
	if fp != k.Fingerprint {
		t.Errorf("fingerprint mismatch: %q vs %q", fp, k.Fingerprint)
	}
	// Uniqueness: two generations differ.
	k2, err := GenerateKeypair("")
	if err != nil {
		t.Fatal(err)
	}
	if k2.PublicKey == k.PublicKey {
		t.Error("two generations produced the same key")
	}
}

func TestParsePublicLineRejects(t *testing.T) {
	for _, bad := range []string{
		"",
		"ssh-rsa AAAA junk",
		"ssh-ed25519",
		"ssh-ed25519 !!!not-base64!!!",
		"ssh-ed25519 aGVsbG8=", // valid base64, wrong wire shape
	} {
		if _, err := ParsePublicLine(bad); err == nil {
			t.Errorf("ParsePublicLine(%q) accepted", bad)
		}
	}
}
