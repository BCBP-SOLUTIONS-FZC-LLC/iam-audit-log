package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

var retain = time.Date(2029, 3, 31, 23, 0, 0, 0, time.UTC)

// §15.4: an archive object is SSE-KMS and Object-Lock retained per tier.
func TestPutArchive_LockAndSSE(t *testing.T) {
	api := &fakeAPI{}
	s := NewWithAPI(api, "iam-audit-archive", "alias/iam-audit-archive", noPresign).
		WithObjectLock(ObjectLock{Mode: "COMPLIANCE", Verify: true})
	body := []byte("gz-bytes")
	require.NoError(t, s.PutArchive(context.Background(), "security_3y/t/2026/03/p.jsonl.gz", bytes.NewReader(body), int64(len(body)), retain.In(time.FixedZone("x", 3600))))
	in := api.putIn
	assert.Equal(t, "iam-audit-archive", aws.ToString(in.Bucket))
	assert.Equal(t, "security_3y/t/2026/03/p.jsonl.gz", aws.ToString(in.Key))
	assert.Equal(t, types.ObjectLockModeCompliance, in.ObjectLockMode)
	require.NotNil(t, in.ObjectLockRetainUntilDate)
	assert.True(t, in.ObjectLockRetainUntilDate.Equal(retain))
	assert.Equal(t, time.UTC, in.ObjectLockRetainUntilDate.Location())
	assert.Equal(t, types.ServerSideEncryptionAwsKms, in.ServerSideEncryption)
	assert.Equal(t, "alias/iam-audit-archive", aws.ToString(in.SSEKMSKeyId))
	assert.Equal(t, "gzip", aws.ToString(in.ContentEncoding))
	assert.EqualValues(t, len(body), aws.ToInt64(in.ContentLength))
	assert.Equal(t, body, api.putB)
	assert.Equal(t, "iam-audit-archive", s.Bucket())
}

// Emulator/dev: no KMS key and no lock mode → neither header is sent.
func TestPutArchive_NoLockNoKMSAndError(t *testing.T) {
	api := &fakeAPI{}
	s := NewWithAPI(api, "b", "", noPresign)
	require.NoError(t, s.PutArchive(context.Background(), "k", bytes.NewReader(nil), 0, retain))
	assert.Empty(t, api.putIn.ObjectLockMode)
	assert.Nil(t, api.putIn.ObjectLockRetainUntilDate)
	assert.Empty(t, api.putIn.ServerSideEncryption)
	assert.Nil(t, api.putIn.SSEKMSKeyId)

	api.putErr = errors.New("throttled")
	err := s.PutArchive(context.Background(), "k", bytes.NewReader(nil), 0, retain)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrDependencyUnavailable, de.Code)
}

// WithObjectLock returns a configured copy, leaving the original untouched.
func TestWithObjectLock_Copies(t *testing.T) {
	base := NewWithAPI(&fakeAPI{}, "b", "", noPresign)
	locked := base.WithObjectLock(ObjectLock{Mode: "GOVERNANCE"})
	assert.Empty(t, base.lock.Mode)
	assert.Equal(t, "GOVERNANCE", locked.lock.Mode)
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// §15.4 verify: the body SHA-256 of the current version, plus (when
// required) the lock echo covering the tier.
func TestArchiveChecksum(t *testing.T) {
	ctx := context.Background()
	body := []byte("archived object body")
	good := retain.Add(time.Hour)

	api := &fakeAPI{body: body, lockMode: types.ObjectLockModeCompliance, lockRetain: &good}
	s := NewWithAPI(api, "b", "", noPresign).WithObjectLock(ObjectLock{Mode: "COMPLIANCE", Verify: true})
	sum, err := s.ArchiveChecksum(ctx, "k", retain)
	require.NoError(t, err)
	assert.Equal(t, sha(body), sum)
	assert.True(t, api.closed, "body closed")
	assert.Equal(t, "k", aws.ToString(api.getIn.Key))

	// Sub-second retain-until (S3 stores whole seconds) is still covered.
	exact := retain
	api.lockRetain = &exact
	_, err = s.ArchiveChecksum(ctx, "k", retain.Add(500*time.Millisecond))
	require.NoError(t, err)

	api.lockMode = types.ObjectLockModeGovernance
	_, err = s.ArchiveChecksum(ctx, "k", retain)
	assert.ErrorContains(t, err, "object lock mode")

	api.lockMode = types.ObjectLockModeCompliance
	short := retain.Add(-time.Hour)
	api.lockRetain = &short
	_, err = s.ArchiveChecksum(ctx, "k", retain)
	assert.ErrorContains(t, err, "retain-until")

	api.lockRetain = nil
	_, err = s.ArchiveChecksum(ctx, "k", retain)
	assert.ErrorContains(t, err, "retain-until")

	// Verify off (emulator): lock echoes are ignored.
	off := NewWithAPI(&fakeAPI{body: body}, "b", "", noPresign).WithObjectLock(ObjectLock{Mode: "COMPLIANCE"})
	sum, err = off.ArchiveChecksum(ctx, "k", retain)
	require.NoError(t, err)
	assert.Equal(t, sha(body), sum)
}

func TestArchiveChecksum_Errors(t *testing.T) {
	ctx := context.Background()
	missing := NewWithAPI(&fakeAPI{getErr: &types.NoSuchKey{}}, "b", "", noPresign)
	_, err := missing.ArchiveChecksum(ctx, "k", retain)
	assert.ErrorIs(t, err, port.ErrObjectMissing)

	broken := NewWithAPI(&fakeAPI{body: []byte("x"), readErr: errors.New("reset")}, "b", "", noPresign)
	_, err = broken.ArchiveChecksum(ctx, "k", retain)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrDependencyUnavailable, de.Code)
}
