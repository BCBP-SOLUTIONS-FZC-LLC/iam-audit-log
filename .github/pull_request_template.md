## Description
Provide a clear description of the changes. Cite the LLD section / ID implemented (e.g. §5.4 AL-5, AL-INV-4, AL-D15).

---

## Type of Change
- [ ] Bug fix
- [ ] New feature
- [ ] Refactor
- [ ] Documentation
- [ ] Test
- [ ] Breaking change
- [ ] New migration (`audit` DB — `production-data-migrations` gate)
- [ ] Taxonomy / retention-tier change (`internal/core/domain/taxonomy.go`)
- [ ] Inbound event contract change (`api/asyncapi.yaml` or `internal/eventschema/`)

---

## Testing
- [ ] Unit tests added/updated (`make test-unit`)
- [ ] Postgres / RLS integration tests added/updated (`make test-postgres`) — canonical fail-closed Cases 1–4 still pass
- [ ] Integration tests added/updated (`make test-integration`)
- [ ] End-to-end tests added/updated (`make test-e2e`)
- [ ] All tests passing with race detector (`make race`)
- [ ] Every invariant touched has a test naming it (e.g. `..._ALINV4`)
- [ ] Manual testing performed (if required)

---

## Checklist

### Code Quality
- [ ] Code is properly formatted (`make fmt-check`)
- [ ] Linting passed (`make lint`)
- [ ] Vet passed (`make vet`)
- [ ] Architecture lint passed (`make arch-lint`)
- [ ] No debug logs / commented-out code
- [ ] No secrets or DSNs hardcoded

### Invariants (AL-INV-*)
- [ ] AL-INV-1: no `UPDATE`/`DELETE` path for `audit_app` (`check-grants.sh` passes)
- [ ] AL-INV-2: bus and direct-write converge on `IngestService` — no per-transport ingest fork
- [ ] AL-INV-3: every query runs under `pgcommon.WithGUCSet` (no session-level `SET app.tenant_id`)
- [ ] AL-INV-6 / AL-INV-11: `retention_tier` derived server-side from `entry_type` only; tiers only ever lengthen
- [ ] AL-INV-10: no publisher / outbox wiring (`check-forbidden-events-bypass.sh`, `check-outbox-access.sh`, `check-asyncapi-receive-only.sh` pass)
- [ ] Rule 5: `cmd/server` never touches the reconciler DSN; `cmd/reconciler` never touches the app DSN

### API Contract
- [ ] Swagger docs regenerated if handler annotations changed (`make swag` — all three files in `docs/swagger/` committed)
- [ ] Direct-write (AL-5/AL-6) request/response shape unchanged — or producers (Catalog, Realm Provisioner, O&M, Group Mapping, Tender ACL) notified
- [ ] §17 error codes unchanged, or `VERSIONING.md` bump rules followed

### Consumed Event Contract
*Complete only when `api/asyncapi.yaml` or `internal/eventschema/` changed.*

- [ ] Only `receive` operations added — zero `send` (AL-EVT-1)
- [ ] New producer event type has a taxonomy row (§7.1) — otherwise it lands as `<domain>.unknown` (AL-EVT-4), which alarms
- [ ] Consumer parsing stays lenient to unknown fields (producers own their schemas; do NOT call `json.Decoder.DisallowUnknownFields()`)
- [ ] `processed_events.consumer` discriminator values unchanged (frozen, §7.5 / §25)

### Database / Migrations
- [ ] New migrations have matching `.up.sql` and `.down.sql`
- [ ] Down migration correctly reverses the up migration
- [ ] RLS policies tested with `FORCE ROW LEVEL SECURITY` (`make test-postgres`)
- [ ] Migration tested against PgBouncer simple-protocol mode (`PG_BOUNCER_MODE=true`)
- [ ] Partition lifecycle is runtime (`PartitionService`), not a migration (§4.4)

### Security
- [ ] No secrets or DSNs hardcoded
- [ ] No audit payload / `metadata` / GUC values logged at info level (§11)
- [ ] New config fields documented in `.env-example`, `deploy/helm/values.yaml`, and `internal/config` validation

### Documentation
- [ ] README / ARCHITECTURE.md updated (if API, env vars, layering, or invariants changed)
- [ ] `docs/implementation/BUILD_PLAN.md` trace updated
- [ ] `CHANGELOG.md` updated

---

## Related Issue
Closes #<issue-id>

---

## Deployment Notes
Mention anything important for operators upgrading (migration steps, new required env vars, config changes, CronJobs enabled, producer coordination).
