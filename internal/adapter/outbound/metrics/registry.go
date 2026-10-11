package metrics

// Status records a shared metric's governance ratification state under the
// Enterprise Platform Observability Standard ("Registry ratification
// requirement"), mirroring its status in the central Platform Observability
// Registry (platform-gincommon obsregistry: approved = Canonical, proposed =
// Proposed); TestRegistryLedger_MatchesPlatformRegistry checks the two agree.
// .github/scripts/metrics-registry-lint.sh parses this file directly.
type Status int

const (
	// StatusCanonical is a name the Platform (or IAM Domain) Observability
	// Registry has ratified: safe as a live alert/recording/SLO/HPA target.
	StatusCanonical Status = iota
	// StatusProposed is a name awaiting registry ratification. It may be a
	// live alert/recording-rule/SLO/HPA target only when the rule is marked
	// as such (alert label metric_status: proposed, a "# metric_status:
	// proposed" comment on a recording rule, "(Proposed)" in a dashboard
	// panel title, the observability.platform/metric-status: proposed
	// annotation on an autoscaler) — metrics-registry-lint.sh enforces it.
	StatusProposed
)

// RegistryEntry is one governed shared (platform_* / iam_*) metric: its
// ratified name, the centrally injected labels, the approved label set,
// and its status.
type RegistryEntry struct {
	Name           string
	RequiredLabels []string // centrally injected (platformLabels / serviceLabels)
	Labels         []string // approved dimensions, from the registry vocabulary
	Status         Status
}

// PlatformRegistry is the Tier-1 (platform_*) ledger this service itself
// records (business.go builds each with gincommon.NewShared*Vec, so name,
// help, labels and buckets are the central registry's). platform-events
// registers the same names and reuses these collectors. Keep in lockstep
// with deploy/monitoring/metric-registry.yaml, which TestMetricRegistry_*
// checks.
//
// One field per line, gofmt-aligned: metrics-registry-lint.sh's awk parser
// reads Name:/Status: on separate lines. Do not collapse an entry.
var PlatformRegistry = []RegistryEntry{
	{
		Name:           "platform_dlq_messages_total",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"operation", "event_type", "reason"},
		Status:         StatusCanonical,
	},
	{
		Name:           "platform_duplicate_messages_total",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"queue", "event_type"},
		Status:         StatusProposed,
	},
	{
		Name:           "platform_dependency_request_seconds",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"dependency", "operation", "outcome"},
		Status:         StatusProposed,
	},
	{
		Name:           "platform_queue_depth",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"queue"},
		Status:         StatusProposed,
	},
	{
		Name:           "platform_dlq_depth",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"queue"},
		Status:         StatusProposed,
	},
}

// LibraryRegistry lists the platform_* metrics a platform library emits in
// this process that this repo's alerts, recording rules, SLOs, dashboard or
// HPA read: platform-events' consumption lifecycle and gincommon's HTTP
// metrics. The service does not register them; they are listed so the
// Proposed lint and the artifact tests know their status and labels.
var LibraryRegistry = []RegistryEntry{
	{
		Name:           "platform_messages_received_total",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"queue"},
		Status:         StatusCanonical,
	},
	{
		Name:           "platform_messages_processed_total",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"queue", "event_type"},
		Status:         StatusCanonical,
	},
	{
		Name:           "platform_messages_failed_total",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"queue", "event_type", "reason"},
		Status:         StatusCanonical,
	},
	{
		Name:           "platform_retry_total",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"operation", "event_type"},
		Status:         StatusCanonical,
	},
	{
		Name:           "platform_event_propagation_seconds",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"queue", "event_type"},
		Status:         StatusProposed,
	},
	{
		Name:           "platform_http_requests_total",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"method", "route", "status_class", "error_class"},
		Status:         StatusProposed,
	},
}

// DomainRegistry is the Tier-2 (iam_*) ledger this service emits. Each name
// must mean the same thing in every IAM service. iam_rls_violations_total is
// the IAM domain metric iam-user-profile established (its LLD §11.2); since
// platform-gincommon v1.5.0 it is a central registry entry (status proposed,
// no IAM owner yet), and business.go builds it with NewSharedCounterVec.
var DomainRegistry = []RegistryEntry{
	{
		Name:           "iam_rls_violations_total",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"violation_type"},
		Status:         StatusProposed,
	},
}
