package domain

// Commercial envelope source names (CD-17, Commercial HLD §9.3).
//
// The envelope `source` field is the producing service's self-declared name.
// These constants pin the three agreed values for the Commercial domain so
// every test, config generator and future validation step has a single
// import to reach them, instead of scattered string literals.
//
// The service stores whatever source arrives in SourceService (bus.go:
// nonEmpty(ev.Source, "unknown")) without an allow-list — Commercial services
// may not be deployed yet.  Audit Log does NOT branch on source for actor
// derivation (§ D-8), entry-type classification, or routing; the source is
// purely provenance metadata.
//
// Old names that must NOT appear in new code:
//
//	iam-billing, billing, usage-metering, entitlements,
//	iam-entitlements, iam-usage-metering
const (
	// CommercialSourceBilling is the agreed envelope source for the
	// Billing service (commercial-billing).
	CommercialSourceBilling = "commercial-billing"

	// CommercialSourceEntitlements is the agreed envelope source for
	// the Entitlements service (commercial-entitlements).
	CommercialSourceEntitlements = "commercial-entitlements"

	// CommercialSourceUsageMetering is the agreed envelope source for
	// the Usage & Metering service (commercial-usage-metering).
	CommercialSourceUsageMetering = "commercial-usage-metering"
)
