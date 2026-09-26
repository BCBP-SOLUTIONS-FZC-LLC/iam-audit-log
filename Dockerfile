# syntax=docker/dockerfile:1.7
#
# iam-audit-log
#
# Multi-stage build producing a minimal, non-root, distroless runtime image
# carrying BOTH binaries this service ships (LLD §3, §13):
#   /iam-audit-log-server       query + ingest API (AL-1..AL-7) and the
#                               11-queue SQS consumer fleet, in one process.
#                               The image's default ENTRYPOINT.
#   /iam-audit-log-reconciler   archival / partition / prune CronJob entry
#                               points, selected via --job=<name>. No
#                               listener, no ports.
#
# One image: the Deployment runs it unmodified; each CronJob
# (deploy/helm/templates/cronjobs.yaml) overrides `command` against the SAME
# image reference. Mirrors iam-realm-provisioner's Dockerfile.

########################################
# Stage: builder
########################################
FROM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS builder

ARG BUILD_VERSION=dev
ARG SOURCE_DATE_EPOCH
ENV SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH}

WORKDIR /src

COPY go.mod go.sum ./
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ENV GOPRIVATE="github.com/BCBP-SOLUTIONS-FZC-LLC/*"
ENV GONOSUMDB="github.com/BCBP-SOLUTIONS-FZC-LLC/*"

# Private platform-* modules: token passed as a BuildKit secret, never a
# layer (git config is unset in the same RUN).
RUN --mount=type=secret,id=go_private_token \
    TOKEN=$(cat /run/secrets/go_private_token 2>/dev/null || true) && \
    if [ -z "$TOKEN" ]; then echo "ERROR: go_private_token secret is missing or empty — pass --secret id=go_private_token,src=<token-file>"; exit 1; fi && \
    git config --global url."https://x-access-token:${TOKEN}@github.com/".insteadOf "https://github.com/" && \
    go mod download && \
    git config --global --unset url."https://x-access-token:${TOKEN}@github.com/".insteadOf

COPY . .

RUN CGO_ENABLED=0 GOOS=linux \
    GOPRIVATE="github.com/BCBP-SOLUTIONS-FZC-LLC/*" \
    go build -trimpath -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" -o /out/iam-audit-log-server ./cmd/server && \
    go build -trimpath -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" -o /out/iam-audit-log-reconciler ./cmd/reconciler

########################################
# Stage: runtime
########################################
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS runtime

ARG BUILD_VERSION=dev
ENV BUILD_VERSION=${BUILD_VERSION}

LABEL org.opencontainers.image.title="iam-audit-log" \
      org.opencontainers.image.description="IAM Audit Log Service — append-only compliance system-of-record (query/ingest API, SQS consumer fleet, archival reconciler)" \
      org.opencontainers.image.source="https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log" \
      org.opencontainers.image.vendor="BCBP Solutions" \
      org.opencontainers.image.licenses="Proprietary" \
      org.opencontainers.image.base.name="gcr.io/distroless/static-debian12:nonroot" \
      org.opencontainers.image.revision="${BUILD_VERSION}"

WORKDIR /
COPY --from=builder /out/iam-audit-log-server /iam-audit-log-server
COPY --from=builder /out/iam-audit-log-reconciler /iam-audit-log-reconciler

USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/iam-audit-log-server"]
