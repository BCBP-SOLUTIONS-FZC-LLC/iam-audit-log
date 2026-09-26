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
}

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
	"tenant.created":            {tier: TierSecurity3y},
	"tenant.trial_started":      {tier: TierSecurity3y},
	"tenant.trial_provisioned":  {tier: TierSecurity3y},
	"tenant.realm_ready":        {tier: TierSecurity3y},
	"tenant.converted":          {tier: TierSecurity3y},
	"tenant.direct_paid_signup": {tier: TierSecurity3y},
	"tenant.trial_expired":      {tier: TierSecurity3y},
	"tenant.trial_reactivated":  {tier: TierSecurity3y},
	"tenant.suspended":          {tier: TierSecurity3y},
	"tenant.offboarded":         {tier: TierSecurity3y},
	"tenant.reactivated":        {tier: TierSecurity3y},
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
	// usage.events
	"usage.quota.warning":  {tier: TierAccess90d},
	"usage.quota.exceeded": {tier: TierAccess90d},
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

	{TopicTenant, "TenantCreated"}:          "tenant.created",
	{TopicTenant, "TrialStarted"}:           "tenant.trial_started",
	{TopicTenant, "TrialTenantProvisioned"}: "tenant.trial_provisioned",
	{TopicTenant, "TenantRealmReady"}:       "tenant.realm_ready",
	{TopicTenant, "TenantConverted"}:        "tenant.converted",
	{TopicTenant, "DirectPaidSignup"}:       "tenant.direct_paid_signup",
	{TopicTenant, "TrialExpired"}:           "tenant.trial_expired",
	{TopicTenant, "TrialReactivated"}:       "tenant.trial_reactivated",
	{TopicTenant, "TenantSuspended"}:        "tenant.suspended",
	{TopicTenant, "TenantOffboarded"}:       "tenant.offboarded",
	{TopicTenant, "TenantReactivated"}:      "tenant.reactivated",

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

	{TopicUsage, "TenantQuotaWarning"}:  "usage.quota.warning",
	{TopicUsage, "TenantQuotaExceeded"}: "usage.quota.exceeded",

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
	TopicBilling: "billing", TopicUsage: "usage", TopicWFWorkflow: "workflow", TopicWFTemplate: "workflow",
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
