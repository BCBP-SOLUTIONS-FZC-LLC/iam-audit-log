# -----------------------------
# CONFIG
# -----------------------------
-include .env

APP_NAME      ?= iam-audit-log
APP_ENV       ?= dev
GO            ?= go
BUILD_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Private modules — resolved via SSH (git@github.com:) using the global URL
# rewrite. No tokens needed for local dev; an SSH key registered with the
# BCBP-SOLUTIONS-FZC-LLC org is required. CI uses GO_PRIVATE_TOKEN instead.
export GOPRIVATE  ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*
export GONOSUMDB  ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*

# Docker socket for testcontainers-go (Mac Docker Desktop uses a user socket).
DOCKER_SOCKET     := $(shell [ -S /Users/$(USER)/.docker/run/docker.sock ] && echo unix:///Users/$(USER)/.docker/run/docker.sock || echo unix:///var/run/docker.sock)
export DOCKER_HOST ?= $(DOCKER_SOCKET)

export APP_NAME APP_ENV BUILD_VERSION

# Test package groups (explicit to handle per-group build tags cleanly).
TEST_UNIT_PKGS     := ./test/unit/...
# cmd/* and the postgres adapter carry integration-tagged tests against a real
# testcontainers Postgres, so they run in the postgres tier.
TEST_POSTGRES_PKGS := ./test/postgres/... ./cmd/server/... ./cmd/reconciler/... ./internal/adapter/outbound/postgres/...
TEST_INT_PKGS      := ./test/integration/...
TEST_E2E_PKGS      := ./test/e2e/...

# Postgres-tier tests call t.Parallel() and each spins its own testcontainers
# Postgres (create+migrate+grant) — capped, not GOMAXPROCS-wide, so we don't
# spin 10+ containers at once on a dev machine and reintroduce Docker
# resource-contention flakes. CI runs with TEST_POSTGRES_PARALLEL=1
# (validate-test.yml). Override on the CLI on a bigger box, e.g.
# `make test-postgres TEST_POSTGRES_PARALLEL=8`.
TEST_POSTGRES_PARALLEL ?= 4
# The integration tier shares ONE floci per run (test/integration/
# harness_test.go); keep this low so queue/DLQ assertions stay deterministic.
TEST_INTEGRATION_PARALLEL ?= 2

# Every build tag the test/ tree declares (integration: test/postgres +
# test/integration + cmd/* + the postgres adapter, e2e: test/e2e). vet and
# lint run a second pass with all of them — CI's quality gate calls
# `make vet`/`make lint`, so without it the tagged test files would never be
# vetted or linted.
ALL_TEST_TAGS := integration,e2e

# White-box (package-internal) tests included in unit runs.
TEST_INTERNAL_PKGS := ./cmd/... ./internal/... ./pkg/... ./api/...

COVER_PKG_LIST := $(shell $(GO) list ./cmd/... ./internal/... ./pkg/... 2>/dev/null | tr '\n' ',' | sed 's/,$$//')

# Dockerfile base images (pinned by `make pin-base-images`).
GOLANG_IMAGE     := golang:1.26.6-bookworm
DISTROLESS_IMAGE := gcr.io/distroless/static-debian12:nonroot

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

# godoc: serve package documentation locally using pkgsite.
# Opens http://localhost:8080 — browse to the module path in the UI.
.PHONY: godoc
godoc:
	@echo "Starting pkgsite at http://localhost:8080 — press Ctrl-C to stop"
	$(GO) run golang.org/x/pkgsite/cmd/pkgsite@latest -open .

# pin-base-images: fetch and pin the current SHA digests for Dockerfile base
# images. Writes the digests both to the Dockerfile FROM lines and to
# .docker-digests (a checked-in provenance record). validate-quality.yml
# rejects any unpinned FROM line. Re-running replaces an existing digest.
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

.PHONY: help
help:
	@echo "Available commands:"
	@echo "  make setup            - copy .env-example to .env if missing; install git hooks"
	@echo "  make tidy             - go mod tidy"
	@echo "  make fmt              - go fmt ./..."
	@echo "  make fmt-check        - verify gofmt formatting (mirrors CI)"
	@echo "  make vet              - go vet (default build + every test build tag)"
	@echo "  make lint             - run golangci-lint (default build + every test build tag)"
	@echo "  make arch-lint        - go-arch-lint against .go-arch-lint.yml + grep gates (LLD §3.2)"
	@echo "  make invariant-lint   - AL-INV-1 grants, platform-events/gincommon confinement, receive-only AsyncAPI, metric naming"
	@echo "  make test             - unit + postgres + integration tests (requires Docker)"
	@echo "  make test-ci          - test with race detector + merged coverage (used in CI; 95% gate)"
	@echo "  make test-unit        - unit tests only (no Docker required)"
	@echo "  make test-postgres    - Postgres/RLS/grant/partition tests (testcontainers)"
	@echo "  make test-integration - SQS/Glue/S3 integration tests (testcontainers + floci)"
	@echo "  make test-e2e         - end-to-end tests against the real binaries (requires Docker)"
	@echo "  make test-smoke       - CI-only image gate: size <=200MB + startup-gate check (needs IMAGE_TAG/BINARY)"
	@echo "  make race             - all tests with -race flag"
	@echo "  make run              - run cmd/server locally (sources .env)"
	@echo "  make run-reconciler JOB=<name> - run one reconciler job locally"
	@echo "                          (reconcile | redaction-retry | redaction-sweep | processed-events-prune)"
	@echo "  make build            - compile both binaries to bin/"
	@echo "  make migrate-create NAME=<desc> - scaffold the next NNNNNN_<desc>.{up,down}.sql"
	@echo "  make cover            - coverage HTML report"
	@echo "  make cover-func       - coverage summary by function"
	@echo "  make ci               - tidy + fmt-check + vet + lint + arch-lint + invariant-lint + test-ci + build"
	@echo "  make docker-up        - start local Postgres + floci (SNS/SQS/S3/Glue)"
	@echo "  make docker-down      - stop local containers"
	@echo "  make mod-verify       - go mod verify"
	@echo "  make vuln-check       - govulncheck on internal + pkg"
	@echo "  make prometheusrule   - regenerate the Helm PrometheusRule from deploy/monitoring/"
	@echo "  make swag             - regenerate docs/swagger/ from handler annotations"
	@echo "  make swag-check       - fail if Swagger regeneration would change docs/swagger/ (CI drift gate)"
	@echo "  make docs-serve       - run the server and print the doc URLs"
	@echo "  make install-hooks    - install .githooks/pre-commit into .git/hooks"
	@echo "  make godoc            - serve local godoc/pkgsite at http://localhost:8080"
	@echo "  make pin-base-images  - fetch + pin SHA digests for Dockerfile base images"
	@echo "  make clean            - remove build artefacts"

# -----------------------------
# GO BASICS
# -----------------------------

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: fmt
fmt:
	$(GO) fmt ./...

.PHONY: vet
vet:
	$(GO) vet ./...
	$(GO) vet -tags=$(ALL_TEST_TAGS) ./...

# fmt-check scopes to the Go source trees (not `.`) so untracked reference
# material in the working tree never fails the gate.
.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l cmd/ internal/ pkg/ api/ test/ 2>/dev/null); \
	if [ -n "$$unformatted" ]; then \
		echo "FAIL: unformatted files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	@echo "gofmt: all files formatted"

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
	@echo "Running linter..."
	$(GO) tool golangci-lint run ./...
	$(GO) tool golangci-lint run --build-tags=$(ALL_TEST_TAGS) ./...

# arch-lint: enforce .go-arch-lint.yml component boundaries plus the grep
# gates (incl. the pgcommon-only database invariant, gap 44) — the same
# script CI's validate-quality.yml runs.
.PHONY: arch-lint
arch-lint:
	bash .github/scripts/arch-lint.sh

# invariant-lint: the service-specific CI gates —
#   check-grants.sh                     AL-INV-1 (audit_app INSERT+SELECT only)
#   check-forbidden-events-bypass.sh    AL-INV-10 + platform-events-only (gap 45)
#   check-asyncapi-receive-only.sh      zero send operations (AL-INV-10)
#   check-metric-naming.sh              metric naming + registry (gap 46)
#   check-observability-confinement.sh  platform-gincommon-only (gap 43)
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
	  -coverpkg=$(COVER_PKG_LIST) \
	  -coverprofile=.coverage/unit.out

# The postgres tier is the longest and noisiest log; on failure, re-print just
# the --- FAIL lines at the end so they aren't buried.
.PHONY: _test-postgres
_test-postgres: | .coverage
	{ $(GO) test $(TEST_POSTGRES_PKGS) \
	  -tags=integration -race -count=1 -timeout 300s -parallel $(TEST_POSTGRES_PARALLEL) \
	  -coverpkg=$(COVER_PKG_LIST) \
	  -coverprofile=.coverage/postgres.out \
	  2>&1; echo $$? >.coverage/postgres.exitcode; } | tee .coverage/postgres.raw; \
	_exit=$$(cat .coverage/postgres.exitcode 2>/dev/null || echo 1); \
	[ "$$_exit" = "0" ] || { \
	  printf '\n\n=== FAILING POSTGRES TESTS (see full log above for details) ===\n'; \
	  grep '^--- FAIL:' .coverage/postgres.raw || printf '(no --- FAIL lines — check for DATA RACE or panic above)\n'; \
	  printf '=============================================================\n\n'; \
	}; \
	exit "$$_exit"

.PHONY: _test-integration
_test-integration: | .coverage
	$(GO) test $(TEST_INT_PKGS) \
	  -tags=integration -race -count=1 -timeout 600s -parallel $(TEST_INTEGRATION_PARALLEL) \
	  -coverpkg=$(COVER_PKG_LIST) \
	  -coverprofile=.coverage/integration.out

.PHONY: test
test:
	$(MAKE) -j3 _test-unit-plain _test-postgres-plain _test-integration-plain

.PHONY: _test-unit-plain _test-postgres-plain _test-integration-plain
_test-unit-plain:
	$(GO) test $(TEST_UNIT_PKGS) $(TEST_INTERNAL_PKGS) -count=1 -timeout 120s -v
_test-postgres-plain:
	$(GO) test $(TEST_POSTGRES_PKGS) -tags=integration -count=1 -timeout 300s -parallel $(TEST_POSTGRES_PARALLEL) -v
_test-integration-plain:
	$(GO) test $(TEST_INT_PKGS) -tags=integration -count=1 -timeout 600s -parallel $(TEST_INTEGRATION_PARALLEL) -v

# Merge the three per-suite profiles into a single coverage.out (max-count
# strategy — any suite covering a block wins). coverage-gate.sh reads it.
.PHONY: _merge-coverage
_merge-coverage:
	@python3 scripts/merge_coverage.py \
	  .coverage/unit.out .coverage/postgres.out .coverage/integration.out \
	  > coverage.out
	@echo "==> coverage.out merged from all suites (max-count strategy)"

.PHONY: test-ci
test-ci: | .coverage
	$(MAKE) -j3 _test-unit _test-postgres _test-integration
	$(MAKE) _merge-coverage

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

# test-smoke: ci.yml runs this once per binary against the CI-built image —
# IMAGE_TAG and BINARY are required; ENTRYPOINT=/iam-audit-log-reconciler
# for the reconciler leg (both binaries ship in the same image).
.PHONY: test-smoke
test-smoke:
	bash .github/scripts/smoke-tests.sh

.PHONY: race
race:
	$(MAKE) -j3 _test-unit _test-postgres _test-integration

# -----------------------------
# RUN
# -----------------------------

.PHONY: run
run:
	@-lsof -ti :$${APP_PORT:-8080} | xargs kill -9 2>/dev/null; true
	bash -c 'set -a && source .env && set +a && BUILD_VERSION=$(BUILD_VERSION) $(GO) run ./cmd/server'

# run-reconciler: one job per invocation, exactly as the Helm CronJobs run it
# (`/iam-audit-log-reconciler --job=<name>`), under RECONCILER_DATABASE_URL.
.PHONY: run-reconciler
run-reconciler:
	@test -n "$(JOB)" || { echo "Usage: make run-reconciler JOB=<reconcile|redaction-retry|redaction-sweep|processed-events-prune>"; exit 1; }
	bash -c 'set -a && source .env && set +a && $(GO) run ./cmd/reconciler --job=$(JOB)'

# No mock-servers target: the one synchronous outbound dependency, Catalog
# CAT-I2, is stale-if-error with the D-13 fallback, so the server runs locally
# without it (AL-1/AL-3 fall back to the default plan window).

# -----------------------------
# BUILD
# -----------------------------

.PHONY: build
build:
	@echo "Building binaries..."
	@mkdir -p bin
	$(GO) build -trimpath -ldflags "-s -w -X main.buildVersion=$(BUILD_VERSION)" -o bin/$(APP_NAME)-server ./cmd/server
	$(GO) build -trimpath -ldflags "-s -w -X main.buildVersion=$(BUILD_VERSION)" -o bin/$(APP_NAME)-reconciler ./cmd/reconciler
	@echo "Verifying library packages compile..."
	$(GO) build ./internal/... ./pkg/...

# -----------------------------
# MIGRATIONS
# -----------------------------
# The server self-migrates at startup via pgcommon/pkg/migrate (LLD §4.4), so
# there is no migrate-up target; this is a local-dev scaffolding convenience.
# After creating one, bump the version in test/postgres/migrations_test.go.

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
	@echo "Starting local PostgreSQL (:5548) + floci (SNS/SQS/S3/Glue, :4573)..."
	docker compose up -d postgres floci

.PHONY: docker-down
docker-down:
	@echo "Stopping local containers..."
	docker compose down

# -----------------------------
# CI
# -----------------------------

.PHONY: ci
ci: tidy fmt-check vet lint arch-lint invariant-lint test-ci build

# -----------------------------
# COVERAGE
# -----------------------------

.PHONY: cover
cover: test-ci
	$(GO) tool cover -html=coverage.out

.PHONY: cover-func
cover-func: test-ci
	$(GO) tool cover -func=coverage.out

# -----------------------------
# MONITORING
# -----------------------------
# deploy/monitoring/{app-alerts,recording-rules,slo-rules}.yml are the source
# of truth; the Helm PrometheusRule is generated from them, and a unit test
# fails if the two drift.

.PHONY: prometheusrule
prometheusrule:
	python3 scripts/gen-prometheusrule.py

# -----------------------------
# SCHEMA GOVERNANCE
# -----------------------------
# Intentionally absent (unlike iam-org-membership's platform-schemagov
# targets): this service PUBLISHES NOTHING (AL-INV-10) — it owns no produced
# event schemas to extract, validate, register, verify or prune. The schemas
# it consumes are registered by their producers; the Glue decode goes through
# platform-events. api/asyncapi.yaml is receive-only, gated by
# check-asyncapi-receive-only.sh in `make invariant-lint`.

# -----------------------------
# CLEAN
# -----------------------------

.coverage:
	@mkdir -p .coverage

.PHONY: clean
clean:
	rm -rf bin .coverage
	rm -f coverage.out coverage.html

# -----------------------------
# DOCS
# -----------------------------
# OpenAPI and AsyncAPI specs have different sources of truth:
#   - OpenAPI (docs/swagger/{docs.go,swagger.json,swagger.yaml}) is GENERATED
#     from swag `@Summary`/`@Tags`/`@Router` annotations on handler functions
#     via `make swag` — never hand-edited. It is checked in as a contract
#     artefact; this service mounts no Swagger UI route.
#   - AsyncAPI (api/asyncapi.yaml) IS hand-maintained (receive-only); the
#     service binary embeds it directly via //go:embed in api/ — no synced
#     duplicate copy, so there is nothing to fall out of sync.
# Serves (outside production, or with docs explicitly enabled; token-guarded
# in production):
#   /asyncapi      AsyncAPI rendering of /asyncapi.yaml (Events — read-only)
#   /asyncapi.yaml raw spec
#
# Run `make swag` after changing handler annotations; api/asyncapi.yaml takes
# effect on the next build/run since it's a direct compile-time embed.

# swag: generate the OpenAPI/Swagger 2.0 spec from // @… annotations on
# handlers under cmd/server and internal/adapter/inbound/http. The output
# under docs/swagger/ is checked into the repo; CI's swag-check target fails a
# PR whose annotations drift from what's on disk.
.PHONY: swag
swag:
	@echo "Generating Swagger docs..."
	$(GO) run github.com/swaggo/swag/cmd/swag@v1.16.6 init \
	  -g swagger_info.go \
	  -d cmd/server,internal/adapter/inbound/http \
	  --output docs/swagger \
	  --parseDependency \
	  --parseInternal
	@echo "Swagger docs written to docs/swagger/"

.PHONY: swag-check
swag-check:
	bash .github/scripts/check-swagger-stale.sh

.PHONY: docs-serve
docs-serve: run
	@echo "Docs will be available at:"
	@echo "  http://localhost:8080/asyncapi       — AsyncAPI (Events)"
	@echo "  http://localhost:8080/asyncapi.yaml  — raw AsyncAPI spec"
