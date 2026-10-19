# iam-audit-log — S3 lifecycle rules (RELEASE_CHECKLIST items 3 & 4)
#
# The bucket `iam-audit-archive` is provisioned by the platform Terraform
# workspace. Apply these rules in the same workspace or as a separate
# targeted apply against the bucket.
#
# RELEASE_CHECKLIST item 3: confirm no *default* Object Lock retention is set.
#   The archiver sets per-object COMPLIANCE retention explicitly on every
#   archive object (PutObjectRetention). A bucket-level default would also
#   lock every export object under exports/, which must expire after 7 days.
#   Verify: aws s3api get-object-lock-configuration --bucket iam-audit-archive
#   Expected: Mode absent (no default) or Mode = COMPLIANCE with Days = 0.
#
# RELEASE_CHECKLIST item 4: exports/ expiration rule (this file).
#   Export objects must expire after EXPORT_SIGNED_URL_TTL (default 7 days).
#   Without this rule, export objects accumulate and consume storage indefinitely.

terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
  }
}

variable "audit_archive_bucket" {
  description = "Name of the iam-audit-archive S3 bucket"
  type        = string
  default     = "iam-audit-archive"
}

variable "export_expiration_days" {
  description = "Days until export objects expire (must be >= EXPORT_SIGNED_URL_TTL, default 7)"
  type        = number
  default     = 7
}

# exports/ prefix: objects expire after export_expiration_days.
# Object Lock is NOT applied to the exports/ prefix; archive objects under
# {compliance_7y,security_3y,access_90d}/ get per-object COMPLIANCE retention
# from the archiver itself (PutObjectRetention).
resource "aws_s3_bucket_lifecycle_configuration" "audit_archive" {
  bucket = var.audit_archive_bucket

  rule {
    id     = "expire-export-objects"
    status = "Enabled"

    filter {
      prefix = "exports/"
    }

    expiration {
      days = var.export_expiration_days
    }
  }

  # Optionally add per-tier archive expiration rules when retention periods
  # are reached, but Object Lock COMPLIANCE will block deletion until the
  # per-object retain-until date passes. Keep this rule absent or as a
  # safety net only; the archiver's drop gate (AL-INV-9) is authoritative.
}
