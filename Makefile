# -----------------------------
# CONFIG
# -----------------------------
-include .env

APP_NAME      ?= iam-audit-log
APP_ENV       ?= dev
GO            ?= go
BUILD_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Private modules — resolved via SSH (git@github.com:) using the global URL
# rewrite; CI uses GO_PRIVATE_TOKEN instead.
export GOPRIVATE  ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*
export GONOSUMDB  ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*

export APP_ENV BUILD_VERSION

TEST_UNIT_PKGS     := ./test/unit/...
# cmd/* and the postgres adapter carry integration-tagged tests against a real
# testcontainers Postgres, so they run in the postgres tier.
TEST_POSTGRES_PKGS := ./test/postgres/... ./cmd/server/... ./cmd/reconciler/... ./internal/adapter/outbound/postgres/...
TEST_INT_PKGS      := ./test/integration/...
TEST_E2E_PKGS      := ./test/e2e/...

TEST_POSTGRES_PARALLEL    ?= 4
TEST_INTEGRATION_PARALLEL ?= 2

# White-box tests next to the code.
TEST_INTERNAL_PKGS := ./cmd/... ./internal/... ./pkg/... ./api/...

COVER_PKG_LIST := $(shell $(GO) list ./cmd/... ./internal/... ./pkg/... 2>/dev/null | tr '\n' ',' | sed 's/,$$//')

ALL_TEST_TAGS := integration,e2e

# -----------------------------
# SETUP
# -----------------------------

.PHONY: setup
setup: install-hooks
	@test -f .env || cp .env-example .env
	@echo "Environment ready (.env)"

.PHONY: install-hooks
install-hooks:
	@mkdir -p .git/hooks
	@cp .githooks/pre-commit .git/hooks/pre-commit
	@chmod +x .git/hooks/pre-commit
	@echo "Installed git hooks"

.PHONY: help
help:
	@echo "Available commands:"
	@echo "  make setup            - copy .env-example to .env if missing; install git hooks"
	@echo "  make tidy / fmt / fmt-check / vet / lint"
	@echo "  make arch-lint        - go-arch-lint + grep gates (LLD §3.2)"
	@echo "  make invariant-lint   - AL-INV-1 grants, AL-INV-10 no publisher, receive-only AsyncAPI, metric naming"
	@echo "  make test-unit        - unit tests (no Docker)"
	@echo "  make test-postgres    - Postgres/RLS/partition tests (testcontainers)"
	@echo "  make test-integration - SQS/Glue/S3 integration tests (testcontainers + floci)"
	@echo "  make test-e2e         - end-to-end tests"
	@echo "  make test-ci          - unit + postgres + integration with -race + merged coverage"
	@echo "  make run              - run cmd/server locally"
	@echo "  make run-reconciler JOB=<name> - run one reconciler job locally"
	@echo "  make build            - compile both binaries to bin/"
	@echo "  make cover-func       - coverage summary by function"
	@echo "  make pin-base-images  - fetch + pin SHA digests for Dockerfile base images"
	@echo "  make docker-up / docker-down - local Postgres + floci"
	@echo "  make swag / swag-check - generate / verify docs/swagger"
	@echo "  make ci               - everything CI runs"

# -----------------------------
# GO BASICS
# -----------------------------

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: fmt
fmt:
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l cmd/ internal/ pkg/ api/ test/ 2>/dev/null); \
	if [ -n "$$unformatted" ]; then echo "FAIL: unformatted files:"; echo "$$unformatted"; exit 1; fi
	@echo "gofmt: all files formatted"

.PHONY: vet
vet:
	$(GO) vet ./...
	$(GO) vet -tags=$(ALL_TEST_TAGS) ./...

.PHONY: mod-verify
mod-verify:
	$(GO) mod verify

.PHONY: vuln-check
vuln-check:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./internal/... ./pkg/...

# -----------------------------
# LINT
# -----------------------------

.PHONY: lint
lint:
	$(GO) tool golangci-lint run ./...
	$(GO) tool golangci-lint run --build-tags=$(ALL_TEST_TAGS) ./...

.PHONY: arch-lint
arch-lint:
	bash .github/scripts/arch-lint.sh

.PHONY: invariant-lint
invariant-lint:
	bash .github/scripts/check-grants.sh
	bash .github/scripts/check-forbidden-events-bypass.sh
	bash .github/scripts/check-asyncapi-receive-only.sh
	bash .github/scripts/check-metric-naming.sh
	bash .github/scripts/check-observability-confinement.sh

# -----------------------------
# TESTS
# -----------------------------

.PHONY: _test-unit
_test-unit: | .coverage
	$(GO) test $(TEST_UNIT_PKGS) $(TEST_INTERNAL_PKGS) \
	  -race -count=1 -timeout 120s \
	  -coverpkg=$(COVER_PKG_LIST) -coverprofile=.coverage/unit.out

.PHONY: _test-postgres
_test-postgres: | .coverage
	$(GO) test $(TEST_POSTGRES_PKGS) \
	  -tags=integration -race -count=1 -timeout 300s -parallel $(TEST_POSTGRES_PARALLEL) \
	  -coverpkg=$(COVER_PKG_LIST) -coverprofile=.coverage/postgres.out

.PHONY: _test-integration
_test-integration: | .coverage
	$(GO) test $(TEST_INT_PKGS) \
	  -tags=integration -race -count=1 -timeout 600s -parallel $(TEST_INTEGRATION_PARALLEL) \
	  -coverpkg=$(COVER_PKG_LIST) -coverprofile=.coverage/integration.out

.PHONY: _merge-coverage
_merge-coverage:
	@python3 scripts/merge_coverage.py \
	  .coverage/unit.out .coverage/postgres.out .coverage/integration.out > coverage.out
	@echo "==> coverage.out merged from all suites"

.PHONY: test-ci
test-ci: | .coverage
	$(MAKE) -j3 _test-unit _test-postgres _test-integration
	$(MAKE) _merge-coverage

.PHONY: test
test: test-unit test-postgres test-integration

.PHONY: test-unit
test-unit:
	$(GO) test $(TEST_UNIT_PKGS) $(TEST_INTERNAL_PKGS) -count=1 -timeout 120s

.PHONY: test-postgres
test-postgres:
	$(GO) test $(TEST_POSTGRES_PKGS) -tags=integration -count=1 -timeout 300s -parallel $(TEST_POSTGRES_PARALLEL) -v

.PHONY: test-integration
test-integration:
	$(GO) test $(TEST_INT_PKGS) -tags=integration -count=1 -timeout 600s -parallel $(TEST_INTEGRATION_PARALLEL) -v

.PHONY: test-e2e
test-e2e:
	$(GO) test $(TEST_E2E_PKGS) -tags=e2e -count=1 -timeout 600s -v

.PHONY: race
race:
	$(MAKE) -j3 _test-unit _test-postgres _test-integration

# -----------------------------
# RUN / BUILD
# -----------------------------

.PHONY: run
run:
	bash -c 'set -a && source .env && set +a && BUILD_VERSION=$(BUILD_VERSION) $(GO) run ./cmd/server'

.PHONY: run-reconciler
run-reconciler:
	@test -n "$(JOB)" || { echo "Usage: make run-reconciler JOB=<job-name>"; exit 1; }
	bash -c 'set -a && source .env && set +a && $(GO) run ./cmd/reconciler --job=$(JOB)'

.PHONY: build
build:
	@mkdir -p bin
	$(GO) build -trimpath -ldflags "-s -w -X main.buildVersion=$(BUILD_VERSION)" -o bin/$(APP_NAME)-server ./cmd/server
	$(GO) build -trimpath -ldflags "-s -w -X main.buildVersion=$(BUILD_VERSION)" -o bin/$(APP_NAME)-reconciler ./cmd/reconciler

# -----------------------------
# MIGRATIONS (the server self-migrates at startup via pgcommon/pkg/migrate;
# these targets are local-dev conveniences only, LLD §4.4)
# -----------------------------

MIGRATIONS_DIR := internal/adapter/outbound/postgres/migrations

.PHONY: migrate-create
migrate-create:
	@test -n "$(NAME)" || { echo "Usage: make migrate-create NAME=<description>"; exit 1; }
	docker run --rm -v $(CURDIR)/$(MIGRATIONS_DIR):/migrations \
	  migrate/migrate create -ext sql -dir /migrations -seq -digits 6 $(NAME)

# -----------------------------
# DOCKER
# -----------------------------

.PHONY: docker-up
docker-up:
	docker compose up -d postgres floci

.PHONY: docker-down
docker-down:
	docker compose down

# -----------------------------
# SWAGGER
# -----------------------------

.PHONY: swag
swag:
	$(GO) run github.com/swaggo/swag/cmd/swag@v1.16.6 init \
	  -g swagger_info.go -d cmd/server,internal/adapter/inbound/http \
	  --output docs/swagger --parseDependency --parseInternal

.PHONY: swag-check
swag-check:
	bash .github/scripts/check-swagger-stale.sh

# -----------------------------
# CI
# -----------------------------

.PHONY: ci
ci: tidy fmt-check vet lint arch-lint invariant-lint test-ci build

.PHONY: cover
cover: test-ci
	$(GO) tool cover -html=coverage.out

.PHONY: cover-func
cover-func: test-ci
	$(GO) tool cover -func=coverage.out

# pin-base-images: fetch and pin the current SHA digests for the Dockerfile
# base images (mirrors iam-org-membership). Writes the digests both to the
# Dockerfile FROM lines and to .docker-digests (a checked-in provenance
# record). validate-quality.yml rejects any unpinned FROM line.
GOLANG_IMAGE     := golang:1.26.6-bookworm
DISTROLESS_IMAGE := gcr.io/distroless/static-debian12:nonroot

.PHONY: pin-base-images
pin-base-images:
	@echo "Fetching SHA digests for Dockerfile base images..."
	@GOLANG_DIGEST=$$(docker buildx imagetools inspect $(GOLANG_IMAGE) --format '{{.Manifest.Digest}}') && \
	 DISTROLESS_DIGEST=$$(docker buildx imagetools inspect $(DISTROLESS_IMAGE) --format '{{.Manifest.Digest}}') && \
	 sed -i.bak -E \
	   -e "s|FROM $(GOLANG_IMAGE)(@sha256:[a-f0-9]+)?|FROM $(GOLANG_IMAGE)@$$GOLANG_DIGEST|" \
	   -e "s|FROM $(DISTROLESS_IMAGE)(@sha256:[a-f0-9]+)?|FROM $(DISTROLESS_IMAGE)@$$DISTROLESS_DIGEST|" \
	   Dockerfile && rm -f Dockerfile.bak && \
	 echo "$(GOLANG_IMAGE) $$GOLANG_DIGEST" > .docker-digests && \
	 echo "$(DISTROLESS_IMAGE) $$DISTROLESS_DIGEST" >> .docker-digests && \
	 echo "Digests written to .docker-digests — commit both Dockerfile and .docker-digests"

.coverage:
	@mkdir -p .coverage

.PHONY: clean
clean:
	rm -rf bin .coverage coverage.out coverage.html
