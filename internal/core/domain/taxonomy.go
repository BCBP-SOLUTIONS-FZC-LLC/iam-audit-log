package domain

import "regexp"

// RetentionTier is how long a row is stored (LLD §4.1, §15.2). Assigned once
// at write time from the entry_type → tier map and never shortened
// (AL-INV-6). Durations are compiled constants, never configuration (§12).
type RetentionTier string

// RetentionTier values — the audit_retention_tier enum.
const (
	TierCompliance7y RetentionTier = "compliance_7y"
	TierSecurity3y   RetentionTier = "security_3y"
	TierAccess90d    RetentionTier = "access_90d"
)

// Consumed topics (logical names, LLD §7.1 / §25).
const (
	TopicAuth           = "iam.auth.events"
	TopicUser           = "iam.user.events"
	TopicMembership     = "iam.membership.events"
	TopicTenant         = "iam.tenant.events"
	TopicDelegation     = "iam.delegation.events"
	TopicServiceAccount = "iam.serviceaccount.events"
	TopicTender         = "tender.events"
	TopicBilling        = "billing.events"
	TopicUsage          = "usage.events"
	TopicEntitlement    = "entitlement.events" // Commercial domain Entitlements service (CD-21)
	TopicWFWorkflow     = "wf.workflow.events"
	TopicWFTemplate     = "wf.template.events"
)

// entryTypeShape is the CI-asserted vocabulary shape (LLD §4.1).
var entryTypeShape = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// ValidEntryTypeShape reports whether s matches <domain>.<object>[.<action>].
func ValidEntryTypeShape(s string) bool { return entryTypeShape.MatchString(s) }

// taxonomyEntry is one entry_type and how it may arrive.
type taxonomyEntry struct {
	tier RetentionTier
	// directWrite marks the non-bus "direct audit write" rows of §7.1 — the
	// only entry types AL-5/AL-6 accept (decision D-6).
	directWrite bool
	// producers, when non-empty, is the closed set of source_service values
	// AL-5/AL-6 accept this direct-write type from (rev 0.31). Empty means
	// any mesh-admitted caller, the D-6 default. It narrows the mesh
	// allow-list (deploy/helm internalCallers), never widens it.
	producers []string
}

// termsAcceptedProducers may write legal.terms.accepted (LLD §7.1, rev
// 0.31): Realm Provisioner records the tenant owner's acceptance at signup
// (RP-1/RP-2). A future re-acceptance flow (a later terms version accepted
// in-product) is added by appending its service here, e.g.
// "iam-org-membership" — plus its pod/SPIFFE id in the Helm internal-caller
// lists if it is not already there — and a §7.1 note; no schema change.
var termsAcceptedProducers = []string{"iam-realm-provisioner"}

// entryTypes is the frozen §7.1 / §25 vocabulary (AL-D2 tier rule:
// approval/auth-failure/credential/privilege → 7 y; configuration/
// lifecycle/delegation/invitations/profile/workflow-assignment → 3 y;
// successful access/operational → 90 d).
var entryTypes = map[string]taxonomyEntry{
	// iam.auth.events
	"auth.login.success":  {tier: TierAccess90d},
	"auth.login.failure":  {tier: TierCompliance7y},
	"auth.password.reset": {tier: TierCompliance7y},
	"auth.mfa.enrolled":   {tier: TierCompliance7y},
	"auth.mfa.reset":      {tier: TierCompliance7y},
	"auth.email.verified": {tier: TierAccess90d},
	// iam.user.events
	"user.provisioned":          {tier: TierSecurity3y},
	"user.updated":              {tier: TierSecurity3y},
	"user.status.changed":       {tier: TierSecurity3y},
	"user.availability.changed": {tier: TierAccess90d},
	"user.deleted":              {tier: TierSecurity3y},
	// iam.membership.events
	"membership.department.granted":       {tier: TierCompliance7y},
	"membership.department.revoked":       {tier: TierCompliance7y},
	"membership.department.level_changed": {tier: TierCompliance7y},
	"membership.role.granted":             {tier: TierCompliance7y},
	"membership.role.revoked":             {tier: TierCompliance7y},
	"membership.revoked":                  {tier: TierCompliance7y},
	"membership.purged":                   {tier: TierCompliance7y},
	"tender.assignee.overridden":          {tier: TierCompliance7y},
	"tenant.state.changed":                {tier: TierSecurity3y},
	"tenant.seat_overage.started":         {tier: TierSecurity3y},
	"tenant.seat_overage.resolved":        {tier: TierSecurity3y},
	// iam.tenant.events
	"tenant.created":              {tier: TierSecurity3y},
	"tenant.trial_started":        {tier: TierSecurity3y},
	"tenant.identity_provisioned": {tier: TierSecurity3y},
	"tenant.realm_ready":          {tier: TierSecurity3y},
	"tenant.converted":            {tier: TierSecurity3y},
	"tenant.direct_paid_signup":   {tier: TierSecurity3y},
	"tenant.trial_expired":        {tier: TierSecurity3y},
	"tenant.trial_reactivated":    {tier: TierSecurity3y},
	"tenant.suspended":            {tier: TierSecurity3y},
	"tenant.offboarded":           {tier: TierSecurity3y},
	"tenant.reactivated":          {tier: TierSecurity3y},
	// iam.delegation.events
	"delegation.started":              {tier: TierSecurity3y},
	"delegation.ended":                {tier: TierSecurity3y},
	"delegation.review_requested":     {tier: TierSecurity3y},
	"delegation.escalation_requested": {tier: TierSecurity3y},
	// iam.serviceaccount.events
	"serviceaccount.registered":         {tier: TierCompliance7y},
	"serviceaccount.credential.issued":  {tier: TierCompliance7y},
	"serviceaccount.credential.rotated": {tier: TierCompliance7y},
	"serviceaccount.credential.revoked": {tier: TierCompliance7y},
	"serviceaccount.revoked":            {tier: TierCompliance7y},
	// tender.events
	"tender.section.approved": {tier: TierCompliance7y},
	"tender.access.granted":   {tier: TierCompliance7y},
	"tender.restricted":       {tier: TierCompliance7y},
	// billing.events (tenant.reactivated is shared with iam.tenant.events)
	"tenant.plan_changed":           {tier: TierSecurity3y},
	"tenant.payment_past_due":       {tier: TierSecurity3y},
	"tenant.subscription_cancelled": {tier: TierSecurity3y},
	"tenant.seats_changed":          {tier: TierSecurity3y}, // CD-21: Billing TenantSeatsChanged (Commercial HLD §9.1)
	"billing.token_addon.purchased": {tier: TierSecurity3y}, // IAM-7: Billing TenantQuotaAddOnChanged (CD-55; Commercial HLD §8.6.2/§9.4)
	// usage.events
	"usage.quota.warning":  {tier: TierAccess90d},
	"usage.quota.exceeded": {tier: TierAccess90d},
	"usage.quota.restored": {tier: TierAccess90d}, // CD-21: new v0.7 event; headroom restored after exhaustion
	// entitlement.events (CD-21: Commercial domain Entitlements service)
	"entitlement.tenant_changed": {tier: TierSecurity3y},
	// wf.workflow.events / wf.template.events
	"workflow.task.created":       {tier: TierAccess90d},
	"workflow.task.sla_warning":   {tier: TierAccess90d},
	"workflow.task.sla_breached":  {tier: TierAccess90d},
	"workflow.task.deferred":      {tier: TierAccess90d},
	"workflow.task.reassigned":    {tier: TierSecurity3y},
	"workflow.finished":           {tier: TierAccess90d},
	"workflow.template.published": {tier: TierSecurity3y},
	// direct-write (AL-D1; the only types AL-5/AL-6 accept — D-6)
	"config.tenant_setting.changed":                 {tier: TierSecurity3y, directWrite: true},
	"config.idp.changed":                            {tier: TierSecurity3y, directWrite: true},
	"tenant.owner_signed_up":                        {tier: TierSecurity3y, directWrite: true},
	"security.cross_tenant_access":                  {tier: TierCompliance7y, directWrite: true},
	"config.department.created":                     {tier: TierSecurity3y, directWrite: true},
	"config.department.updated":                     {tier: TierSecurity3y, directWrite: true},
	"config.plan.updated":                           {tier: TierSecurity3y, directWrite: true},
	"config.group_mapping.department_roles.changed": {tier: TierSecurity3y, directWrite: true},
	"config.group_mapping.departments.changed":      {tier: TierSecurity3y, directWrite: true},
	"config.group_mapping.tenant_roles.changed":     {tier: TierSecurity3y, directWrite: true},
	"config.tender_acl.granted":                     {tier: TierSecurity3y, directWrite: true},
	"config.tender_acl.revoked":                     {tier: TierSecurity3y, directWrite: true},
	"config.tender_acl.cascade":                     {tier: TierSecurity3y, directWrite: true},
	"invitation.created":                            {tier: TierSecurity3y, directWrite: true},
	"invitation.revoked":                            {tier: TierSecurity3y, directWrite: true},
	"invitation.expired":                            {tier: TierSecurity3y, directWrite: true},
	// Legal: a user's acceptance of a terms-of-service version (rev 0.31).
	// compliance_7y — kept 7 y and never redacted on erasure (AL-INV-7) —
	// and producer-restricted (termsAcceptedProducers).
	"legal.terms.accepted": {tier: TierCompliance7y, directWrite: true, producers: termsAcceptedProducers},
	// Commercial domain direct-write entries (CD-21; Entitlements and Billing operator actions)
	"config.entitlement_override.set":     {tier: TierSecurity3y, directWrite: true},
	"config.entitlement_override.removed": {tier: TierSecurity3y, directWrite: true},
	"config.entitlement_override.expired": {tier: TierSecurity3y, directWrite: true},
	"billing.adjustment.issued":           {tier: TierSecurity3y, directWrite: true},
	"billing.refund.issued":               {tier: TierSecurity3y, directWrite: true},
	// Token add-on lifecycle (IAM-8/IAM-11; CD-55): purchased arrives on the bus; granted/adjusted/revoked/expired via AL-5 from Entitlements.
	"billing.token_addon.granted":  {tier: TierSecurity3y, directWrite: true},
	"billing.token_addon.adjusted": {tier: TierSecurity3y, directWrite: true},
	"billing.token_addon.revoked":  {tier: TierSecurity3y, directWrite: true},
	"billing.token_addon.expired":  {tier: TierSecurity3y, directWrite: true}, // IAM-11: pack reaches 12 months after purchase with tokens unused
	// Cross-tenant bulk jobs (period rollover, limit sync, reconciliation) audit one entry
	// per run, not per tenant, using the platform_tenant sentinel UUID (CD-21; HLD §7.2).
	"commercial.cross_tenant_job.ran": {tier: TierSecurity3y, directWrite: true},
}

type busKey struct{ topic, sourceType string }

// busTypes maps (topic, producer's verbatim type) → entry_type (§7.1). Two
// producer types are dual-source by design: MFAReset (Event Consumer and
// O&M's admin reset) and TenantReactivated (operator vs billing).
var busTypes = map[busKey]string{
	{TopicAuth, "LoginSuccess"}:  "auth.login.success",
	{TopicAuth, "LoginFailure"}:  "auth.login.failure",
	{TopicAuth, "PasswordReset"}: "auth.password.reset",
	{TopicAuth, "MFAEnrolled"}:   "auth.mfa.enrolled",
	{TopicAuth, "MFAReset"}:      "auth.mfa.reset",
	{TopicAuth, "EmailVerified"}: "auth.email.verified",

	{TopicUser, "UserProvisioned"}:         "user.provisioned",
	{TopicUser, "UserUpdated"}:             "user.updated",
	{TopicUser, "UserStatusChanged"}:       "user.status.changed",
	{TopicUser, "UserAvailabilityChanged"}: "user.availability.changed",
	{TopicUser, "UserDeleted"}:             "user.deleted",

	{TopicMembership, "DepartmentMembershipGranted"}:      "membership.department.granted",
	{TopicMembership, "DepartmentMembershipRevoked"}:      "membership.department.revoked",
	{TopicMembership, "DepartmentMembershipLevelChanged"}: "membership.department.level_changed",
	{TopicMembership, "TenantRoleGranted"}:                "membership.role.granted",
	{TopicMembership, "TenantRoleRevoked"}:                "membership.role.revoked",
	{TopicMembership, "MembershipRevoked"}:                "membership.revoked",
	{TopicMembership, "TenantMembershipsPurged"}:          "membership.purged",
	{TopicMembership, "TenderAssigneeOverridden"}:         "tender.assignee.overridden",
	{TopicMembership, "MFAReset"}:                         "auth.mfa.reset",
	{TopicMembership, "TenantStateChanged"}:               "tenant.state.changed",
	{TopicMembership, "TenantSeatOverageStarted"}:         "tenant.seat_overage.started",
	{TopicMembership, "TenantSeatOverageResolved"}:        "tenant.seat_overage.resolved",

	{TopicTenant, "TenantCreated"}:             "tenant.created",
	{TopicTenant, "TrialStarted"}:              "tenant.trial_started",
	{TopicTenant, "TenantIdentityProvisioned"}: "tenant.identity_provisioned",
	{TopicTenant, "TenantRealmReady"}:          "tenant.realm_ready",
	{TopicTenant, "TenantConverted"}:           "tenant.converted",
	{TopicTenant, "DirectPaidSignup"}:          "tenant.direct_paid_signup",
	{TopicTenant, "TrialExpired"}:              "tenant.trial_expired",
	{TopicTenant, "TrialReactivated"}:          "tenant.trial_reactivated",
	{TopicTenant, "TenantSuspended"}:           "tenant.suspended",
	{TopicTenant, "TenantOffboarded"}:          "tenant.offboarded",
	{TopicTenant, "TenantReactivated"}:         "tenant.reactivated",

	{TopicDelegation, "DelegationStarted"}:             "delegation.started",
	{TopicDelegation, "DelegationEnded"}:               "delegation.ended",
	{TopicDelegation, "DelegationReviewRequested"}:     "delegation.review_requested",
	{TopicDelegation, "DelegationEscalationRequested"}: "delegation.escalation_requested",

	{TopicServiceAccount, "ServiceAccountRegistered"}:        "serviceaccount.registered",
	{TopicServiceAccount, "ServiceAccountCredentialIssued"}:  "serviceaccount.credential.issued",
	{TopicServiceAccount, "ServiceAccountCredentialRotated"}: "serviceaccount.credential.rotated",
	{TopicServiceAccount, "ServiceAccountCredentialRevoked"}: "serviceaccount.credential.revoked",
	{TopicServiceAccount, "ServiceAccountRevoked"}:           "serviceaccount.revoked",

	{TopicTender, "TenderSectionApproved"}: "tender.section.approved",
	{TopicTender, "TenderAccessGranted"}:   "tender.access.granted",
	{TopicTender, "TenderRestricted"}:      "tender.restricted",

	{TopicBilling, "TenantPlanChanged"}:           "tenant.plan_changed",
	{TopicBilling, "TenantPaymentPastDue"}:        "tenant.payment_past_due",
	{TopicBilling, "TenantSubscriptionCancelled"}: "tenant.subscription_cancelled",
	{TopicBilling, "TenantReactivated"}:           "tenant.reactivated",
	{TopicBilling, "TenantSeatsChanged"}:          "tenant.seats_changed",          // CD-21: Commercial HLD §9.1
	{TopicBilling, "TenantQuotaAddOnChanged"}:     "billing.token_addon.purchased", // IAM-7: token add-on purchase (CD-55; Commercial HLD §9.4)

	{TopicUsage, "TenantQuotaWarning"}:  "usage.quota.warning",
	{TopicUsage, "TenantQuotaExceeded"}: "usage.quota.exceeded",
	{TopicUsage, "TenantQuotaRestored"}: "usage.quota.restored", // CD-21: new v0.7 event

	{TopicEntitlement, "TenantEntitlementsChanged"}: "entitlement.tenant_changed", // CD-21

	{TopicWFWorkflow, "TaskCreated"}:      "workflow.task.created",
	{TopicWFWorkflow, "TaskSLAWarning"}:   "workflow.task.sla_warning",
	{TopicWFWorkflow, "TaskSLABreached"}:  "workflow.task.sla_breached",
	{TopicWFWorkflow, "TaskDeferred"}:     "workflow.task.deferred",
	{TopicWFWorkflow, "TaskReassigned"}:   "workflow.task.reassigned",
	{TopicWFWorkflow, "WorkflowFinished"}: "workflow.finished",

	{TopicWFTemplate, "TemplatePublished"}: "workflow.template.published",

	// Decision D-7: Workflow Engine's actual wire types (dotted lowercase,
	// Workflow execution/definition service docs) map to the same entries.
	{TopicWFWorkflow, "workflow.task.created"}:       "workflow.task.created",
	{TopicWFWorkflow, "workflow.task.sla-warning"}:   "workflow.task.sla_warning",
	{TopicWFWorkflow, "workflow.task.sla-breached"}:  "workflow.task.sla_breached",
	{TopicWFWorkflow, "workflow.task.deferred"}:      "workflow.task.deferred",
	{TopicWFWorkflow, "workflow.task.reassigned"}:    "workflow.task.reassigned",
	{TopicWFWorkflow, "workflow.instance.finished"}:  "workflow.finished",
	{TopicWFTemplate, "workflow.template.published"}: "workflow.template.published",
}

// busAliasCount is the number of D-7 alias rows in busTypes (beyond §7.1).
const busAliasCount = 7

// topicDomains names the <domain> of the fail-safe "<domain>.unknown" entry
// for each consumed topic (AL-EVT-4, AL-D9).
var topicDomains = map[string]string{
	TopicAuth: "auth", TopicUser: "user", TopicMembership: "membership", TopicTenant: "tenant",
	TopicDelegation: "delegation", TopicServiceAccount: "serviceaccount", TopicTender: "tender",
	TopicBilling: "billing", TopicUsage: "usage", TopicEntitlement: "entitlement",
	TopicWFWorkflow: "workflow", TopicWFTemplate: "workflow",
}

// TierFor returns the retention tier of a known entry_type.
func TierFor(entryType string) (RetentionTier, bool) {
	e, ok := entryTypes[entryType]
	return e.tier, ok
}

// IsDirectWriteEntryType reports whether entryType may be written through
// AL-5/AL-6 (decision D-6).
func IsDirectWriteEntryType(entryType string) bool {
	return entryTypes[entryType].directWrite
}

// DirectWriteProducers returns the source_service values entryType is
// restricted to, or nil when any mesh-admitted caller may write it.
func DirectWriteProducers(entryType string) []string {
	p := entryTypes[entryType].producers
	if len(p) == 0 {
		return nil
	}
	return append([]string(nil), p...)
}

// DirectWriteProducerAllowed reports whether sourceService may write
// entryType through AL-5/AL-6: always, unless the type names producers.
func DirectWriteProducerAllowed(entryType, sourceService string) bool {
	p := entryTypes[entryType].producers
	if len(p) == 0 {
		return true
	}
	for _, s := range p {
		if s == sourceService {
			return true
		}
	}
	return false
}

// ClassifyBus maps a consumed event to its entry_type and tier. An
// unrecognized type — or a topic outside §7.1 — is never dropped: it is
// classified "<domain>.unknown" at security_3y and known=false, so the
// caller persists it and alarms (AL-EVT-4, AL-D9).
func ClassifyBus(topic, sourceEventType string) (entryType string, tier RetentionTier, known bool) {
	if et, ok := busTypes[busKey{topic, sourceEventType}]; ok {
		return et, entryTypes[et].tier, true
	}
	d, ok := topicDomains[topic]
	if !ok {
		d = "unmapped"
	}
	return d + ".unknown", TierSecurity3y, false
}

// EntryTypes returns every known entry_type (a copy; for tests and docs).
func EntryTypes() map[string]RetentionTier {
	out := make(map[string]RetentionTier, len(entryTypes))
	for k, v := range entryTypes {
		out[k] = v.tier
	}
	return out
}

// BusMapping is one (topic, source_event_type) → entry_type row.
type BusMapping struct{ Topic, SourceEventType, EntryType string }

// BusMappings returns every §7.1 bus row (a copy; for tests and docs).
func BusMappings() []BusMapping {
	out := make([]BusMapping, 0, len(busTypes))
	for k, v := range busTypes {
		out = append(out, BusMapping{Topic: k.topic, SourceEventType: k.sourceType, EntryType: v})
	}
	return out
}

// EventTypeLabel bounds a bus event type for use as a metric label value:
// a type known to the §7.1 taxonomy for its topic is kept, and anything
// else becomes "unknown". A producer can therefore never create unbounded
// label cardinality (Observability Standard: no unbounded labels).
func EventTypeLabel(topic, eventType string) string {
	if _, _, known := ClassifyBus(topic, eventType); known {
		return eventType
	}
	return "unknown"
}
