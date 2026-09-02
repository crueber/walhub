# walhub — Make targets per 15_testing.md §7 / 16_packaging.md §7.2 (Make is the only task runner).
GO      ?= go
BINARY  ?= walhub
SHA     ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.buildSHA=$(SHA)
T5  := $(shell command -v timeout >/dev/null 2>&1 && echo "timeout 300" || echo "")
T15 := $(shell command -v timeout >/dev/null 2>&1 && echo "timeout 900" || echo "")

.DEFAULT_GOAL := help

web: ## build the UI: vite (SolidJS+Tailwind → dist/) then esbuild (SDK → dist/repos.js)
	pnpm --dir web run build

build: web ## compile everything (the SDK bundle is a build dependency)
	$(GO) build -trimpath -buildvcs=auto -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/walhub

fmt: ## format Go sources
	gofmt -w $$(gofmt -l .)

vet: web ## the Go "-D warnings" gate: gofmt clean, go vet, go build
	test -z "$$(gofmt -l .)" || (echo "gofmt needed:" && gofmt -l . && exit 1)
	$(GO) vet ./...
	$(GO) build ./...

test: test-go test-web ## fast tier = Go fast tests + web tests
test-go: web ## fast Go tier: hermetic, < 1 min, watchdog-wrapped
	$(T5) $(GO) test -short -count=1 ./...
test-web: ## headless JS logic tests + fetch smoke (node --test, imports source)
	node --test web/test/

race: web ## full fast tier under the race detector
	$(T15) $(GO) test -race -short -count=1 ./...

cover: ## coverage gate: >= 95% statements, every internal/... package (per-leaf profiles; e2e harness glue excluded)
	@mkdir -p .cover && rm -f .cover/*.out
	@pkgs=$$($(GO) list -f '{{if .TestGoFiles}}{{.ImportPath}}{{end}}' ./internal/... | grep -v devtools | grep -v "/e2e"); \
	for p in $$pkgs; do \
		name=$$(echo $$p | sed 's|git.packden.us/crueber/walhub/internal/||; s|/|_|g'); \
		$(GO) test -count=1 -coverprofile=.cover/$$name.out "$$p" || exit 1; \
	done
	$(GO) run ./internal/devtools/covergate -dir .cover -min 95

contract: ## store contract suite: memory + filesystem (always run)
	$(GO) test -count=1 ./internal/store/ -run TestContract

contract-s3: ## store contract against rustfs (make dev-store first)
	WALHUB_TEST_S3_ENDPOINT=http://127.0.0.1:9000 $(GO) test -count=1 ./internal/store/ -run TestContractS3

contract-gcs: ## store contract against a real GCS bucket
	WALHUB_TEST_GCS_BUCKET=$${WALHUB_TEST_GCS_BUCKET:?set WALHUB_TEST_GCS_BUCKET} $(GO) test -count=1 ./internal/store/ -run TestContractGCS

e2e: ## smart-HTTP end-to-end against the real git binary
	$(T15) $(GO) test -count=1 ./internal/e2e/...

image: ## build the OCI image
	docker build -t walhub .

dev-store: ## start rustfs (S3-compatible) for bucket-contract tests
	docker compose up -d rustfs

dev-store-stop:
	docker compose down

clean: ## remove build artifacts
	rm -rf $(BINARY) .cover web/dist
	mkdir -p web/dist && touch web/dist/.keep

ci: vet test race cover ## everything that must be green before a merge

help: ## show targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: web build fmt vet test test-go test-web race cover sim contract contract-s3 contract-gcs e2e image dev-store dev-store-stop clean ci help
