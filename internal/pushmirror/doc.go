// Package pushmirror implements Forgejo #623: push mirroring.
//
// A repo configured with a push mirror has its refs+objects pushed to an
// upstream URL (Forgejo/GitHub mirror or personal backup) after pushes land
// in walhub (on-push trigger) and optionally on a preset schedule. This
// complements internal/mirror (pull-only: fetches FROM an upstream and
// refuses pushes); the two sidecars are fully independent — either may
// exist alone.
//
// Layout (bucket-is-repo, law 4; append-only compat, law 5):
//
//	repos/<o>/<r>/meta/pushmirror.json         config sidecar (Create-once-then-CAS'd)
//	repos/<o>/<r>/meta/pushmirror-secret.json  auth material (CAS'd, never echoed)
//	leases/pushmirror-<owner>-<name>.pb         cross-instance sync lease (CAS+TTL)
//
// Auth kinds: none (file:// and public https), password (https basic),
// token (https bearer-style), ssh (ssh/scp deploy-key style: user-provided
// or walhub-generated ed25519). Credentials are write-only (presence +
// last-4 hint, never full echo) and scrubbed from every error/last_result
// surface (the pull-mirror scrub contract).
//
// Transfer shells out to the stock git binary (law 2: git is a subprocess;
// exact argv in docs/go/04_git.md): the serving copy (manifest refs already
// applied by Sync) pushes via `git push --mirror`. SSH auth rides
// GIT_SSH_COMMAND with a materialized key file — x/crypto/ssh is
// server-transport-only and is never used as a client. Keypairs are
// generated dep-free (stdlib ed25519 + hand-rolled OpenSSH wire format).
//
// ### Concurrency
//
// Hazard: two instances firing one repo's push mirror would run two full
// pushes and race outcomes; the (repo,kind) single-flight is
// instance-memory only and does not span instances.
// Avoidance: the sync body takes the bucket lease BEFORE pushing (held =
// skip + narrate, never wait, never retry in the fire). The scheduled
// loop is one goroutine exiting via ctx; every fire is a task whose
// goroutines derive from the task ctx (drain cancels everything); no lock
// is held across store/network calls. Push bytes are BULK: the Runner's
// own bounded pool never shares the control-plane lane (law 3).
package pushmirror
