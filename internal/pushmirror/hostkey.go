// hostkey.go — SSH host-key trust harvest (Forgejo #625).
//
// Accept-new trust used to land on ephemeral per-fire disk (the
// known_hosts scratch file, swept with the fire) with no verification
// surface. After a successful SSH push the runner hands the post-push
// file content back, and this file merges the learned lines into the
// secret sidecar (same CAS discipline as other secret writes) and
// derives the presence-style display fields (fingerprint(s),
// first-accepted-at — never key material).
//
// Trust policy (decided + documented here, pinned by test):
//   - merge, don't overwrite — learned lines append to the stored trust;
//   - operator-pinned lines stay authoritative: on a host+keytype
//     conflict (same host, different key bytes) the stored line wins and
//     the learned line is dropped (an operator pin is an explicit trust
//     decision; silent replacement would downgrade it);
//   - fingerprints are DERIVED at read from the merged lines (single
//     source of truth — no second copy to skew); only the
//     first-accepted-at stamp is stored.
//
// Secrets hygiene (law 4/8): learned key lines live ONLY in the secret
// sidecar; logs/errors/views carry the SHA256 fingerprint at most (the
// deploy-key fingerprint precedent); the scrub contract is unchanged —
// no new surface carries key material.
//
// Parsing is string work over known_hosts lines (stdlib only — law 1:
// no x/crypto/ssh client use): blank lines, `#` comments, and `@`
// marker lines (@cert-authority/@revoked) are skipped; anything else
// must be `<hosts> <keytype> <base64> [comment]`.
package pushmirror

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

// hostKeyLine is one parsed known_hosts trust line.
type hostKeyLine struct {
	hosts   string // comma-joined host patterns (or the |1| hash token), verbatim
	keyType string // ssh-ed25519, ssh-rsa, ecdsa-sha2-*, ...
	key     string // base64 key blob, verbatim
	raw     string // the full line, verbatim (preserved on merge)
}

// id keys the dedupe/conflict domain: host + key type. Two lines for
// the same host carrying different key types are independent trust
// (a host may legitimately serve ed25519 + rsa); two lines for the
// same host+keytype with different key bytes are a conflict.
func (l hostKeyLine) id() string { return l.hosts + " " + l.keyType }

// sameKey reports whether two lines carry the same key bytes (the
// trailing comment is cosmetic — key rotation keeps the comment while
// changing bytes, and re-learning keeps bytes while changing it).
func (l hostKeyLine) sameKey(o hostKeyLine) bool { return l.keyType == o.keyType && l.key == o.key }

// parseKnownHostLine parses one known_hosts line; ok=false for blanks,
// comments, @-markers, and malformed lines (a valid key blob must be
// base64-decodable — junk never merges into stored trust).
func parseKnownHostLine(line string) (l hostKeyLine, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "@") {
		return hostKeyLine{}, false
	}
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] == "" || fields[1] == "" || fields[2] == "" {
		return hostKeyLine{}, false
	}
	if _, err := base64.StdEncoding.DecodeString(fields[2]); err != nil {
		if _, err := base64.RawStdEncoding.DecodeString(fields[2]); err != nil {
			return hostKeyLine{}, false
		}
	}
	return hostKeyLine{hosts: fields[0], keyType: fields[1], key: fields[2], raw: line}, true
}

// parseKnownHosts parses content into lines in file order (invalid
// lines dropped — the file is ssh-written, so these are rare).
func parseKnownHosts(content string) []hostKeyLine {
	var out []hostKeyLine
	for _, line := range strings.Split(content, "\n") {
		if l, ok := parseKnownHostLine(line); ok {
			out = append(out, l)
		}
	}
	return out
}

// FingerprintKnownHostKey renders the OpenSSH-style SHA256 fingerprint
// of one known_hosts line ("SHA256:…" over the raw key blob — the same
// `ssh-keygen -l` shape keygen.go uses for deploy keys, so both
// surfaces read alike). ok=false when the line carries no valid key.
func FingerprintKnownHostKey(line string) (fp string, ok bool) {
	l, valid := parseKnownHostLine(line)
	if !valid {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(l.key)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(l.key)
		if err != nil {
			return "", false
		}
	}
	sum := sha256.Sum256(raw)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), true
}

// KnownHostsFingerprints renders the display fingerprints for stored
// trust: one SHA256 per valid line, deduped, in file order,
// comma-joined ("" when nothing is trusted yet). Presence-style —
// safe for views, logs, and the ETag input.
func KnownHostsFingerprints(content string) string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		fp, ok := FingerprintKnownHostKey(line)
		if !ok || seen[fp] {
			continue
		}
		seen[fp] = true
		out = append(out, fp)
	}
	return strings.Join(out, ",")
}

// MergeKnownHosts folds learned lines over stored trust and reports
// whether anything new landed. Stored lines are never dropped and
// never replaced: an exact (host+keytype+key) duplicate is skipped,
// and a host+keytype conflict keeps the stored line (the operator pin
// is authoritative — decided in the #625 design). The stored block is
// preserved verbatim (comments included); learned lines append in
// arrival order.
func MergeKnownHosts(stored, learned string) (merged string, added bool) {
	storedLines := parseKnownHosts(stored)
	byID := make(map[string]hostKeyLine, len(storedLines))
	for _, l := range storedLines {
		if _, dup := byID[l.id()]; !dup {
			byID[l.id()] = l
		}
	}
	var extra []string
	seenLearned := map[string]bool{}
	for _, l := range parseKnownHosts(learned) {
		if seenLearned[l.id()+"\x00"+l.key] {
			continue
		}
		seenLearned[l.id()+"\x00"+l.key] = true
		cur, exists := byID[l.id()]
		switch {
		case exists && cur.sameKey(l):
			continue // already trusted
		case exists:
			continue // conflict: the stored (operator) line wins
		}
		byID[l.id()] = l
		extra = append(extra, l.raw)
		added = true
	}
	if !added {
		return stored, false
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(stored, "\n"))
	if strings.TrimSpace(stored) != "" {
		b.WriteString("\n")
	}
	for _, line := range extra {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String(), true
}

// RecordHostKeyTrust merges post-push known_hosts content into the
// secret sidecar under the usual secret CAS discipline and returns the
// display fingerprints plus whether new trust landed. It skips the
// write when nothing is new (no extra store trips on the steady-state
// path — law 6); a nil/absent secret is a no-op (non-SSH configs never
// reach here — resolveAuth guarantees material for ssh pushes).
//
// first-accepted-at is stamped only on the first learn and preserved
// after (operator-pinned-only trust keeps it empty — the stamp names
// accept-new learning, not explicit pins).
func RecordHostKeyTrust(ctx context.Context, st store.ObjectStore, owner, name, learned string, now time.Time) (fingerprints string, added bool, err error) {
	if strings.TrimSpace(learned) == "" {
		return "", false, nil
	}
	sec, _, err := LoadSecret(ctx, st, owner, name)
	if err != nil {
		return "", false, err
	}
	if sec == nil {
		return "", false, nil
	}
	merged, isNew := MergeKnownHosts(sec.SSHKnownHosts, learned)
	fingerprints = KnownHostsFingerprints(merged)
	if !isNew {
		// Steady state: no write (no extra store trips — law 6). The
		// stamp stays empty for pinned-only trust — it names
		// accept-new learning, not explicit operator pins.
		return fingerprints, false, nil
	}
	sec.SSHKnownHosts = merged
	if sec.SSHKnownHostsAcceptedAt == "" {
		sec.SSHKnownHostsAcceptedAt = now.UTC().Format(time.RFC3339)
	}
	if serr := SaveSecretCAS(ctx, st, owner, name, sec); serr != nil {
		return "", false, fmt.Errorf("pushmirror: record host-key trust: %w", serr)
	}
	return fingerprints, true, nil
}
