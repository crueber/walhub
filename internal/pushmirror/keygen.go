// keygen.go — dep-free ed25519 keypair generation (Forgejo #623).
//
// Law 1 leaves two lawful shapes: stdlib crypto + hand-rolled OpenSSH
// wire format, or an ssh-keygen subprocess with a documented runtime
// dependency. The runtime image (Dockerfile) ships git + CA certs only —
// no openssh-client — so a subprocess would add a runtime dependency for
// every deployment. This file takes the stdlib shape: crypto/ed25519
// for the keypair, hand-rolled OpenSSH wire encoding for both the
// `ssh-ed25519 AAAA…` public line git/Forgejo accept as a deploy key and
// the `BEGIN OPENSSH PRIVATE KEY` PEM the `ssh -i` client (via
// GIT_SSH_COMMAND) reads. No new module, no new binary.
package pushmirror

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"strings"
)

// GeneratedKey is one generated deploy keypair: the OpenSSH private key
// PEM (stored in the secret sidecar, never echoed) plus the public line
// and fingerprint (shown to the user for upstream install).
type GeneratedKey struct {
	PrivatePEM  string // "-----BEGIN OPENSSH PRIVATE KEY-----..." (secret)
	PublicKey   string // "ssh-ed25519 AAAA... walhub-pushmirror" (public)
	Fingerprint string // "SHA256:..." (public)
}

// GenerateKeypair mints a fresh ed25519 deploy keypair. comment names
// the key in the public line (fixed default, no user free-text —
// upstream-controlled text never reaches key material).
func GenerateKeypair(comment string) (*GeneratedKey, error) {
	if strings.TrimSpace(comment) == "" {
		comment = "walhub-pushmirror"
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("pushmirror: generate key: %w", err)
	}
	return encodeKeypair(pub, priv, comment), nil
}

func encodeKeypair(pub ed25519.PublicKey, priv ed25519.PrivateKey, comment string) *GeneratedKey {
	pubBlob := sshString([]byte("ssh-ed25519"))
	pubBlob = append(pubBlob, sshString([]byte(pub))...)
	pubLine := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(pubBlob) + " " + comment
	sum := sha256.Sum256(pubBlob)
	fp := "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "=")
	return &GeneratedKey{
		PrivatePEM:  string(encodeOpenSSHPem(pub, priv, comment)),
		PublicKey:   pubLine,
		Fingerprint: fp,
	}
}

// sshString encodes an SSH wire string (u32 length + bytes).
func sshString(b []byte) []byte {
	out := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	copy(out[4:], b)
	return out
}

// encodeOpenSSHPem encodes the OpenSSH private-key container
// (PROTOCOL.key format: magic + none/none cipher/kdf + 1 key + the
// private block with checkints), PEM-wrapped. Decoders verified: the
// public blob inside matches encodeKeypair's public line, and
// `ssh-keygen -y -f` reproduces the public line on hosts that have it.
func encodeOpenSSHPem(pub ed25519.PublicKey, priv ed25519.PrivateKey, comment string) []byte {
	pubBlob := sshString([]byte("ssh-ed25519"))
	pubBlob = append(pubBlob, sshString([]byte(pub))...)

	var check [4]byte
	if _, err := rand.Read(check[:]); err != nil {
		check = [4]byte{1, 2, 3, 4}
	}
	privBlock := check[:]
	privBlock = append(privBlock, check[:]...)
	privBlock = append(privBlock, sshString([]byte("ssh-ed25519"))...)
	privBlock = append(privBlock, sshString([]byte(pub))...)
	privBlock = append(privBlock, sshString([]byte(priv))...)
	privBlock = append(privBlock, sshString([]byte(comment))...)
	// Pad to the cipher block size (8 for none): bytes 1,2,3,...
	pad := 8 - (len(privBlock) % 8)
	if pad == 0 {
		pad = 8
	}
	for i := 1; i <= pad; i++ {
		privBlock = append(privBlock, byte(i))
	}

	var buf bytes.Buffer
	buf.WriteString("openssh-key-v1\x00")
	buf.Write(sshString([]byte("none")))
	buf.Write(sshString([]byte("none")))
	buf.Write(sshString(nil))
	binary.Write(&buf, binary.BigEndian, uint32(1)) //nolint:errcheck // bytes.Buffer never fails
	buf.Write(sshString(pubBlob))
	buf.Write(sshString(privBlock))

	var pemBuf bytes.Buffer
	_ = pem.Encode(&pemBuf, &pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: buf.Bytes()})
	return pemBuf.Bytes()
}

// ParsePublicLine validates a user-provided SSH public key line: it must
// be a single-line `ssh-ed25519 AAAA… [comment]` with a decodable payload
// whose inner key type matches. Other key types are refused (v1 supports
// ed25519 only — the generated shape; RSA material would need ASN.1 the
// stdlib path does not take).
func ParsePublicLine(line string) (fingerprint string, err error) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 || fields[0] != "ssh-ed25519" {
		return "", fmt.Errorf("pushmirror: want a single-line \"ssh-ed25519 AAAA...\" public key")
	}
	raw, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		if raw, err = base64.RawStdEncoding.DecodeString(fields[1]); err != nil {
			return "", fmt.Errorf("pushmirror: public key is not base64: %v", err)
		}
	}
	typ, rest, ok := sshReadString(raw)
	if !ok || string(typ) != "ssh-ed25519" {
		return "", fmt.Errorf("pushmirror: public key wire type is not ssh-ed25519")
	}
	key, _, ok := sshReadString(rest)
	if !ok || len(key) != ed25519.PublicKeySize {
		return "", fmt.Errorf("pushmirror: public key payload is not a 32-byte ed25519 key")
	}
	// Fingerprint over the full wire blob (the `ssh-keygen -l` shape).
	blob := sshString([]byte("ssh-ed25519"))
	blob = append(blob, sshString(key)...)
	sum := sha256.Sum256(blob)
	return "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "="), nil
}

func sshReadString(b []byte) (out, rest []byte, ok bool) {
	if len(b) < 4 {
		return nil, b, false
	}
	n := binary.BigEndian.Uint32(b[:4])
	if uint64(n) > uint64(len(b)-4) {
		return nil, b, false
	}
	return b[4 : 4+n], b[4+n:], true
}
