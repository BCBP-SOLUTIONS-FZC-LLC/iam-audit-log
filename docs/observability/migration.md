# Renamed and removed metric names

iam-audit-log moved to the Enterprise Platform Observability Standard in three steps. The service has never been deployed to any environment, so no dashboard, alert or stored series depended on the old names.

- **Step 1 (gap 46)** emitted the three renamed Tier 3 gauges in parallel with their replacements for a compatibility period.
- **Step 2** removed those twins, and the libraries' own pre-standard names, outright. Every rule, SLO, dashboard and HPA reference in this repo was rewritten in the same change.

`metricslint refs` (in CI, `make metrics-lint`) rejects any removed library name that comes back, and `TestAlerts_ReferenceKnownMetrics` rejects any name the service does not emit.

## 1. Three-tier alignment (gap 46, LLD rev 0.26, 2026-09-28)

The Phase 8 metrics were reorganised into the three tiers, the Canonical `platform_*` set was adopted, and the identity labels were injected centrally.

| Removed name | Replacement | Notes |
|---|---|---|
| `iam_audit_log_dlq_messages_total{queue="<q>-dlq"}` | `platform_dlq_depth{queue="<q>"}` | A gauge must not end in `_total`, and DLQ depth is a platform concept. `queue` is now the **source** queue's name |
| `iam_audit_log_default_partition_rows_total` | `iam_audit_log_default_partition_rows` | gauge; no `_total` |
| `iam_audit_log_archive_stalled` | `iam_audit_log_archive_stalled_partitions` | A gauge names its measured quantity |
| `platform_dependency_request_seconds` (Phase 8 label set) | `platform_dependency_request_seconds{dependency,operation,outcome}` | Relabelled to the registry label set. It now also times S3 |

New in the same step:

- Tier 1: `platform_retry_total`, `platform_duplicate_messages_total`, `platform_event_propagation_seconds`, `platform_queue_depth` and `platform_dlq_depth`.
- Tier 2: `iam_rls_violations_total` was registered earlier but never incremented. It is now fed through `audit_rls_violation_counts()` (migration `000010`).

All three removed names were emitted in parallel, marked `deprecated` in `deploy/monitoring/metric-registry.yaml`, until step 2.

## 2. Platform library migration (gincommon v1.4.0, 2026-10-02)

The platform libraries dropped their pre-standard names: platform-gincommon v1.4.0, platform-events/v2 v2.0.0 and platform-pgcommon/v2 v2.0.0. Every query in this repo was rewritten to the `platform_*` successor, and the three deprecated Tier 3 twins from step 1 were removed with no further compatibility period.

| Removed name | Replacement | Label changes |
|---|---|---|
| `http_requests_total` | `platform_http_requests_total` | + `domain`, `environment`; − `version` |
| `http_request_duration_seconds` | `platform_http_request_duration_seconds` | same; identical buckets |
| `http_requests_per_second` (prometheus-adapter) | `platform_http_requests_per_second` | derived from `platform_http_requests_total` |
| `service:http_requests_error_ratio:5m` (recording rule) | `service:platform_http_requests_error_ratio:5m` | |
| `events_consumed_total` | `platform_messages_*` (received, processed, failed) | see platform-events' `docs/observability/` |
| `pgcommon_*` | `platform_db_*` | `pool` label; see platform-pgcommon's `docs/observability/` |

The identity became mandatory and library-injected at the same time:

| Label | Before | After |
|---|---|---|
| `service` | `audit-log` (set by `business.go`) | `iam-audit-log`: gincommon's `Config.ServiceName`, injected on every collector |
| `domain` | `iam` (set by `business.go`) | `iam`: `gincommon.Config.Domain` / `OBSERVABILITY_DOMAIN` |
| `environment` | `APP_ENV` | `APP_ENV`, restricted to `local`, `dev`, `test`, `staging` and `prod` (`production` is rejected at startup) |
| `version` | on every series | only on `platform_build_info` |

Every alert, recording rule, SLO, dashboard panel and adapter rule changed its selector from `service="audit-log"` to `service="iam-audit-log"`.

## 3. Registry-built Tier 2 and gincommon constructors (gincommon v1.6.0, 2026-10-03)

No series was renamed. Only construction and two label details changed.

| Before | After | Why |
|---|---|---|
| `iam_rls_violations_total` defined by hand (`prometheus.NewCounterVec`), ledger status `canonical` | Built from the registry entry (`gincommon.NewSharedCounterVec`, platform-gincommon v1.5.0), status `proposed`. The help text is the registry's | The registry now defines its help, labels and `violation_type` vocabulary. The two RLS alerts carry `metric_status: proposed` |
| Tier 3 `iam_audit_log_*` built with `prometheus.New*` | Built with `gincommon.NewCounterVec` / `NewCounter` / `NewGauge` / `NewHistogramVec`. Names, labels and buckets are unchanged | gincommon validates the prefix, suffixes and prohibited labels at startup |
| Reconciler series labelled `service="iam-audit-log-reconciler"` | `service="iam-audit-log"` | gincommon's Tier 3 prefix check ties `iam_audit_log_*` to `service="iam-audit-log"`. Nothing scrapes the reconciler (D-21), so no rule changes. Traces keep `service.name=iam-audit-log-reconciler` |
| `iam_audit_log_ingest_lag_seconds`: a `Histogram` | A `HistogramVec` with no labels | Same series; gincommon has no unlabelled histogram constructor |

## Querying after the migration

```promql
# DLQ backlog per inbound queue (was iam_audit_log_dlq_messages_total{queue="<q>-dlq"})
max by (environment, queue) (platform_dlq_depth{domain="iam", service="iam-audit-log"})

# Messages that reached the dead-letter threshold, counted once (by the service's dead-letter handler)
sum by (environment, event_type, reason) (increase(platform_dlq_messages_total{service="iam-audit-log"}[15m]))

# 5xx ratio (was service:http_requests_error_ratio:5m over http_requests_total)
service:platform_http_requests_error_ratio:5m{domain="iam", service="iam-audit-log"}

# Partitions held past archival eligibility (was iam_audit_log_archive_stalled)
max(iam_audit_log_archive_stalled_partitions{service="iam-audit-log"})
```
