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
- Exit codes stay 0/1/2/3 (§16): 0 ok, 1 command/config error, 2 bad argv (a missing config file no longer exits — D5 boots on built-in defaults), 3 `config check --strict` with ignored overrides.
- git is ALWAYS the subprocess `git` binary with exact argv (never go-git or any VCS library). On startup `serve` MUST run `git version`, parse it, and exit 1 with a clear message if it is < 2.47 (server-side needs: `pack.writeReverseIndex`, `--rev-index`, bundle-uri). The `git.binary` config key (unchanged name) selects the executable.

### 1.2 Embedded web assets (`go:embed`; one build: the SDK bundle)

- File `web/embed.go`:
  ```go
  package web

  import "embed"

  //go:embed index.html all:dist all:sdk all:src all:css
  var Files embed.FS
  ```
- `internal/server` serves `web.Files` through `http.FileServerFS`; the browser loads the **vite-built SolidJS bundle** (`web/dist/index.html` + content-hashed `assets/*`, 2026-09-02 user decision, DEVIATIONS.md D-WEB-6 — Tailwind v4 CSS-first, dark mode by default, no CDN). The SDK is **authored as submodules** (`web/sdk/src/*.js`, 12_web_ui.md §1.0) and bundled by esbuild into `web/dist/repos.js`, served at its contract path (see 06_server_http.md / 07_api.md) — the `repos.mjs` twin is gone (D2); `make build` depends on `make web`.
- The SPA has NO build step (no TypeScript, no framework); the SDK bundle is the ONLY build output. `node --test web/test/` runs the headless JS tests (§7.2/§8) but never feeds the build. The plain (non-`all:`) embed pattern excludes `.`/`_`-prefixed entries, so `web/test/` and dotfiles stay out of the binary while remaining on disk for tests.

### 1.3 Version identity

- Resolution order for the reported build sha:
  1. `-ldflags -X main.buildSHA=<12-hex>` (set by Makefile/CI/Dockerfile from `git rev-parse --short=12 HEAD`),
  2. `runtime/debug.ReadBuildInfo` `vcs.revision` (populated by `-buildvcs=stamp`),
  3. `"dev"`.
- `walhub version` prints the sha plus the full `debug.BuildInfo` (module versions, Go version, settings). `serve` logs the same identity once at startup. The exact command table lives in 11_config_cli.md; the mechanism is specified here because it is a build artifact.

### 1.4 Runtime external dependencies

Exactly ONE external binary at runtime: `git` (≥ 2.47 server-side, ≥ 2.46 for clients using credential `authtype`). git-lfs is a **client** concern and MUST NOT be shipped in the server image. CA certificates are data, and are required (OIDC discovery, upstream follow, S3/GCS over TLS).

## 2. Container image

`Dockerfile` at the repo root (three stages): node runs the ONE web build (`pnpm run build` — vite SPA + esbuild SDK, D-WEB-6) → Go builds the static binary (`golang:1.27-alpine` per go.mod) → alpine runtime with git. (Rust used debian-slim + tini; see deviations.)

```dockerfile
# Dockerfile — one binary in front of a store. Zero config by default: filesystem store in the data dir.
#   docker build -t walhub .
#   docker run --rm -p 8080:8080 walhub     # → open http://host:8080/setup
#   podman run --rm -p 8080:8080 walhub     # → open http://host:8080/setup

# ---- 1. Web: the ONE build step — esbuild bundles web/sdk/src → dist/repos.js ----
FROM docker.io/library/node:22-alpine AS web
RUN corepack enable && corepack prepare pnpm@10 --activate
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm run build:sdk && test -f dist/repos.js

# ---- 2. Go build (static, no cgo; embeds the built dist/ + raw src/) ----
FROM docker.io/library/golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG WALHUB_BUILD_SHA=dev
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=stamp \
      -ldflags "-s -w -X main.buildSHA=${WALHUB_BUILD_SHA}" \
      -o /out/bin/walhub ./cmd/walhub

# ---- 2. runtime: git >= 2.47, CA certs, nonroot ----
FROM docker.io/library/alpine:3.22
RUN apk add --no-cache git ca-certificates \
    && git --version \
    && addgroup -g 1000 walhub && adduser -D -u 1000 -G walhub walhub \
    && mkdir -p /var/lib/walhub && chown -R walhub:walhub /var/lib/walhub
COPY --from=build /out/bin/walhub /usr/local/bin/walhub
ENV WALHUB_DATA_DIR=/var/lib/walhub
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
- **No config file is baked**: the image boots zero-config (D5) with `WALHUB_DATA_DIR=/var/lib/walhub`; that data dir holds `store/`, `cache/`, and the `walhub.toml` that `/setup` writes. Operators who prefer a file mount one and point `WALHUB_CONFIG` at it, or set `WALHUB__*` env overrides — the table in §3.1 is unchanged.
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
- `--config /dev/null` = explicit defaults + env only. **A missing config file is NOT fatal (D5):** boot proceeds on built-in defaults — `server.listen = "0.0.0.0:8080"`, `store.backend = "filesystem"` rooted at `<data-dir>/store`, `server.auth.mode = "none"` (loud WARNINGs in logs + a setup banner), `server.auto_create_on_push = true` — and the UI advertises `/setup` (D6).
- A config file that IS present but fails validation boots into **setup-only mode** (D6): only `/setup`, `/healthz`, `/readyz` answer; everything else is `503` with a pointer to the exact errors, which the setup UI also shows. Saving a fixed config then requires a restart.
- `walhub config check [--env-file …] [--strict]` stays fail-closed for the file it is handed (§15) and remains the supervisor pre-start gate (exit 1 invalid, exit 3 on ignored overrides under `--strict`):
  ```sh
  docker run --rm -v ./walhub.toml:/etc/walhub/walhub.toml:ro walhub config check --strict
  ```

### 3.2 `walhub.standalone.toml` — OPTIONAL one-box bucket shape

Copy-pasteable; every key is a §15.1 key. **This file is OPTIONAL** — the documented default path is NO config at all (D5/D6: `./walhub serve`, first-run `/setup`). Use this shape when you deliberately want TLS in-process and a bucket as the durable state. The file the setup UI saves lands at `<data-dir>/walhub.toml` (atomic tmp+rename), not at this path. Memory-friendly: small cache budget, small remote-reader LRUs.

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

### 3.3 Minimal S3 config (production) — OPTIONAL

OPTIONAL, like §3.2 — the zero-config filesystem default (D4/D5) needs none of this. Shown as the graduation path for when a bucket becomes the job.

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

Auth: everything except open routes (`^/(healthz|readyz|repos\.js|repos\.mjs)$`, `/services/public/`, `/_auth/`) passes `auth_request /_auth/check`; walhub answers `204` (or 401/403/503) with no body. (`repos\.mjs` stays in the regex so edge configs written for walgit keep validating; walhub no longer ships that file — D2 — so the route simply never matches a real asset.) Verdicts are cached **5 min per credential** (`proxy_cache_key "$http_authorization|$cookie_walgit_session"`), 401/403 only **5 s**; `proxy_cache_lock on`. A denied browser navigation (Accept: text/html) is 302'd to `/_auth/login?next=…`; everything else gets a real 401 with `WWW-Authenticate: Bearer realm="walgit"` so git erases the dead token.

Byte offload at `/_store/` (internal only): `slice 64m` (a resumed 30 GB download hits cache; `Range` is not a signed header for S3), `proxy_cache_key "$store_key$slice_range"`, cached `30d`, `proxy_cache_lock` + `use_stale updating`, conditionals stripped (`If-Range`/`If-(None-)Match`/`If-Modified-Since` were already decided by walhub against ITS validator), bucket headers hidden, exactly one validator re-emitted: walhub's `X-Walgit-Etag`. Pushes/LFS uploads stream: `client_max_body_size 0` + `proxy_request_buffering off`.

Repo-prefix routing for a fleet: `location ~ ^/<owner>/<repo>([./?]|$)` blocks per upstream group — the repo prefix is the only routing key walhub needs. When the edge hands a repo to the wrong upstream, that instance answers read-only/refs-level with `503` + `Retry-After: 15` (placed-repo fallback; the object work is refused).

### 4.2 When NOT to use nginx

`walhub serve` terminates TLS itself (`server.tls.mode = "self_signed" | "files"`), streams every byte, and authenticates every request. nginx buys: ACME/public certs + HTTP/2, the disk cache for bundle/LFS bytes (`X-Accel-Redirect` offload), one cached auth verdict per credential, and prefix routing across hosts.

## 5. Deployment shapes

| Shape | Config essence | Notes |
|---|---|---|
| **Personal / self-hosted (default)** | no config at all: `./walhub serve` — filesystem store under `<data-dir>/store`, cache under `<data-dir>/cache`, `auth.mode = "none"` (loud warnings + setup banner), all roles | one binary, no bucket, no edge; tune later via `/setup` → `<data-dir>/walhub.toml`; add the nginx edge (§4) when it leaves the LAN |
| **One box** | §3.2 standalone: all roles, TLS in-process, one S3/GCS bucket, nothing in front | `roles = []`; self-signed or files TLS; the bucket is the only durable state |
| **Fleet** | many `walhub serve` hosts behind the nginx edge; the monorepo's host pins `cache.mode = "disk"` + `maintenance.disk = "ssd"`, everyone else budget-mode (`cache.max_bytes`) | placement globs decide who does what (below); the edge does TLS/routing/offload; `server.auth.session_secret` MUST be identical on every host (rotation revokes all sessions+tokens) |
| **Serverless** | `server.roles = ["serve"]` only, config purely via env (`--config /dev/null` + `WALHUB__*`), `cache.dir` on the ephemeral FS, bounded prefetch (`wal.prefetch_max_bytes`, set it low; `wal.prefetch_packs = false` under tight CPU) | CPU throttles between requests; a maintain fleet elsewhere holds the maintain role; keep-alives off, cold starts re-warm on demand |

One store per instance: `store.backend` is a single choice — the filesystem backend (D4) and the bucket backends cannot be mixed or tiered (03_store_backends.md). The personal shape uses filesystem; move to a bucket when durability/replication outgrows a disk, not alongside it.

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

### 7.1 Compose examples (repo root) — two stacks, both verified end to end

Two example compose files ship at the repo root:

- **`compose.standalone.yml`** — walhub ALONE: filesystem store on a named
  volume, port 8080, healthcheck. First boot is zero-config (auth `none` +
  loud warning); `/setup` writes `walhub.toml` into the volume for later boots.
- **`compose.yaml`** — the **bucket-backed stack**: rustfs (S3-compatible, the
  same fixture `make dev-store` and CI's s3-contract use) + a one-shot
  `create-bucket` job + walhub configured for the S3 store entirely through
  the `WALHUB__STORE__*` env overlay (no config file). `make dev-store`
  starts only the rustfs service from this file.

```yaml
# compose.yaml (abridged — the shipped file is the source of truth)
name: walhub-rustfs
services:
  rustfs:
    image: rustfs/rustfs:latest
    environment:
      RUSTFS_ACCESS_KEY: walgit-dev        # DEV KEYS — never production
      RUSTFS_SECRET_KEY: walgit-dev-secret
      RUSTFS_ADDRESS: "0.0.0.0:9000"
      RUSTFS_CONSOLE_ADDRESS: "0.0.0.0:9001"
      RUSTFS_VOLUMES: /data
    healthcheck: { test: ["CMD", "wget", "-qO-", "http://127.0.0.1:9000/minio/health/live"], interval: 5s, retries: 10 }
  create-bucket:
    image: amazon/aws-cli:latest           # idempotent s3 mb s3://walhub-test
    depends_on: { rustfs: { condition: service_healthy } }
  walhub:
    build: .
    depends_on:
      rustfs: { condition: service_healthy }
      create-bucket: { condition: service_completed_successfully }
    environment:
      WALHUB__STORE__BACKEND: s3
      WALHUB__STORE__S3__ENDPOINT: http://rustfs:9000   # NESTED keys: store.s3.endpoint
      WALHUB__STORE__S3__REGION: us-east-1
      WALHUB__STORE__S3__FORCE_PATH_STYLE: "true"        # required for rustfs/MinIO
      WALHUB__STORE__BUCKET: walhub-test
      AWS_ACCESS_KEY_ID: walgit-dev
      AWS_SECRET_ACCESS_KEY: walgit-dev-secret
    ports: ["8080:8080"]
```

**Env-overlay gotchas encoded here** (each one broke a real boot):
- Nested config (`store.s3.endpoint`) needs EVERY segment in the env name —
  `WALHUB__STORE__ENDPOINT` is silently recorded as an unknown key and ignored.
- rustfs/MinIO require `force_path_style: true`; bucket-virtual-host addressing
  403s (`InvalidAccessKeyId`-class errors against the wrong host).
- Healthchecks pin `127.0.0.1` — `localhost` resolves to `::1` in busybox and
  rustfs binds IPv4 only.
- pnpm 11 refuses dependency build scripts unless `pnpm-workspace.yaml`
  declares `allowBuilds: { esbuild: true }` (the web stage's `pnpm install`
  fails otherwise).

```## 7.2 `Makefile` (repo root) — the ONLY task runner (D3)

```make
BINARY  := walhub
SHA     ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo dev)
GO      := CGO_ENABLED=0 go
LDFLAGS := -s -w -X main.buildSHA=$(SHA)

build: web                      ## static binary; web/ embedded (dist/repos.js built by `make web`)
	$(GO) build -trimpath -buildvcs=stamp -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/walhub

vet:
	go vet ./...

fmt:
	gofmt -w .

test:                           ## fast tier; -race needs cgo, so CGO stays on here
	CGO_ENABLED=1 go test -race ./...

race: test                      ## alias, named for the flag

js-test:                        ## headless JS logic tests — Node's own runner, zero npm deps
	node --test web/test/

e2e:                            ## smart-HTTP end-to-end against the real git binary
	CGO_ENABLED=1 go test -race -tags e2e ./internal/server/... -run TestE2E

sim:                            ## WAL crash/replay simulation (see 08_wal.md)
	CGO_ENABLED=1 go test -race -tags sim ./internal/wal/... -run TestSim

contract:                       ## store contract suite; filesystem + memory ALWAYS run (D4)
	CGO_ENABLED=1 go test -race ./internal/store/...

cover:                          ## D7 gate: >= 95% statements per internal/... package
	@set -e; for pkg in $$($(GO) list ./internal/...); do \
	  CGO_ENABLED=1 go test -count=1 -cover $$pkg \
	    | awk -v p=$$pkg '/coverage:/ { gsub(/[^0-9.]/,"",$$2); \
	        if ($$2+0 < 95) { print "FAIL " p ": " $$2 "%"; exit 1 } }'; \
	done

dev: build                      ## zero-config loop: filesystem store, no bucket, /setup on :8080
	./$(BINARY) serve

dev-store:                      ## rustfs + bucket — ONLY for the bucket-backend contract job
	docker compose up -d rustfs && docker compose run --rm create-bucket

test-s3:                        ## store contract against local rustfs (needs dev-store)
	WALHUB_TEST_S3_ENDPOINT=http://127.0.0.1:9000 \
	WALHUB_TEST_S3_BUCKET=walgit-test \
	go test -tags s3 ./internal/store/... -run TestS3Contract

image:                          ## two-stage build with the real sha
	docker build --build-arg WALHUB_BUILD_SHA=$(SHA) -t walhub .

clean:
	rm -f $(BINARY) cover.out

ci: vet test js-test cover e2e contract   ## what CI runs; image build happens in CI
```

(Make is canonical and exclusive — there is no justfile. `dev-store` exists only so `test-s3` and CI's `s3-contract` job have a bucket; the zero-config dev loop never touches it.)

## 8. CI pipeline

The repo lives on Forgejo; CI is Woodpecker-shaped (no GitHub Actions). Pipeline = vet → web (SDK bundle) → test `-race` → js-test (`node --test`) → cover gate (D7) → e2e → store contract (filesystem + memory ALWAYS per D4; rustfs service for the optional S3 job) → image build. The only web build is the SDK bundle (`make web`); JS tests import source and need no build.

```yaml
# .woodpecker/pipeline.yaml
when: { event: [push, pull_request] }

steps:
  vet:   { image: golang:1.25, commands: [make vet]  }
  test:  { image: golang:1.25, commands: [make test] }   # golang image ships git + gcc (-race)
  js-test: { image: node:22-alpine, commands: [node --test web/test/] }
  cover: { image: golang:1.25, commands: [make cover], depends_on: [test] }
  e2e:   { image: golang:1.25, commands: [make e2e],   depends_on: [test] }
  contract: { image: golang:1.25, commands: [make contract], depends_on: [test] }

  s3-contract:
    image: golang:1.25
    commands: [make test-s3]
    depends_on: [test]
    environment:
      WALHUB_TEST_S3_ENDPOINT: http://rustfs:9000
      WALHUB_TEST_S3_BUCKET: walgit-test

  image:
    image: plugins/docker
    settings: { repo: git.packden.us/crueber/walhub, tags: "${CI_COMMIT_SHA:0:12},latest" }
    depends_on: [test, js-test, cover, e2e, contract, s3-contract]

services:
  rustfs:
    image: rustfs/rustfs:latest
    environment:
      RUSTFS_ACCESS_KEY: walgit-dev
      RUSTFS_SECRET_KEY: walgit-dev-secret
```

CI MUST also verify the container contract: after `image`, run `podman run --rm <img> git --version` and fail if < 2.47, plus a `walhub config check --strict` against the example config (catches a baked default that no longer validates).

The GitHub mirror (`github.com/crueber/walhub`, auto-pushed from the Forgejo origin) additionally
runs `.github/workflows/docker.yml`: a vet + fast-test + headless-JS smoke job, then buildx
publishes `ghcr.io/crueber/walhub` (linux/amd64; tags `latest` + `main` from main, semver from
`v*` tags, `sha-<sha>` per commit; GHA layer cache). That workflow is the only GHCR publisher;
users pull the image per README "Run with docker compose".

## 9. Onboarding a developer (30-second quickstart)

```sh
# Prerequisites: docker (or Go 1.25 to build the binary); git >= 2.47 wherever the
# server runs, >= 2.46 on client machines.
git clone https://git.packden.us/crueber/walhub && cd walhub

# Zero-config: no TOML, no bucket, no TLS. Filesystem store inside the data dir.
docker run --rm -p 8080:8080 walhub       # or: make build && ./walhub serve
# The boot log warns that auth.mode = "none" and prints the setup banner.

# Open http://host:8080/setup — every config section with its current effective
# value. Save writes <data-dir>/walhub.toml atomically and lists the keys that
# need a restart. Or change nothing and keep the defaults: also a valid choice.

# First push creates the repo (auto_create_on_push defaults to true):
mkdir demo && cd demo && git init -b main && echo hello > README.md
git add . && git commit -m init
git remote add origin http://host:8080/$USER/demo.git
git push -u origin main

# Clone from another terminal/dir — a fresh clone rides bundle-uri:
git clone http://host:8080/$USER/demo.git
```

TLS, OIDC, and bucket-backed shapes are deliberate upgrades, not defaults — §3.2/§3.3 (OPTIONAL config files) and §4 (nginx edge).

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
- **~~Node 20 (not 24) + pnpm@10 for the web stage~~ — SUPERSEDED twice by divergence D2 (2026-08-31): first removed entirely (zero-build), then restored in minimal form when the user directed a build step for the modular SDK — a node stage exists ONLY to run esbuild (`pnpm run build:sdk` → `web/dist/repos.js`).** The frontend itself is zero-build vanilla ES modules embedded raw from `web/`; TypeScript, SolidJS, and vite remain gone.
- **Go release flags `-trimpath -buildvcs=stamp -ldflags "-s -w"` replace thin LTO + unwind-panic profile** — Go has no LTO; per-request/per-pass `recover` reproduces the "one panic must not kill the instance" guarantee (§6).
- **Startup git version check is fatal (exit 1) below 2.47** — the Rust spec declares ≥ 2.47 required for server features; failing fast with a message beats mysterious wire breakage.
- **Header names stay `X-Walgit-*` (including the `walgit_session` cookie in the auth cache key) and the token prefix stays `wgt_`** — the nginx contract and users' stored credentials are the edge-compat surface; renaming would silently invalidate deployed edge configs and credential helpers.
- **Dev-rig rustfs keys/bucket keep the Rust values (`walgit-dev` / `walgit-dev-secret`, bucket `walgit-test`)** — muscle-memory continuity for contributors coming from the Rust rig; local-only credentials.
- **Nix flake packaging not ported** — the flake was a Rust toolchain convenience; Go's static cross-build plus the Dockerfile covers the same ground without a second packaging system (revisit only on demand).
- **CI is a Makefile + Woodpecker pipeline, not GitHub Actions** — the repo lives on Forgejo; Woodpecker's services block covers the rustfs contract job.
- **`install.sh` is served from the Forgejo raw URL** — the Rust edge example leaves the installer out of the open-route set; keeping it repo-hosted avoids inventing a new public server route in the Go rewrite.
- **Divergence (2026-08-31), applied in this revision:**
- **D2 — SUPERSEDED 2026-09-02 by explicit user request (DEVIATIONS.md D-WEB-6): the frontend is a SolidJS + Tailwind v4 SPA built by vite into `web/dist/` (plain JSX, no TypeScript; runtime deps exactly solid-js + @solidjs/router; dark by default; no CDN); the node stage now runs `vite build` plus the esbuild SDK bundle. Historical:** the frontend was vanilla ESM; the node stage was minimal. `web/` is hand-written ES modules (native import map, `<template>`, ~40 lines of reactive helpers) embedded RAW — no TypeScript, no framework; SolidJS/vite are gone, superseding the "Node 20 + pnpm@10" decision above. The SDK is **authored as submodules** (`web/sdk/src/*.js`) and esbuild-bundled to `web/dist/repos.js` (the `repos.mjs` twin is deleted; `dist/` is a build artifact, `dist/.keep` committed for the embed); JS tests run on Node's built-in `node --test` with zero runtime npm dependencies and import source, never the build.
- **D3 — Make is the only task runner.** All dev/CI entry points are Make targets (`build vet fmt test race js-test e2e sim contract cover dev dev-store test-s3 image clean ci`); the "`just` is fine too" note is retracted — there is no justfile.
- **D4 — filesystem store backend is first-class.** `store.backend = "filesystem"` roots keys under `store.root` with sidecar-`flock` conditional writes, `renameat2(RENAME_NOREPLACE)` create-if-absent (portable fallback), `"<size>:<mtime_ns>"` version tokens/ETags, and no accel/signed URLs. It joins the contract suite as an ALWAYS-run backend beside memory (Make `contract`); the container's default path uses it; rustfs remains only as the bucket-contract fixture.
- **D5 — zero-config first run (user friendliness is a first-class law).** A missing config boots on built-in defaults — `listen 0.0.0.0:8080`, filesystem store rooted at `<data-dir>/store`, `auth.mode = "none"` on ANY bind with loud warnings (a deliberate reversal of the Rust fail-closed loopback rule), `auto_create_on_push = true`; exit code 2 is now bad-argv only. Data dir = `--data-dir` / `WALHUB_DATA_DIR` (default `~/.local/share/walhub`, containers `/var/lib/walhub`), holding `store/`, `cache/`, and the saved `walhub.toml`.
- **D6 — setup UI + API are first-class.** `/setup` groups every config key by section with effective values; `GET /api/v1/setup`, `POST /api/v1/setup/test` (validate only), `PUT /api/v1/setup` (validate + atomic write of `<data-dir>/walhub.toml`, reporting restart-needed keys). Invalid config → setup-only mode: only `/setup`, `/healthz`, `/readyz` answer, everything else 503. Access: open while (no config file OR config invalid OR auth.mode = none), else admin principal; optional `WALHUB_SETUP_TOKEN` gate for exposed hosts.
- **D7 — coverage gate.** CI fails below 95% statement coverage per `internal/...` package (Make `cover`; `cmd/` main glue excluded); the pipeline gains `cover` + `js-test` steps; table-driven httptest for every handler.
