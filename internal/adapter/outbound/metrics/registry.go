package metrics

// Status records a shared metric's governance ratification state under the
// Enterprise Platform Observability Standard ("Registry ratification
// requirement"). A Proposed name may be emitted, but must never be the live
// query target of an alert, recording rule, SLO or HPA until it flips to
// Canonical. .github/scripts/metrics-registry-lint.sh enforces this by
// parsing this file directly.
type Status int

const (
	// StatusCanonical is a name the Platform (or IAM Domain) Observability
	// Registry has ratified: safe as a live alert/recording/SLO/HPA target.
	StatusCanonical Status = iota
	// StatusProposed is a name awaiting registry ratification.
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

// PlatformRegistry is the Tier-1 (platform_*) ledger this service emits.
// All ten names and label sets are Canonical in the Platform Observability
// Registry: ratified by the Platform Observability Owner, and the same
// entries iam-event-consumer carries (its LLD §16 EC-Q8, rev 0.9.7). This
// service adds no platform_* name of its own (standard rule 11). Keep in
// lockstep with deploy/monitoring/metric-registry.yaml, which
// TestMetricRegistry_* checks.
//
// One field per line, gofmt-aligned: metrics-registry-lint.sh's awk parser
// reads Name:/Status: on separate lines. Do not collapse an entry.
var PlatformRegistry = []RegistryEntry{
	{
		Name:           "platform_messages_received_total",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"queue", "event_type"},
		Status:         StatusCanonical,
	},
	{
		Name:           "platform_messages_processed_total",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"event_type"},
		Status:         StatusCanonical,
	},
	{
		Name:           "platform_messages_failed_total",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"event_type", "reason"},
		Status:         StatusCanonical,
	},
	{
		Name:           "platform_retry_total",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"event_type", "reason"},
		Status:         StatusCanonical,
	},
	{
		Name:           "platform_dlq_messages_total",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"queue", "reason"},
		Status:         StatusCanonical,
	},
	{
		Name:           "platform_duplicate_messages_total",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"event_type"},
		Status:         StatusCanonical,
	},
	{
		Name:           "platform_dependency_request_seconds",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"dependency", "operation", "outcome"},
		Status:         StatusCanonical,
	},
	{
		Name:           "platform_event_propagation_seconds",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"event_type"},
		Status:         StatusCanonical,
	},
	{
		Name:           "platform_queue_depth",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"queue"},
		Status:         StatusCanonical,
	},
	{
		Name:           "platform_dlq_depth",
		RequiredLabels: []string{"domain", "service", "environment"},
		Labels:         []string{"queue"},
		Status:         StatusCanonical,
	},
}

// DomainRegistry is the Tier-2 (iam_*) ledger this service emits. Each name
// must mean the same thing in every IAM service. iam_rls_violations_total is
// the IAM domain metric iam-user-profile established (its LLD §11.2), with
// the same violation_type vocabulary.
var DomainRegistry = []RegistryEntry{
	{
		Name:           "iam_rls_violations_total",
		RequiredLabels: []string{"service", "environment"},
		Labels:         []string{"violation_type"},
		Status:         StatusCanonical,
	},
}
