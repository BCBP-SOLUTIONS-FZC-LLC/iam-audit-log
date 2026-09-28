package domain

import "testing"

// lldTable is LLD §7.1's taxonomy table, transcribed row by row. Every
// consumed (topic, source_event_type) must map to exactly this entry_type
// and tier (AL-D2, AL-D3; closes EC-Q3).
var lldTable = []struct {
	topic, sourceType, entryType string
	tier                         RetentionTier
}{
	{TopicAuth, "LoginSuccess", "auth.login.success", TierAccess90d},
	{TopicAuth, "LoginFailure", "auth.login.failure", TierCompliance7y},
	{TopicAuth, "PasswordReset", "auth.password.reset", TierCompliance7y},
	{TopicAuth, "MFAEnrolled", "auth.mfa.enrolled", TierCompliance7y},
	{TopicAuth, "MFAReset", "auth.mfa.reset", TierCompliance7y},
	{TopicAuth, "EmailVerified", "auth.email.verified", TierAccess90d},
	{TopicUser, "UserProvisioned", "user.provisioned", TierSecurity3y},
	{TopicUser, "UserUpdated", "user.updated", TierSecurity3y},
	{TopicUser, "UserAvailabilityChanged", "user.availability.changed", TierAccess90d},
	{TopicUser, "UserDeleted", "user.deleted", TierSecurity3y},
	{TopicMembership, "DepartmentMembershipGranted", "membership.department.granted", TierCompliance7y},
	{TopicMembership, "DepartmentMembershipRevoked", "membership.department.revoked", TierCompliance7y},
	{TopicMembership, "DepartmentMembershipLevelChanged", "membership.department.level_changed", TierCompliance7y},
	{TopicMembership, "TenantRoleGranted", "membership.role.granted", TierCompliance7y},
	{TopicMembership, "TenantRoleRevoked", "membership.role.revoked", TierCompliance7y},
	{TopicMembership, "MembershipRevoked", "membership.revoked", TierCompliance7y},
	{TopicMembership, "TenantMembershipsPurged", "membership.purged", TierCompliance7y},
	{TopicMembership, "TenderAssigneeOverridden", "tender.assignee.overridden", TierCompliance7y},
	{TopicMembership, "MFAReset", "auth.mfa.reset", TierCompliance7y},
	{TopicMembership, "TenantStateChanged", "tenant.state.changed", TierSecurity3y},
	{TopicMembership, "TenantSeatOverageStarted", "tenant.seat_overage.started", TierSecurity3y},
	{TopicMembership, "TenantSeatOverageResolved", "tenant.seat_overage.resolved", TierSecurity3y},
	{TopicTenant, "TenantCreated", "tenant.created", TierSecurity3y},
	{TopicTenant, "TrialStarted", "tenant.trial_started", TierSecurity3y},
	{TopicTenant, "TrialTenantProvisioned", "tenant.trial_provisioned", TierSecurity3y},
	{TopicTenant, "TenantRealmReady", "tenant.realm_ready", TierSecurity3y},
	{TopicTenant, "TenantConverted", "tenant.converted", TierSecurity3y},
	{TopicTenant, "DirectPaidSignup", "tenant.direct_paid_signup", TierSecurity3y},
	{TopicTenant, "TrialExpired", "tenant.trial_expired", TierSecurity3y},
	{TopicTenant, "TrialReactivated", "tenant.trial_reactivated", TierSecurity3y},
	{TopicTenant, "TenantSuspended", "tenant.suspended", TierSecurity3y},
	{TopicTenant, "TenantOffboarded", "tenant.offboarded", TierSecurity3y},
	{TopicTenant, "TenantReactivated", "tenant.reactivated", TierSecurity3y},
	{TopicDelegation, "DelegationStarted", "delegation.started", TierSecurity3y},
	{TopicDelegation, "DelegationEnded", "delegation.ended", TierSecurity3y},
	{TopicDelegation, "DelegationReviewRequested", "delegation.review_requested", TierSecurity3y},
	{TopicDelegation, "DelegationEscalationRequested", "delegation.escalation_requested", TierSecurity3y},
	{TopicServiceAccount, "ServiceAccountRegistered", "serviceaccount.registered", TierCompliance7y},
	{TopicServiceAccount, "ServiceAccountCredentialIssued", "serviceaccount.credential.issued", TierCompliance7y},
	{TopicServiceAccount, "ServiceAccountCredentialRotated", "serviceaccount.credential.rotated", TierCompliance7y},
	{TopicServiceAccount, "ServiceAccountCredentialRevoked", "serviceaccount.credential.revoked", TierCompliance7y},
	{TopicServiceAccount, "ServiceAccountRevoked", "serviceaccount.revoked", TierCompliance7y},
	{TopicTender, "TenderSectionApproved", "tender.section.approved", TierCompliance7y},
	{TopicTender, "TenderAccessGranted", "tender.access.granted", TierCompliance7y},
	{TopicTender, "TenderRestricted", "tender.restricted", TierCompliance7y},
	{TopicBilling, "TenantPlanChanged", "tenant.plan_changed", TierSecurity3y},
	{TopicBilling, "TenantPaymentPastDue", "tenant.payment_past_due", TierSecurity3y},
	{TopicBilling, "TenantSubscriptionCancelled", "tenant.subscription_cancelled", TierSecurity3y},
	{TopicBilling, "TenantReactivated", "tenant.reactivated", TierSecurity3y},
	{TopicUsage, "TenantQuotaWarning", "usage.quota.warning", TierAccess90d},
	{TopicUsage, "TenantQuotaExceeded", "usage.quota.exceeded", TierAccess90d},
	{TopicWFWorkflow, "TaskCreated", "workflow.task.created", TierAccess90d},
	{TopicWFWorkflow, "TaskSLAWarning", "workflow.task.sla_warning", TierAccess90d},
	{TopicWFWorkflow, "TaskSLABreached", "workflow.task.sla_breached", TierAccess90d},
	{TopicWFWorkflow, "TaskDeferred", "workflow.task.deferred", TierAccess90d},
	{TopicWFWorkflow, "TaskReassigned", "workflow.task.reassigned", TierSecurity3y},
	{TopicWFWorkflow, "WorkflowFinished", "workflow.finished", TierAccess90d},
	{TopicWFTemplate, "TemplatePublished", "workflow.template.published", TierSecurity3y},
}

// lldDirectWrite is §7.1's direct-write rows (the only AL-5/AL-6 types, D-6).
var lldDirectWrite = map[string]RetentionTier{
	"config.tenant_setting.changed":                 TierSecurity3y,
	"config.idp.changed":                            TierSecurity3y,
	"tenant.owner_signed_up":                        TierSecurity3y,
	"security.cross_tenant_access":                  TierCompliance7y,
	"config.department.created":                     TierSecurity3y,
	"config.department.updated":                     TierSecurity3y,
	"config.plan.updated":                           TierSecurity3y,
	"config.group_mapping.department_roles.changed": TierSecurity3y,
	"config.group_mapping.departments.changed":      TierSecurity3y,
	"config.group_mapping.tenant_roles.changed":     TierSecurity3y,
	"config.tender_acl.granted":                     TierSecurity3y,
	"config.tender_acl.revoked":                     TierSecurity3y,
	"config.tender_acl.cascade":                     TierSecurity3y,
	"invitation.created":                            TierSecurity3y,
	"invitation.revoked":                            TierSecurity3y,
	"invitation.expired":                            TierSecurity3y,
}

func TestTaxonomy_EveryLLDBusRowMaps_ALD3(t *testing.T) {
	for _, row := range lldTable {
		et, tier, known := ClassifyBus(row.topic, row.sourceType)
		if !known || et != row.entryType || tier != row.tier {
			t.Errorf("%s/%s → (%s,%s,%v), want (%s,%s)", row.topic, row.sourceType, et, tier, known, row.entryType, row.tier)
		}
	}
	if got := len(BusMappings()); got != len(lldTable)+busAliasCount {
		t.Errorf("code has %d bus rows, LLD §7.1 has %d (+%d D-7 aliases)", got, len(lldTable), busAliasCount)
	}
}

func TestTaxonomy_DirectWriteRowsExact_D6(t *testing.T) {
	n := 0
	for et, tier := range EntryTypes() {
		if IsDirectWriteEntryType(et) {
			n++
			if lldDirectWrite[et] != tier {
				t.Errorf("%s: tier %s, want %s", et, tier, lldDirectWrite[et])
			}
		}
	}
	if n != len(lldDirectWrite) {
		t.Errorf("%d direct-write types in code, %d in LLD §7.1", n, len(lldDirectWrite))
	}
	for _, row := range lldTable {
		if IsDirectWriteEntryType(row.entryType) {
			t.Errorf("bus type %s must not be accepted via direct-write (D-6)", row.entryType)
		}
	}
}

// §25 frozen vocabulary: every entry_type matches the CI shape, is reached by
// a bus row or is a direct-write row, and has a tier.
func TestTaxonomy_VocabularyShapeAndCoverage(t *testing.T) {
	reached := map[string]bool{}
	for _, m := range BusMappings() {
		reached[m.EntryType] = true
		if _, ok := TierFor(m.EntryType); !ok {
			t.Errorf("bus row points at unknown entry_type %s", m.EntryType)
		}
	}
	for et, tier := range EntryTypes() {
		if !ValidEntryTypeShape(et) {
			t.Errorf("%s fails the vocabulary shape", et)
		}
		if tier != TierCompliance7y && tier != TierSecurity3y && tier != TierAccess90d {
			t.Errorf("%s has invalid tier %q", et, tier)
		}
		if !reached[et] && !IsDirectWriteEntryType(et) {
			t.Errorf("%s is neither reachable from a topic nor direct-write", et)
		}
	}
	if len(EntryTypes()) != 72 {
		t.Errorf("vocabulary size %d; LLD §25 lists 72 (excluding <domain>.unknown)", len(EntryTypes()))
	}
}

// AL-EVT-4 / AL-D9: an unrecognized type is classified <domain>.unknown at
// security_3y — persisted and alarmed, never dropped.
func TestTaxonomy_UnknownTypeFailSafe_ALEVT4(t *testing.T) {
	cases := map[string]string{
		TopicAuth: "auth.unknown", TopicMembership: "membership.unknown", TopicBilling: "billing.unknown",
		TopicWFWorkflow: "workflow.unknown", TopicWFTemplate: "workflow.unknown", "some.new.topic": "unmapped.unknown",
	}
	for topic, want := range cases {
		et, tier, known := ClassifyBus(topic, "BrandNewEvent")
		if known || et != want || tier != TierSecurity3y {
			t.Errorf("%s → (%s,%s,%v), want (%s,security_3y,false)", topic, et, tier, known, want)
		}
		if !ValidEntryTypeShape(et) {
			t.Errorf("%s fails the vocabulary shape", et)
		}
	}
	if _, ok := TierFor("auth.unknown"); ok {
		t.Error("<domain>.unknown must not be a taxonomy value (not direct-writable)")
	}
}

// Decision D-7: Workflow Engine's dotted wire types map to the LLD entries;
// other workflow wire types stay on the fail-safe path.
func TestTaxonomy_WorkflowWireAliases_D7(t *testing.T) {
	for wire, want := range map[string]string{
		"workflow.task.created": "workflow.task.created", "workflow.task.sla-warning": "workflow.task.sla_warning",
		"workflow.task.sla-breached": "workflow.task.sla_breached", "workflow.task.deferred": "workflow.task.deferred",
		"workflow.task.reassigned": "workflow.task.reassigned", "workflow.instance.finished": "workflow.finished",
	} {
		if et, _, ok := ClassifyBus(TopicWFWorkflow, wire); !ok || et != want {
			t.Errorf("%s → %s/%v, want %s", wire, et, ok, want)
		}
	}
	if et, _, ok := ClassifyBus(TopicWFTemplate, "workflow.template.published"); !ok || et != "workflow.template.published" {
		t.Errorf("template alias → %s/%v", et, ok)
	}
	if et, _, ok := ClassifyBus(TopicWFWorkflow, "workflow.task.claimed"); ok || et != "workflow.unknown" {
		t.Errorf("unmapped wire type → %s/%v", et, ok)
	}
}

// EventTypeLabel keeps only types the §7.1 taxonomy knows for the topic, so a
// producer can never create unbounded metric label cardinality.
func TestEventTypeLabel(t *testing.T) {
	for _, tc := range []struct{ topic, typ, want string }{
		{TopicUser, "UserDeleted", "UserDeleted"},
		{TopicUser, "TenantOffboarded", "unknown"}, // real type, wrong topic
		{TopicUser, "NoSuchEvent", "unknown"},
		{TopicUser, "", "unknown"},
		{"no.such.topic", "UserDeleted", "unknown"},
	} {
		if got := EventTypeLabel(tc.topic, tc.typ); got != tc.want {
			t.Errorf("EventTypeLabel(%q, %q) = %q, want %q", tc.topic, tc.typ, got, tc.want)
		}
	}
}
