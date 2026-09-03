package sshd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"git.packden.us/crueber/walhub/internal/git"
	gossh "golang.org/x/crypto/ssh"
)

// gitProtocolKey is the context key carrying the client's GIT_PROTOCOL value.
type gitProtocolKey struct{}

func withGitProtocol(ctx context.Context, v string) context.Context {
	return context.WithValue(ctx, gitProtocolKey{}, v)
}

func gitProtocol(ctx context.Context) string {
	v, _ := ctx.Value(gitProtocolKey{}).(string)
	if v == "version=2" {
		return v
	}
	return ""
}

// hostSigner resolves the host key: explicit bytes, then the file at
// HostKeyPath, then generation of a fresh ed25519 key persisted at that path.
// The generated key is what makes a zero-config SSH boot possible: clients
// pin it on first connect (TOFU), like ssh-keygen -A does on real hosts.
func hostSigner(cfg Config) (gossh.Signer, error) {
	if len(cfg.HostKey) > 0 {
		s, err := gossh.ParsePrivateKey(cfg.HostKey)
		if err != nil {
			return nil, fmt.Errorf("server.ssh host key: %w", err)
		}
		return s, nil
	}
	if cfg.HostKeyPath != "" {
		if raw, err := os.ReadFile(cfg.HostKeyPath); err == nil {
			s, err := gossh.ParsePrivateKey(raw)
			if err != nil {
				return nil, fmt.Errorf("server.ssh host key %s: %w", cfg.HostKeyPath, err)
			}
			return s, nil
		}
		// Missing file → generate and persist below.
		_, gen, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("server.ssh host key generation: %w", err)
		}
		block, err := gossh.MarshalPrivateKey(gen, "walhub ssh host key")
		if err != nil {
			return nil, fmt.Errorf("server.ssh host key marshal: %w", err)
		}
		raw := pem.EncodeToMemory(block)
		if err := os.MkdirAll(filepath.Dir(cfg.HostKeyPath), 0o700); err != nil {
			return nil, fmt.Errorf("server.ssh host key dir: %w", err)
		}
		if err := os.WriteFile(cfg.HostKeyPath, raw, 0o600); err != nil {
			return nil, fmt.Errorf("server.ssh host key write: %w", err)
		}
		s, err := gossh.ParsePrivateKey(raw)
		if err != nil {
			return nil, fmt.Errorf("server.ssh host key %s: %w", cfg.HostKeyPath, err)
		}
		return s, nil
	}
	// No path configured at all: ephemeral in-memory key. Clients will see a
	// new host key per boot — acceptable for tests, discouraged in production
	// (17_ssh.md recommends setting host_key).
	_, gen, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("server.ssh host key generation: %w", err)
	}
	return gossh.NewSignerFromKey(gen)
}

// SplitCommand splits a client command string into shell words, honoring
// single quotes, double quotes, and backslash escapes — exactly the shapes
// git's SSH transport produces (`git-upload-pack '/owner/repo.git'`).
func SplitCommand(cmd string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	i := 0
	for i < len(cmd) {
		c := cmd[i]
		switch {
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
			i++
		case c == '\'':
			inWord = true
			i++
			end := strings.IndexByte(cmd[i:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("unterminated single quote")
			}
			cur.WriteString(cmd[i : i+end])
			i += end + 1
		case c == '"':
			inWord = true
			i++
			for i < len(cmd) && cmd[i] != '"' {
				if cmd[i] == '\\' && i+1 < len(cmd) && (cmd[i+1] == '"' || cmd[i+1] == '\\') {
					i++
				}
				cur.WriteByte(cmd[i])
				i++
			}
			if i >= len(cmd) {
				return nil, fmt.Errorf("unterminated double quote")
			}
			i++
		case c == '\\':
			if i+1 >= len(cmd) {
				return nil, fmt.Errorf("dangling escape")
			}
			inWord = true
			cur.WriteByte(cmd[i+1])
			i += 2
		default:
			inWord = true
			cur.WriteByte(c)
			i++
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// ParseGitCommand validates and maps one client command onto a verb and repo:
// strictly `git-upload-pack|git-receive-pack '<owner/repo[.git]>'`. Anything
// else — option-bearing argv (the classic SSH option-injection), extra words,
// unknown verbs, invalid repo paths — is refused before any transport work.
func ParseGitCommand(cmd string) (verb string, id git.RepoId, err error) {
	words, err := SplitCommand(cmd)
	if err != nil {
		return "", git.RepoId{}, err
	}
	if len(words) != 2 {
		return "", git.RepoId{}, fmt.Errorf("expected exactly: git-upload-pack|git-receive-pack '<owner/repo.git>', got %d argument(s)", len(words))
	}
	verb = words[0]
	if verb != "git-upload-pack" && verb != "git-receive-pack" {
		return "", git.RepoId{}, fmt.Errorf("command not served: %q (only git-upload-pack and git-receive-pack)", verb)
	}
	path := words[1]
	if strings.HasPrefix(path, "-") {
		return "", git.RepoId{}, fmt.Errorf("option-looking arguments are refused")
	}
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		return "", git.RepoId{}, fmt.Errorf("empty repository path")
	}
	id, err = git.ParseRepoId(path)
	if err != nil {
		return "", git.RepoId{}, err
	}
	return verb, id, nil
}
