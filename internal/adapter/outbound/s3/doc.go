// Package s3 is the archive object store adapter (aws-sdk-go-v2): SSE-KMS,
// S3 Object Lock (COMPLIANCE), Glacier Instant Retrieval (LLD §15.4).
// It is the only package that talks to S3 (LLD §3.2). Read + export paths: Phase 4; archive writes: Phase 7.
package s3
