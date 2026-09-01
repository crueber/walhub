# Containerfile — one binary in front of a store. Zero config by default: filesystem store in the data dir.
#   podman build -t walhub -f Containerfile .
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
