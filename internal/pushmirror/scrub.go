// scrub.go — credential redaction for every error/last_result surface
// (the pull-mirror scrub contract, Forgejo #623): secrets must never
// appear in task logs, errors, or the config sidecar.
package pushmirror

import "strings"

// scrubText redacts credential-shaped secrets before a string reaches
// task logs, errors, or the sidecar.
func scrubText(s string) string {
	out := redactKV(s, "password=")
	out = redactKV(out, "passwd=")
	out = redactKV(out, "token=")
	out = redactKV(out, "secret=")
	out = redactKV(out, "private=")
	// userinfo-shaped secrets (scheme://user:pass@host)
	if i := strings.Index(out, "://"); i >= 0 {
		if j := strings.Index(out[i:], "@"); j >= 0 {
			out = out[:i+3] + "[redacted]@" + out[i+j+1:]
		}
	}
	return out
}

// redactKV cuts `key<value>` at the next delimiter (whitespace, quote,
// semicolon, or end of string).
func redactKV(s, key string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, key)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i+len(key)])
		b.WriteString("[redacted]")
		j := i + len(key)
		for j < len(s) && !strings.ContainsRune(" \t\n\r\"';", rune(s[j])) {
			j++
		}
		s = s[j:]
	}
}

// last4 renders the write-only confirmation hint for stored secret
// material: presence without content ("••••1234" style). Trailing
// whitespace/newlines are trimmed first (PEM blocks end in "-----";
// without the trim the hint would name the armor, not the secret).
// Short secrets still confirm presence without leaking length structure.
func last4(secret string) string {
	secret = strings.TrimRight(secret, " \t\n\r")
	if secret == "" {
		return ""
	}
	if len(secret) <= 4 {
		return "••••"
	}
	return "••••" + secret[len(secret)-4:]
}
