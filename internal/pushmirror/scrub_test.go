// scrub_test.go — redaction: secrets never reach logs, errors, or the sidecar.
package pushmirror

import (
	"strings"
	"testing"
)

func TestScrubText(t *testing.T) {
	cases := []struct{ in, notWant string }{
		{"clone failed: password=s3cret boom", "s3cret"},
		{"token=abc123 failed", "abc123"},
		{"https://user:hunter2@host/x failed", "hunter2"},
		{"private key material secret=topsecret here", "topsecret"},
	}
	for _, c := range cases {
		if got := scrubText(c.in); strings.Contains(got, c.notWant) {
			t.Errorf("scrubText(%q) leaks %q: %q", c.in, c.notWant, got)
		}
	}
	if got := scrubText("clean line"); got != "clean line" {
		t.Errorf("clean line rewritten: %q", got)
	}
	if got := scrubText("https://host/x ok"); !strings.Contains(got, "https://host/x") {
		t.Errorf("public URL mangled: %q", got)
	}
}

func TestLast4(t *testing.T) {
	if got := last4(""); got != "" {
		t.Errorf("empty secret hint = %q, want empty", got)
	}
	if got := last4("abcdefgh"); got != "••••efgh" {
		t.Errorf("last4 = %q", got)
	}
	if got := last4("abc"); strings.Contains(got, "abc") {
		t.Errorf("short secret leaked: %q", got)
	}
	if got := last4("abcdefgh\n"); got != "••••efgh" {
		t.Errorf("trailing newline names the armor, not the secret: %q", got)
	}
}
