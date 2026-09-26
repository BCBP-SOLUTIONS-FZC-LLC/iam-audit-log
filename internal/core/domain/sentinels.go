package domain

// Reserved sentinels (LLD §10.3, §25) — platform-wide frozen values every
// IAM service recognizes on sight. Kept as canonical strings so this package
// stays standard-library-only (LLD §3.2); adapters parse them.
const (
	// IAMSystemActorID is the reserved iam-system principal — crons,
	// cascade consumers, internal provisioning, and every mesh-internal
	// caller.
	IAMSystemActorID = "00000000-0000-0000-0000-0000000000a1"

	// PlatformTenantID is the reserved platform_tenant tenant_id for
	// direct-write entries describing a platform-wide change (AL-D14). It is
	// an ordinary UUID to RLS; no real tenant is ever assigned it.
	PlatformTenantID = "00000000-0000-0000-0000-0000000000b1"
)
