# iam-audit-log — PostgreSQL role prerequisites (RELEASE_CHECKLIST items 1 & 2)
#
# Apply once, before the first helm install.  Uses the
# cyrilgdn/postgresql provider (or equivalent).  The PG connection is
# the superuser DSN for the `audit` database.
#
# Variable values depend on your environment; set them in your
# platform workspace's tfvars / secrets.

terraform {
  required_providers {
    postgresql = {
      source  = "cyrilgdn/postgresql"
      version = ">= 1.22.0"
    }
  }
}

# ── Item 1: audit_migrator can SET ROLE audit_reconciler ─────────────────────
# migration 000007 runs ALTER FUNCTION … OWNER TO audit_reconciler, which
# PostgreSQL only allows when the executing role is a member of audit_reconciler.
resource "postgresql_grant_role" "migrator_owns_reconciler_functions" {
  role       = "audit_migrator"
  grant_role = "audit_reconciler"
  # with_admin_option = false  (default; migrator should not re-grant)
}

# ── Item 2: runtime role attributes ──────────────────────────────────────────
# The server checks these at startup; a mismatch causes a panic/exit.
# Create the roles here only if they do not yet exist in your cluster;
# otherwise use postgresql_role with `skip_reassign_owned = true`.

resource "postgresql_role" "audit_app" {
  name             = "audit_app"
  login            = true
  bypass_row_level_security = false   # NOBYPASSRLS — enforced at startup
  password         = var.audit_app_password
  skip_reassign_owned = true
}

resource "postgresql_role" "audit_reconciler" {
  name             = "audit_reconciler"
  login            = true
  bypass_row_level_security = true    # BYPASSRLS — reconciler reads across all tenants
  password         = var.audit_reconciler_password
  skip_reassign_owned = true
}

resource "postgresql_role" "audit_migrator" {
  name             = "audit_migrator"
  login            = true
  bypass_row_level_security = true    # BYPASSRLS — owns schema objects
  superuser        = false
  create_database  = false
  create_role      = false
  password         = var.audit_migrator_password
  skip_reassign_owned = true
}

# ── Variables ─────────────────────────────────────────────────────────────────
variable "audit_app_password" {
  description = "Password for the audit_app role (stored in Helm Secret)"
  type        = string
  sensitive   = true
}

variable "audit_reconciler_password" {
  description = "Password for the audit_reconciler role (stored in Helm Secret)"
  type        = string
  sensitive   = true
}

variable "audit_migrator_password" {
  description = "Password for the audit_migrator role (startup migration DSN)"
  type        = string
  sensitive   = true
}
