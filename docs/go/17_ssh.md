# 17 — SSH transport (`internal/sshd`)
> Source: walgit parity goal (issue #1) with Gitea/Forgejo prior art (`modules/ssh/ssh.go`) · Status: normative for the walhub Go implementation.

`internal/sshd` serves git over SSH: the standard transport in both directions —
`git-upload-pack` for clone/fetch (out) and `git-receive-pack` for push (in). It is built
directly on `golang.org/x/crypto/ssh` (the one Law-1 amendment; hand-rolling SSH is not on the
table, and this is the same primitive Gitea and Forgejo build on). The package owns the SSH wire
(auth, session channels, command parsing); the git pipeline stays in `internal/git` and
`internal/wal` — the server implements the transport interface, so SSH sessions run the EXACT
pipeline the HTTP handlers run.

## 1. Wire behavior

One exec request per session, strictly:

```
git-upload-pack   '<owner/repo[.git]>'
git-receive-pack  '<owner/repo[.git]>'
```

- The command string is split with a shell-word splitter (single/double quotes, backslash) —
  `sshd.SplitCommand` — and mapped by `ParseGitCommand`: exactly two words, verb whitelisted, the
  repo path may carry a leading `/` and a trailing `.git`, and option-looking argv (the classic
  SSH option-injection) is refused. Repo paths go through `git.ParseRepoId`.
- `GIT_PROTOCOL=version=2` env (SSH `SendEnv`) is accepted and forwarded to upload-pack; receive-pack
  always speaks v0 (protocol v2 does not cover push — git itself ignores v2 for push).
- No PTY, no shell, no subsystem (sftp), no channel forwarding: session channels carry exactly one
  exec, then an exit-status request, then close. Interactive attempts get a stderr notice and exit 1.

## 2. Fetch vs push framing (the load-bearing difference)

- **Fetch (upload-pack)**: HTTP framing gives upload-pack a complete request body; over SSH the
  client never closes its side. `Layer.UploadPackSSH` therefore runs upload-pack WITHOUT
  `--stateless-rpc` (stateless-rpc waits for stdin EOF — the hang this variant exists to avoid).
  The feeder goroutine is released when the child exits, not awaited.
- **Push (receive-pack)**: `git send-pack` also keeps the channel open until it sees the status
  report, so nothing may read the channel to EOF. The server:
  1. opens the repo (auto-create rules identical to HTTP) and writes the **ref advertisement** to
     the channel FIRST — over SSH, receive-pack opens with the server's advertisement; a client
     sends nothing until it has read it. Receive advertisements are always v0.
  2. parses the command (and push-options) section with `Layer.ParsePushRequestStream` — pkt-lines
     up to the flush — leaving the reader positioned at the pack start.
  3. streams the pack through `Layer.IngestStream` (piped straight into `index-pack`, which stops
     at the pack trailer; the feed goroutine is released at child exit, not awaited). max_push_bytes
     is enforced by a capped reader; crossing it fails index-pack and surfaces ErrMaxBytes.
  4. runs connectivity + publish + report-status exactly as the HTTP core (`pushPipeline`).
- A pure-delete push sends no pack bytes at all; the pipeline skips ingest when every command is a
  delete. (Reading the pack start of a delete push would hang — there is nothing coming.)

## 3. Config and auth

```toml
[server.ssh]
listen = ""            # e.g. "0.0.0.0:2222"; empty = disabled (default)
host_key = ""          # path to an OpenSSH/PEM private key
host_key_env = ""      # env var NAME holding the private key; overrides host_key
[[server.ssh.keys]]
principal = "ada"
key = "ssh-ed25519 AAAA... ada@laptop"   # authorized_keys line; exactly one of key/key_env
key_env = ""
write = true
admin = false
```

- Validation (`11_config_cli.md §5`): `listen` must be host:port when set; each key needs a
  principal and exactly one of key/key_env (resolved at boot — a set-but-unreadable env is fatal);
  key lines must parse as authorized_keys (`ssh.ParseAuthorizedKey`); duplicate fingerprints fail.
- Host key resolution: `host_key_env` → `host_key` path → auto-generated ed25519 key persisted at
  `<data-dir>/ssh/ed25519_host_key` (0600, dir 0700). Auto-generation keeps zero-config SSH boots
  whole; clients pin the key on first connect (TOFU), like `ssh-keygen -A`.
- Keys are a credential class of their own (like `auth.tokens`): each maps a public key to a
  principal with write/admin flags. They work in every auth mode. The matched principal travels in
  the transport call; push requires the key's `write` flag (enforced in the session handler).
- The transport is disabled unless `listen` is set, and disabled in setup-only mode (no engine).

## 4. Transport seam

```go
// internal/sshd
type Transport interface {
    SSHUploadPack(ctx context.Context, id git.RepoId, protocol string,
        stdin io.Reader, stdout, stderr io.Writer) error
    SSHReceivePack(ctx context.Context, id git.RepoId, principal string,
        stdin io.Reader, stdout, stderr io.Writer) error
}
```

Implemented by `internal/server` (`bind_ssh.go`): gates first (drain §12, placement §4.3 — a repo
not served here is refused before any sync), then the shared pipeline. `pushPipeline` is the
transport-agnostic push core used by both the HTTP handler and SSH; HTTP maps pre-pipeline errors
onto 4xx/5xx statuses, SSH maps them onto stderr text and exit code 1. Sentinel errors
(`sshd.ErrNotFound`, `ErrUnavailable`, `ErrDenied`) are the mapping vocabulary.

## 5. Git layer additions

- `Layer.UploadPackSSH` — UploadPack without `--stateless-rpc` (see §2).
- `Layer.ParsePushRequestStream` — command/options section parse leaving the reader at the pack
  start (`ParsePushRequest` remains the framed/HTTP form).
- `Layer.IngestStream` — pack piped straight into `index-pack` (no staging file): index-pack stops
  at the pack trailer, so the feed ends at child exit. `Ingest` (staged) is unchanged for HTTP.

## 6. Security

- Strictly two verbs; no option-bearing argv; no interactive anything.
- `MaxSessions` (default 64) caps concurrent exec sessions; each session's git subprocess runs on
  the blocking pool like HTTP; x/crypto channel windowing bounds output buffering.
- Auth failures log the key fingerprint at warn (scan visibility) and fail the handshake.
- The SSH listener shares the process drain: sessions started during drain are refused by the
  transport gates; the listener itself closes with the process context.

## 7. Tests

- `internal/sshd/sshd_test.go`: `SplitCommand` table (quoting, escapes, unterminated), command
  table (verbs, `.git`, leading `/`, injection attempts, extra words), auth matrix over a real
  loopback handshake (accepted key reaches the transport with the right RepoId/protocol; read-only
  principal cannot push; unknown key fails the handshake), transport-error → stderr mapping, host
  key generation persistence.
- `internal/server/bind_ssh_test.go`: real `git clone` / `git push` over `ssh://127.0.0.1:<random>`
  with a generated client key and `GIT_SSH_COMMAND` — push → clone → second push → log assertions,
  plus a read-only key clone-then-refused-push case.

## Decisions & deviations from the Rust design

- **17.1 (2026-09-02) — `golang.org/x/crypto/ssh` is allowed as the fourth backend module.**
  The user directed SSH transport support (issue #1); hand-rolling SSH is out of the question and
  every Go reference implementation (Gitea, Forgejo) builds on x/crypto. Amends Law 1.
- **17.2 (2026-09-02) — receive-pack opens with the server's advertisement, always v0.** SSH
  receive-pack differs from HTTP framing: without the advertisement the client sends nothing and
  both sides hang. Protocol v2 does not cover push; git ignores v2 for push.
- **17.3 (2026-09-02) — `IngestStream` bypasses staging.** Staging reads the pack to EOF, which
  deadlocks against a client that holds the channel open; streaming straight into index-pack
  relies on index-pack stopping at the pack trailer. The feed goroutine is released at child exit
  and dies with the session.
- **17.4 (2026-09-02) — SSH pushes do not use the push broker.** The broker is an HTTP-forwarding
  optimization (§4.3); an SSH session is already on the serving host, so the local pipeline runs
  or the placement gate refuses it.

### Concurrency

- Hazard: unbounded SSH sessions × git subprocesses exhaust CPU/memory like unbounded HTTP git
  requests; long-lived channels could pin goroutines.
- Avoidance: `MaxSessions` semaphore (default 64) around exec dispatch; connection and session
  goroutines bound to the connection context (canceled on disconnect/shutdown); feed goroutines
  released at child exit and die with the session; heavy execs stay on the blocking pool.
