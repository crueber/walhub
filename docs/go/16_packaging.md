# 16 — Packaging, deployment, and the local rig

> Source: MASTER_RUST_SPEC.md §18 (packaging/deployment) · §15.1 (ops-referenced config keys) · §16 (CLI/exit codes) · the `deploy/nginx.conf.example` edge contract · Status: normative for the walhub Go implementation.

An operator MUST be able to build, containerize, deploy (one box or a fleet), and onboard a developer from this document alone. Behavioral truth comes from the Rust spec; everything Go-specific here is a marked decision (see the final section).

## 1. Build

### 1.1 The binary

- Module `git.packden.us/crueber/walhub`; main package `cmd/walhub`. One binary, `walhub`, doing everything (`serve`, `bundle`, `repo`, `wal`, `import`, …; **no subcommand = `serve`**). There is no separate `walhub-server` binary.
- `CGO_ENABLED=0` always: pure Go, static, no libc coupling. This is what makes the runtime image choice (§2) and cross-compilation trivial:
  ```sh
  make build                       # native
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 make build   # cross, no toolchain changes
  ```
- Release build flags: `CGO_ENABLED=0 go build -trimpath -buildvcs=stamp -ldflags "-s -w -X main.buildSHA=$(SHA)"`. Go has no LTO and does not need one; `-s -w` + `-trimpath` is the equivalent "thin" profile (smaller, reproducible).
- Exit codes stay 0/1/2/3 (§16): 0 ok, 1 command/config error, 2 missing config file / bad argv, 3 `config check --strict` with ignored overrides.
- git is ALWAYS the subprocess `git` binary with exact argv (never go-git or any VCS library). On startup `serve` MUST run `git version`, parse it, and exit 1 with a clear message if it is < 2.47 (server-side needs: `pack.writeReverseIndex`, `--rev-index`, bundle-uri). The `git.binary` config key (unchanged name) selects the executable.

### 1.2 Embedded web assets (`go:embed` replaces rust-embed)

- File `web/embed.go`:
  ```go
  package web

  import "embed"

  //go:embed all:dist
  var Dist embed.FS
  ```
- `internal/server` serves `web.Dist` through `http.FileServerFS` (Go 1.22+); `repos.js`/`repos.mjs` are served from the same FS at their contract paths (see 06_server_http.md / 07_api.md).
- The build MUST fail if `web/dist` is missing or empty — this falls out of `go:embed` (compile error), matching the Rust rule "build fails if missing". No placeholder files. Every build entry point (Makefile `build`, CI, Containerfile stage 2) therefore depends on the web build first.
- `all:` prefix includes dotfiles; `.gitignore`-style exclusions do not apply inside `dist` once built.

### 1.3 Version identity

- Resolution order for the reported build sha:
  1. `-ldflags -X main.buildSHA=<12-hex>` (set by Makefile/CI/Containerfile from `git rev-parse --short=12 HEAD`),
  2. `runtime/debug.ReadBuildInfo` `vcs.revision` (populated by `-buildvcs=stamp`),
  3. `"dev"`.
- `walhub version` prints the sha plus the full `debug.BuildInfo` (module versions, Go version, settings). `serve` logs the same identity once at startup. The exact command table lives in 11_config_cli.md; the mechanism is specified here because it is a build artifact.

### 1.4 Runtime external dependencies

Exactly ONE external binary at runtime: `git` (≥ 2.47 server-side, ≥ 2.46 for clients using credential `authtype`). git-lfs is a **client** concern and MUST NOT be shipped in the server image. CA certificates are data, and are required (OIDC discovery, upstream follow, S3/GCS over TLS).

## 2. Container image

Multi-stage `Containerfile` at the repo root. Node 20 + pnpm builds the SPA → Go builds the static binary → alpine runtime with git. (Rust used debian-slim + tini; see deviations.)

```dockerfile
# Containerfile — one binary in front of an object store.
#   podman build -t walhub -f Containerfile .
#   podman run --rm -p 8080:8080 -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY \
#       -v ./walhub.toml:/etc/walhub/walhub.toml:ro -v walhub-cache:/var/lib/walhub walhub

# ---- 1. web UI (embedded at compile time) ------------------------------------------
FROM docker.io/library/node:20-alpine AS web
RUN corepack enable && corepack prepare pnpm@10 --activate
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm run build && test -f dist/index.html && test -f dist/repos.js

# ---- 2. Go build (static, no cgo, no protoc needed) --------------------------------
FROM docker.io/library/golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ARG WALHUB_BUILD_SHA=dev
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=stamp \
      -ldflags "-s -w -X main.buildSHA=${WALHUB_BUILD_SHA}" \
      -o /out/bin/walhub ./cmd/walhub

# ---- 3. runtime: git >= 2.47, CA certs, nonroot ------------------------------------
FROM docker.io/library/alpine:3.22
RUN apk add --no-cache git ca-certificates \
    && git --version \
    && addgroup -g 1000 walhub && adduser -D -u 1000 -G walhub walhub \
    && mkdir -p /etc/walhub /var/lib/walhub && chown -R walhub:walhub /var/lib/walhub
COPY --from=build /out/bin/walhub /usr/local/bin/walhub
COPY walhub.example.toml /etc/walhub/walhub.toml
ENV WALHUB_CONFIG=/etc/walhub/walhub.toml \
    WALHUB__CACHE__DIR=/var/lib/walhub \
    WALHUB__SERVER__LISTEN=0.0.0.0:8080
USER walhub
WORKDIR /var/lib/walhub
EXPOSE 8080
VOLUME ["/var/lib/walhub"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s \
    CMD wget -qO- http://127.0.0.1:8080/readyz >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/walhub"]
CMD ["serve"]
```

Rules:

- **Runtime base is alpine, not distroless/static**: the one hard runtime dependency is `git`, and distroless ships none. Alpine 3.22 ships git ≥ 2.47; a CI job (§8) fails the image if `git --version` inside it is < 2.47. tini is not needed (alpine `docker run --init`, k8s, and podman reap zombies; `git` children are waited on by `os/exec` anyway — see 04_git.md).
- `/readyz` reflects real readiness: store open + prewarm done (gated by `cache.prewarm_ready_timeout`) — NOT liveness. `/healthz` is the dumb liveness probe.
- The config file baked at `/etc/walhub/walhub.toml` is the *example* file; operators mount their own over it or set `WALHUB__*` env overrides. The three baked ENVs (`WALHUB_CONFIG`, `WALHUB__CACHE__DIR`, `WALHUB__SERVER__LISTEN`) are the same three the Rust image baked, renamed.
- `WALHUB_BUILD_SHA` build-arg comes from CI; a locally built image reports `dev`.

## 3. Configuration for operators

### 3.1 File discovery, env overrides, and compatibility

Behavior is §15 of the Rust spec, unchanged, plus these naming rules:

| Mechanism | Primary (walhub) | Legacy accepted (compat) |
|---|---|---|
| `--config PATH` flag | wins | — |
| config file env pointer | `WALHUB_CONFIG` | `WALGIT_CONFIG` |
| default file name (CWD) | `walhub.toml` | `walgit.toml` |
| section/key env overrides | `WALHUB__SECTION__KEY=value` | `WALGIT__SECTION__KEY=value` |
| log filter env | `WALHUB_LOG` | `RUST_LOG` |

- TOML **key names** are unchanged from §15.1 — that is the compatibility surface. Both prefixes are stripped by the same loop (accept either; the longest matching prefix wins per key), so ops scripts and compose files written for the Rust deployment keep working verbatim.
- `PORT` env overrides the listen port and rewrites a loopback `public_url`'s port in lockstep (unchanged).
- `--config /dev/null` = explicit defaults + env only. Missing config file is fatal (exit 2).
- `walhub config check [--env-file …] [--strict]` is fail-closed (§15) and is the supervisor pre-start gate (exit 3 on ignored overrides under `--strict`):
  ```sh
  docker run --rm -v ./walhub.toml:/etc/walhub/walhub.toml:ro walhub config check --strict
  ```

### 3.2 `walhub.standalone.toml` — the one-box shape

Copy-pasteable; every key is a §15.1 key. This is the local/dev default (`make dev`) and the template for a single production box. Memory-friendly: small cache budget, small remote-reader LRUs.

```toml
# walhub.standalone.toml — one binary, one bucket, one origin.
#   walhub serve --config walhub.standalone.toml
#   → https://walhub.localhost:8080/   (self-signed TLS; PORT=… moves it)

[server]
listen = "127.0.0.1:8080"                      # PORT env wins; also binds the ::1 twin
public_url = "https://walhub.localhost:8080"
auto_create_on_push = true
roles = []                                     # empty = every role: serve, maintain, events

[server.tls]
mode = "self_signed"          # generated once under <cache.dir>/tls/, published at /services/public/ca.pem

[server.auth]
mode = "none"                 # loopback play: everyone is `anon` with write
# Shared machine — static tokens:
# mode = "token"
# anonymous_read = false
# tokens = [
#   { principal = "alice", token_env = "WALHUB_TOKEN_ALICE", write = true },
# ]

[store]
backend = "s3"
bucket = "walgit-test"                         # created by `make dev-store` (compose)
prefix = ""
[store.s3]
endpoint = "http://127.0.0.1:9000"
region = "us-east-1"
access_key_env = "AWS_ACCESS_KEY_ID"
secret_key_env = "AWS_SECRET_ACCESS_KEY"
force_path_style = true

# Chained bundles: weekly full, then each daily on the previous daily (first on the
# weekly), each hourly on the previous hourly (restarting at every new daily).
[[bundles.strategy]]
name = "weekly"
kind = "full"
schedule = "0 0 23 * * Sun"
keep = 1
backfill_max = 1
[[bundles.strategy]]
name = "daily"
kind = "incremental"
base = "weekly"
schedule = "0 0 23 * * *"
chain = true
[[bundles.strategy]]
name = "hourly"
kind = "incremental"
base = "daily"
schedule = "0 0 * * * *"
chain = true

[cache]
dir = "/tmp/walhub"                            # materialized repos + tls/; wiped = warmth lost
max_bytes = "8GiB"
remote_block_bytes = "512MiB"                  # memory-friendly LRU caps for a laptop/one-box
remote_object_bytes = "128MiB"

[telemetry]
log_format = "pretty"
```

### 3.3 Minimal S3 config (production)

```toml
[server]
listen = "0.0.0.0:8080"
public_url = "https://git.example.com"
[server.tls]
mode = "files"
cert = "/etc/walhub/tls/fullchain.pem"
key = "/etc/walhub/tls/privkey.pem"
[server.auth]
mode = "oidc"
issuer = "https://id.example.com"
allowed_domains = ["example.com"]
oauth_client_id = "walhub"
# secrets via env: WALHUB__SERVER__AUTH__OAUTH_CLIENT_SECRET / __SESSION_SECRET
[store]
backend = "s3"
bucket = "git-packden-us"                      # any S3: AWS, R2, MinIO, Ceph…
[store.s3]
region = "us-east-1"
# endpoint / force_path_style omitted = AWS defaults; set them for non-AWS S3.
```

GCS is the same shape with `backend = "gcs"` + `store.gcs.signing_service_account` for signed URLs (see 03_store_backends.md for credentials and the JSON-API transport).

## 4. The nginx edge contract

The Rust `deploy/nginx.conf.example` is the contract of record; walhub keeps it byte-for-byte on the wire. Header names stay `X-Walgit-*` (decision below) — an nginx config written for walgit MUST work in front of walhub unchanged.

### 4.1 The per-request contract (never assumed from config)

| Direction | Header | Meaning |
|---|---|---|
| nginx → walhub | `X-Walgit-Capabilities: accel-redirect, client-authorization` | what this edge takes over; walhub trusts it only because *this hop* set it (clients are stripped of it) |
| nginx → walhub | `X-Walgit-Authorization: <client's Authorization>` | the client's credential, preserved even if an intermediate rewrote `Authorization` |
| nginx → walhub | `X-Walgit-Principal: ""` | cleared; only walhub's own push-broker hop may set it (`server.auth.trusted_forwarders`) |
| walhub → nginx | `200` + `X-Accel-Redirect: /_store/` | the bytes are nginx's problem now (only when `server.accel_redirect = true`) |
| walhub → nginx | `X-Walgit-Store-Url` | bucket URL (S3: presigned; GCS: plain) |
| walhub → nginx | `X-Walgit-Store-Authorization` | `Bearer …` (GCS only; S3 URLs are self-credentialed) |
| walhub → nginx | `X-Walgit-Store-Key` | bucket key, percent-encoded — the cache key |
| walhub → nginx | `X-Walgit-Etag` | walhub's strong validator, re-emitted by nginx |

Auth: everything except open routes (`^/(healthz|readyz|repos\.js|repos\.mjs)$`, `/services/public/`, `/_auth/`) passes `auth_request /_auth/check`; walhub answers `204` (or 401/403/503) with no body. Verdicts are cached **5 min per credential** (`proxy_cache_key "$http_authorization|$cookie_walgit_session"`), 401/403 only **5 s**; `proxy_cache_lock on`. A denied browser navigation (Accept: text/html) is 302'd to `/_auth/login?next=…`; everything else gets a real 401 with `WWW-Authenticate: Bearer realm="walgit"` so git erases the dead token.

Byte offload at `/_store/` (internal only): `slice 64m` (a resumed 30 GB download hits cache; `Range` is not a signed header for S3), `proxy_cache_key "$store_key$slice_range"`, cached `30d`, `proxy_cache_lock` + `use_stale updating`, conditionals stripped (`If-Range`/`If-(None-)Match`/`If-Modified-Since` were already decided by walhub against ITS validator), bucket headers hidden, exactly one validator re-emitted: walhub's `X-Walgit-Etag`. Pushes/LFS uploads stream: `client_max_body_size 0` + `proxy_request_buffering off`.

Repo-prefix routing for a fleet: `location ~ ^/<owner>/<repo>([./?]|$)` blocks per upstream group — the repo prefix is the only routing key walhub needs. When the edge hands a repo to the wrong upstream, that instance answers read-only/refs-level with `503` + `Retry-After: 15` (placed-repo fallback; the object work is refused).

### 4.2 When NOT to use nginx

`walhub serve` terminates TLS itself (`server.tls.mode = "self_signed" | "files"`), streams every byte, and authenticates every request. nginx buys: ACME/public certs + HTTP/2, the disk cache for bundle/LFS bytes (`X-Accel-Redirect` offload), one cached auth verdict per credential, and prefix routing across hosts.

## 5. Deployment shapes

| Shape | Config essence | Notes |
|---|---|---|
| **One box** | §3.2 standalone: all roles, TLS in-process, one S3/GCS bucket, nothing in front | `roles = []`; self-signed or files TLS; the bucket is the only durable state |
| **Fleet** | many `walhub serve` hosts behind the nginx edge; the monorepo's host pins `cache.mode = "disk"` + `maintenance.disk = "ssd"`, everyone else budget-mode (`cache.max_bytes`) | placement globs decide who does what (below); the edge does TLS/routing/offload; `server.auth.session_secret` MUST be identical on every host (rotation revokes all sessions+tokens) |
| **Serverless** | `server.roles = ["serve"]` only, config purely via env (`--config /dev/null` + `WALHUB__*`), `cache.dir` on the ephemeral FS, bounded prefetch (`wal.prefetch_max_bytes`, set it low; `wal.prefetch_packs = false` under tight CPU) | CPU throttles between requests; a maintain fleet elsewhere holds the maintain role; keep-alives off, cold starts re-warm on demand |

Placement globs (`owner/name` | `owner/*` | `*`), on every host:

```toml
[placement]                       # SSD host (the monorepo's home)
serve = ["acme/monorepo", "team/*"]
maintain = ["acme/monorepo"]
maintenance.disk = "ssd"
cache.mode = "disk"
```

```toml
[placement]                       # budget hosts (everything else)
serve = ["*"]
serve_exclude = ["acme/monorepo"]
maintain_exclude = ["acme/monorepo"]
```

`[placement]` env overrides replace the whole section (all-or-nothing) — fleets inject placement per instance via env. Nix-flake packaging is a Rust-side nicety and is NOT ported (deviation below).

## 6. Process lifecycle, readiness, drain

`walhub serve` starts: store → prewarm (if configured) → maintainer + follow loops (per roles) → HTTP server. `SIGTERM`/`SIGINT` → graceful drain: new work refused, in-flight requests finish within `server.drain_timeout` (default `20s`), then exit 0.

### Concurrency

Hazards at the packaging boundary (playbook: 13_concurrency.md):

- **Prewarm vs readiness**: prewarm fetches MUST run on at most `cache.prewarm_parallelism` goroutines sharing one `context.Context` owned by `cmd/walhub`; `/readyz` reports a `sync/atomic` readiness flag, never polls locks — a healthcheck that blocks on a mutex makes the orchestrator's probe the deadlock detector of last resort. `cache.prewarm_ready_timeout` bounds how long readiness may stay false; after it, serve anyway and log a WARNING.
- **Drain**: the root context is canceled on the second signal (hard stop); the first signal stops listeners via `http.Server.Shutdown` and lets bulk worker pools finish their current unit within `drain_timeout`. Every goroutine started by `serve` (maintainer, events, bundle builders) takes that context; `cmd/walhub` owns the context and waits for the worker pools' `WaitGroup` before exiting — nobody else closes the context.
- **Panics**: net/http recovers handler panics per request (log + 500 + request id). Every background loop (maintainer pass, events sweep, bundle build) wraps each pass in its own `recover` so one bad repo degrades to a WARN line, not a dead instance — the Go equivalent of the Rust "unwind panics, one request's panic must not kill the instance" release profile.

## 7. Local rig

### 7.1 `compose.yaml` (repo root) — rustfs + bucket creation

```yaml
# S3-compatible store for local dev. Console on :9001. Fixed dev keys — never in production.
services:
  rustfs:
    image: rustfs/rustfs:latest
    ports: ["9000:9000", "9001:9001"]
    environment:
      RUSTFS_ACCESS_KEY: walgit-dev
      RUSTFS_SECRET_KEY: walgit-dev-secret
      RUSTFS_ADDRESS: "0.0.0.0:9000"
      RUSTFS_CONSOLE_ADDRESS: "0.0.0.0:9001"
      RUSTFS_VOLUMES: /data
    volumes: [rustfs-data:/data]
    healthcheck:
      test: ["CMD", "curl", "-sf", "http://localhost:9000/minio/health/live"]
      interval: 5s
      timeout: 3s
      retries: 10
  create-bucket:                 # one-shot, idempotent
    image: amazon/aws-cli:latest
    depends_on:
      rustfs: { condition: service_healthy }
    environment:
      AWS_ACCESS_KEY_ID: walgit-dev
      AWS_SECRET_ACCESS_KEY: walgit-dev-secret
      AWS_DEFAULT_REGION: us-east-1
    entrypoint: []
    command: >
      sh -c "aws --endpoint-url http://rustfs:9000 s3 mb s3://walgit-test 2>/dev/null || true &&
             aws --endpoint-url http://rustfs:9000 s3 ls"
    restart: "no"
volumes:
  rustfs-data: {}
```

### 7.2 `Makefile` (repo root) — the dev loop

```make
BINARY  := walhub
SHA     ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo dev)
GO      := CGO_ENABLED=0 go
LDFLAGS := -s -w -X main.buildSHA=$(SHA)

web-build:                      ## node20 + pnpm → web/dist (embedded at compile time)
	cd web && pnpm install --frozen-lockfile && pnpm run build

build: web-build                ## static binary; fails if web/dist is missing
	$(GO) build -trimpath -buildvcs=stamp -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/walhub

vet: web-build
	go vet ./...

test:                           ## fast tier; -race needs cgo, so CGO stays on here
	CGO_ENABLED=1 go test -race ./...

e2e:                            ## smart-HTTP end-to-end against the real git binary
	CGO_ENABLED=1 go test -race -tags e2e ./internal/server/... -run TestE2E

dev-store:                      ## rustfs + bucket (compose.yaml)
	docker compose up -d rustfs && docker compose run --rm create-bucket

dev: dev-store web-build        ## the whole loop: store + build + serve standalone
	./$(BINARY) serve --config walhub.standalone.toml

test-s3:                        ## store contract against local rustfs (needs dev-store)
	WALHUB_TEST_S3_ENDPOINT=http://127.0.0.1:9000 \
	WALHUB_TEST_S3_BUCKET=walgit-test \
	go test -tags s3 ./internal/store/... -run TestS3Contract

image:                          ## multi-stage build with the real sha
	podman build --build-arg WALHUB_BUILD_SHA=$(SHA) -t walhub -f Containerfile .

ci: vet test e2e                ## what CI runs; image build happens in CI
```

(The `just dev`-equivalent is `make dev`. `just` is fine too; the Makefile is the canonical target set.)

## 8. CI pipeline

The repo lives on Forgejo; CI is Woodpecker-shaped (no GitHub Actions). Pipeline = vet → test `-race` → e2e → store contract against a rustfs service → image build. Web build runs FIRST in every Go step's dependency chain (embed fails otherwise).

```yaml
# .woodpecker/pipeline.yaml
when: { event: [push, pull_request] }

steps:
  web:
    image: node:20-alpine
    commands:
      - corepack enable && corepack prepare pnpm@10 --activate
      - make web-build

  vet:   { image: golang:1.25, commands: [make vet],    depends_on: [web] }
  test:  { image: golang:1.25, commands: [make test],   depends_on: [web] }   # golang image ships git + gcc (-race)
  e2e:   { image: golang:1.25, commands: [make e2e],    depends_on: [test] }

  s3-contract:
    image: golang:1.25
    commands: [make test-s3]
    depends_on: [web]
    environment:
      WALHUB_TEST_S3_ENDPOINT: http://rustfs:9000
      WALHUB_TEST_S3_BUCKET: walgit-test

  image:
    image: plugins/docker
    settings: { repo: git.packden.us/crueber/walhub, tags: "${CI_COMMIT_SHA:0:12},latest" }
    depends_on: [test, e2e, s3-contract]

services:
  rustfs:
    image: rustfs/rustfs:latest
    environment:
      RUSTFS_ACCESS_KEY: walgit-dev
      RUSTFS_SECRET_KEY: walgit-dev-secret
```

CI MUST also verify the container contract: after `image`, run `podman run --rm <img> git --version` and fail if < 2.47, plus a `walhub config check --strict` against the example config (catches a baked default that no longer validates).

## 9. Onboarding a developer (30-second quickstart)

```sh
# Prerequisites: docker, git >= 2.46 on the client.
git clone https://git.packden.us/crueber/walhub && cd walhub
make dev                                  # rustfs + bucket + web build + serve
# → https://walhub.localhost:8080 (self-signed; browser: Advanced → proceed)

# Trust the self-signed CA for git (pinned once per machine):
mkdir -p ~/.walhub && curl -k https://walhub.localhost:8080/services/public/ca.pem -o ~/.walhub/ca.pem
git config --global http.https://walhub.localhost:8080/.sslCAInfo ~/.walhub/ca.pem

# Push creates the repo (auto_create_on_push = true):
mkdir demo && cd demo && git init -b main && echo hello > README.md
git add . && git commit -m init
git remote add origin https://walhub.localhost:8080/$USER/demo.git
git push -u origin main

# Clone from another terminal/dir — a fresh clone rides bundle-uri:
git clone https://walhub.localhost:8080/$USER/demo.git
```

Token minting (team/production, `auth.mode = "oidc"`):

1. Open `https://git.example.com` → sign in through the issuer (browser flow, `/_auth/callback`).
2. Visit `/_auth/tokens` → mint a token: `wgt_…`, valid `server.auth.access_token_ttl` (default `90d`).
3. Feed it to git once per machine:
   ```sh
   export WALHUB_TOKEN=wgt_…   # from the tokens page
   git config --global credential.https://git.example.com.helper \
     '!f() { printf "username=x\npassword=%s\n" "$WALHUB_TOKEN"; }; f'
   ```
   Static-token deployments (`auth.mode = "token"`) skip the browser: the principal's `token_env` value IS the password (HTTP Basic or bearer).

Install one-liner (from any walhub host):

```sh
curl -fsSL https://git.packden.us/crueber/walhub/raw/branch/main/scripts/install.sh | sh
# installs ./walhub into ~/.local/bin (release tarball by arch, or `go install` fallback),
# and, when the target server serves self-signed TLS, fetches its CA and configures git.
```

## Decisions & deviations from the Rust design

- **One binary (`walhub`), no `walhub-server` twin** — Rust shipped two; Go compiles one `main` with the same "no subcommand = serve" semantics, so the twin adds nothing.
- **`walhub.toml` is the default config name; `walgit.toml` still accepted, `WALHUB__` env prefix primary with `WALGIT__` also stripped** — TOML key names stay identical (the compat surface), while process/env names follow the new binary; honoring legacy prefixes keeps Rust-era ops scripts and compose files working verbatim.
- **Runtime image is alpine (git ≥ 2.47), not debian-slim + tini** — distroless/static lacks `git`, the one hard runtime dependency; alpine also ships busybox `wget` so HEALTHCHECK needs no curl; zombie reaping is handled by `os/exec` wait + orchestrator init.
- **git-lfs omitted from the image** — the Rust spec itself notes it is a client-side tool; the server never invokes it.
- **Node 20 (not 24) + pnpm@10 for the web stage** — meets the SolidJS toolchain floor while keeping the image tag pinned and small.
- **Go release flags `-trimpath -buildvcs=stamp -ldflags "-s -w"` replace thin LTO + unwind-panic profile** — Go has no LTO; per-request/per-pass `recover` reproduces the "one panic must not kill the instance" guarantee (§6).
- **Startup git version check is fatal (exit 1) below 2.47** — the Rust spec declares ≥ 2.47 required for server features; failing fast with a message beats mysterious wire breakage.
- **Header names stay `X-Walgit-*` (including the `walgit_session` cookie in the auth cache key) and the token prefix stays `wgt_`** — the nginx contract and users' stored credentials are the edge-compat surface; renaming would silently invalidate deployed edge configs and credential helpers.
- **Dev-rig rustfs keys/bucket keep the Rust values (`walgit-dev` / `walgit-dev-secret`, bucket `walgit-test`)** — muscle-memory continuity for contributors coming from the Rust rig; local-only credentials.
- **Nix flake packaging not ported** — the flake was a Rust toolchain convenience; Go's static cross-build plus the Containerfile covers the same ground without a second packaging system (revisit only on demand).
- **CI is a Makefile + Woodpecker pipeline, not GitHub Actions** — the repo lives on Forgejo; Woodpecker's services block covers the rustfs contract job.
- **`install.sh` is served from the Forgejo raw URL** — the Rust edge example leaves the installer out of the open-route set; keeping it repo-hosted avoids inventing a new public server route in the Go rewrite.
