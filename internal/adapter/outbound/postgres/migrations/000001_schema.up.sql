-- 000001_schema — LLD §4.1 (extensions, enums) + §4.2 (tables, indexes).
-- Forward-only and additive (O&M MIG-1); every object is dropped again by
-- 000001_schema.down.sql. There is NO outbox schema (LLD §4.4, AL-INV-10).

CREATE EXTENSION IF NOT EXISTS pgcrypto;   -- gen_random_uuid() fallback; UUIDv7 minted in-app

-- ─────────────────────────────────────────────────────────────────────────
-- §4.1 Enums. entry_type is deliberately NOT an enum (AL-D3): it is a
-- controlled text vocabulary validated in the service layer.
-- ─────────────────────────────────────────────────────────────────────────

-- Storage-retention tier, assigned once at write time (AL-INV-6).
CREATE TYPE audit_retention_tier AS ENUM (
    'compliance_7y',   -- tender approval/signature + security-critical; 7 y; overrides GDPR erasure (AL-INV-7)
    'security_3y',     -- general security records + configuration changes; 3 y
    'access_90d'       -- access logs; 90 d
);

-- Who performed the action (§10.3).
CREATE TYPE audit_actor_type AS ENUM (
    'user',            -- human end user (Keycloak sub as actor_id)
    'service_account', -- Token Service platform-automation principal
    'iam_system',      -- reserved iam-system sentinel 00000000-0000-0000-0000-0000000000a1
    'anonymous'        -- pre-auth origin; actor_id always NULL (AL-D11)
);

-- How the entry reached the sink; never changes the record shape (AL-INV-2).
CREATE TYPE audit_ingest_mode AS ENUM ('bus', 'direct_write');

-- Archival lifecycle of a (partition, tier) pair (§15.4).
CREATE TYPE audit_archive_status AS ENUM (
    'pending', 'archiving', 'archived', 'verified', 'dropped', 'expired', 'failed'
);

-- Async export job lifecycle (§5.4).
CREATE TYPE audit_export_status AS ENUM ('pending', 'running', 'ready', 'failed', 'expired');

-- GDPR redaction task lifecycle (§4.2, §8.7, AL-INV-12).
CREATE TYPE audit_redaction_status AS ENUM ('pending', 'applied', 'not_applicable', 'missed');

-- ─────────────────────────────────────────────────────────────────────────
-- audit_events — the hub table, RANGE-partitioned monthly on occurred_at
-- (AL-D4). Append-only (AL-INV-1). No foreign keys: actor/target ids are
-- opaque soft refs that must outlive the entities they describe (§4).
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE audit_events (
    id                uuid        NOT NULL,                          -- UUIDv7 minted at ingestion
    occurred_at       timestamptz NOT NULL,                          -- event/business time; RANGE partition key
    tenant_id         uuid        NOT NULL,                          -- RLS scope (AL-INV-3)
    entry_type        text        NOT NULL,                          -- normalised controlled vocabulary (§7.1)
    action            text        NOT NULL,                          -- coarse verb
    actor_type        audit_actor_type NOT NULL,
    actor_id          uuid,                                          -- NULL iff actor_type = 'anonymous'
    actor_display     text,                                          -- label only; never authz-consulted
    target_type       text,
    target_id         text,                                          -- NULL for tenant-wide actions
    source_service    text        NOT NULL,
    source_topic      text,                                          -- NULL for direct_write
    source_event_type text        NOT NULL,                          -- producer's verbatim type (AL-INV-5)
    source_event_id   text        NOT NULL,                          -- envelope id / idempotency key (AL-INV-4/5)
    recorded_at       timestamptz NOT NULL DEFAULT now(),
    retention_tier    audit_retention_tier NOT NULL,
    ingest_mode       audit_ingest_mode NOT NULL,
    ip_address        inet,
    user_agent        text,
    trace_id          text,
    metadata          jsonb       NOT NULL DEFAULT '{}',             -- ≤ 8 KiB, service-enforced
    CONSTRAINT audit_events_pkey PRIMARY KEY (id, occurred_at),
    CONSTRAINT chk_anonymous_actor CHECK (
        (actor_type = 'anonymous' AND actor_id IS NULL)
        OR (actor_type <> 'anonymous' AND actor_id IS NOT NULL)
    ),
    CONSTRAINT chk_metadata_object CHECK (jsonb_typeof(metadata) = 'object')
) PARTITION BY RANGE (occurred_at);

-- Catch-all for any occurred_at outside every explicit partition; must be
-- empty in steady state (iam_audit_log_default_partition_rows_total, RB-3).
CREATE TABLE audit_events_default PARTITION OF audit_events DEFAULT;

-- Hard dedup backstop to processed_events (AL-INV-4); survives a ledger prune.
CREATE UNIQUE INDEX uq_audit_events_source_id ON audit_events (source_event_id, occurred_at);

-- Query-path indexes; tenant_id leads each (every query is tenant-scoped).
CREATE INDEX idx_audit_events_tenant_time   ON audit_events (tenant_id, occurred_at DESC);
CREATE INDEX idx_audit_events_tenant_type   ON audit_events (tenant_id, entry_type, occurred_at DESC);
CREATE INDEX idx_audit_events_tenant_actor  ON audit_events (tenant_id, actor_id, occurred_at DESC);
CREATE INDEX idx_audit_events_tenant_target ON audit_events (tenant_id, target_type, target_id, occurred_at DESC);
CREATE INDEX idx_audit_events_tenant_tier   ON audit_events (tenant_id, retention_tier, occurred_at DESC);

-- ─────────────────────────────────────────────────────────────────────────
-- audit_event_archive_state — per (partition, tier) archival bookkeeping.
-- Cross-tenant, RLS-exempt, reconciler-only (§4.2, AL-INV-9).
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE audit_event_archive_state (
    partition_name  text NOT NULL,
    retention_tier  audit_retention_tier NOT NULL,
    period_month    date NOT NULL,
    row_count       bigint NOT NULL DEFAULT 0,
    s3_bucket       text,
    s3_prefix       text,
    object_count    int NOT NULL DEFAULT 0,
    sha256_manifest text,
    status          audit_archive_status NOT NULL DEFAULT 'pending',
    archived_at     timestamptz,
    verified_at     timestamptz,
    dropped_at      timestamptz,
    error           text,
    CONSTRAINT audit_event_archive_state_pkey PRIMARY KEY (partition_name, retention_tier)
);
CREATE INDEX idx_archive_state_status ON audit_event_archive_state (status, period_month);

-- ─────────────────────────────────────────────────────────────────────────
-- processed_events — (event_id, consumer) idempotency ledger (HLD §9.3,
-- AL-INV-4). TEXT event_id: also dedupes direct-write idempotency keys.
-- Global, RLS-exempt; 8-day retention (PROCESSED_EVENTS_TTL_DAYS).
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE processed_events (
    event_id     text NOT NULL,
    consumer     text NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, consumer)
);
CREATE INDEX idx_processed_events_prune ON processed_events (processed_at);

-- ─────────────────────────────────────────────────────────────────────────
-- tenant_plan_window — plan-gated query-window projection (§4.2, AL-D6,
-- AL-D15). query_window_days is the last-known-good fallback; the live
-- value is resolved from plan_code against the CAT-I2 map at query time.
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE tenant_plan_window (
    tenant_id         uuid PRIMARY KEY,
    plan_code         text NOT NULL,
    query_window_days int  NOT NULL,
    last_event_at     timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- ─────────────────────────────────────────────────────────────────────────
-- audit_export_jobs — async export (§5.4 AL-3/AL-4). Tenant-scoped + RLS.
-- updated_at is added so §4.5's touch_row() trigger has a column to touch
-- (the §4.2 DDL omits it — BUILD_PLAN gap 9).
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE audit_export_jobs (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             uuid NOT NULL,
    requested_by          uuid NOT NULL,
    filter                jsonb NOT NULL,
    status                audit_export_status NOT NULL DEFAULT 'pending',
    s3_key                text,
    signed_url_expires_at timestamptz,
    row_count             bigint,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    completed_at          timestamptz,
    error                 text
);
CREATE INDEX idx_export_jobs_tenant ON audit_export_jobs (tenant_id, created_at DESC);
CREATE INDEX idx_export_jobs_status ON audit_export_jobs (status) WHERE status IN ('pending', 'running');

-- ─────────────────────────────────────────────────────────────────────────
-- audit_redaction_tasks — GDPR erasure ledger (§4.2, §8.7, AL-INV-12).
-- One row per erasure trigger; a redelivered trigger is a no-op.
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE audit_redaction_tasks (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               uuid NOT NULL,
    subject_actor_id        uuid NOT NULL,
    trigger_event_type      text NOT NULL,
    trigger_source_event_id text NOT NULL,
    requested_at            timestamptz NOT NULL DEFAULT now(),
    status                  audit_redaction_status NOT NULL DEFAULT 'pending',
    applied_at              timestamptz,
    rows_redacted           bigint,
    error                   text,
    CONSTRAINT uq_redaction_trigger UNIQUE (trigger_source_event_id)
);
CREATE INDEX idx_redaction_tasks_pending ON audit_redaction_tasks (tenant_id, subject_actor_id) WHERE status = 'pending';
