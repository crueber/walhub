# Dockerfile — one binary in front of a store. Zero config by default: filesystem store in the data dir.
#   docker build -t walhub .
#   docker run --rm -p 8080:8080 walhub     # → open http://host:8080/setup
#
# Compose examples: compose.standalone.yml (filesystem store) and
# compose.yaml (walhub + rustfs, the S3-backed stack).

# ---- 1. Web: the ONE build step — vite builds the SolidJS SPA into dist/,
#         esbuild bundles web/sdk/src → dist/repos.js (D-WEB-6) ----
FROM docker.io/library/node:22-alpine AS web
RUN corepack enable && corepack prepare pnpm@11.25.0 --activate
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml ./
# --ignore-scripts: esbuild's postinstall only validates its platform
# binary (shipped as an optional dependency); pnpm 11's build-script gate
# would otherwise fail the fresh install.
RUN pnpm install --frozen-lockfile --ignore-scripts
COPY web/ ./
RUN pnpm run build \
    && test -f dist/index.html \
    && test -f dist/repos.js

# ---- 2. Go build (static, no cgo; embeds the built dist/) ----
FROM docker.io/library/golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The web stage's dist/ (vite SPA + SDK bundle) replaces whatever the
# context carried — the Go build embeds it (web/embed.go all:dist).
COPY --from=web /src/web/dist /src/web/dist
ARG WALHUB_BUILD_SHA=dev
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=auto \
      -ldflags "-s -w -X main.buildSHA=${WALHUB_BUILD_SHA}" \
      -o /out/bin/walhub ./cmd/walhub

# ---- 3. runtime: git >= 2.47, CA certs, nonroot ----
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
