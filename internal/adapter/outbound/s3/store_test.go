package s3

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

type fakeAPI struct {
	body   []byte
	getErr error
	getIn  *awss3.GetObjectInput
	putIn  *awss3.PutObjectInput
	putB   []byte
	putErr error
	closed bool
}

type trackingBody struct {
	io.Reader
	api *fakeAPI
}

func (b trackingBody) Close() error { b.api.closed = true; return nil }

func (f *fakeAPI) GetObject(_ context.Context, in *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	f.getIn = in
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &awss3.GetObjectOutput{Body: trackingBody{Reader: bytes.NewReader(f.body), api: f}}, nil
}

func (f *fakeAPI) PutObject(_ context.Context, in *awss3.PutObjectInput, _ ...func(*awss3.Options)) (*awss3.PutObjectOutput, error) {
	f.putIn = in
	if in.Body != nil {
		f.putB, _ = io.ReadAll(in.Body)
	}
	return &awss3.PutObjectOutput{}, f.putErr
}

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	_, err := zw.Write([]byte(s))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return b.Bytes()
}

func noPresign(context.Context, *awss3.GetObjectInput, time.Duration) (string, error) {
	return "", errors.New("unused")
}

const (
	rec1 = `{"id":"a","occurred_at":"2026-01-02T00:00:00Z","tenant_id":"t","entry_type":"x.y","metadata":{"k":1}}`
	rec2 = `{"id":"b","occurred_at":"2026-01-01T00:00:00Z","tenant_id":"t","entry_type":"x.z","metadata":{}}`
)

func TestReadArchive_StreamsRecordsSkippingBlankLines(t *testing.T) {
	api := &fakeAPI{body: gz(t, rec1+"\n\n"+rec2+"\n")}
	s := NewWithAPI(api, "exports-bucket", "", noPresign)
	var got []domain.ArchiveRecord
	err := s.ReadArchive(context.Background(), "archive-bucket", "k1", func(r domain.ArchiveRecord) error {
		got = append(got, r)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].ID)
	assert.Equal(t, "x.z", got[1].EntryType)
	assert.JSONEq(t, `{"k":1}`, string(got[0].Metadata))
	assert.Equal(t, "archive-bucket", aws.ToString(api.getIn.Bucket), "reads the manifest's bucket, not the export bucket")
	assert.Equal(t, "k1", aws.ToString(api.getIn.Key))
	assert.True(t, api.closed, "body closed")
}

func TestReadArchive_Errors(t *testing.T) {
	ctx := context.Background()
	nop := func(domain.ArchiveRecord) error { return nil }

	err := NewWithAPI(&fakeAPI{body: []byte("not gzip")}, "b", "", noPresign).ReadArchive(ctx, "b", "k", nop)
	assert.ErrorContains(t, err, "gzip")

	err = NewWithAPI(&fakeAPI{body: gz(t, rec1+"\n{broken\n")}, "b", "", noPresign).ReadArchive(ctx, "b", "k", nop)
	assert.ErrorContains(t, err, "decode record")

	boom := errors.New("stop")
	calls := 0
	err = NewWithAPI(&fakeAPI{body: gz(t, rec1+"\n"+rec2)}, "b", "", noPresign).ReadArchive(ctx, "b", "k",
		func(domain.ArchiveRecord) error { calls++; return boom })
	assert.ErrorIs(t, err, boom)
	assert.Equal(t, 1, calls)

	long := `{"id":"` + strings.Repeat("x", maxRecordBytes+1) + `"}`
	err = NewWithAPI(&fakeAPI{body: gz(t, long)}, "b", "", noPresign).ReadArchive(ctx, "b", "k", nop)
	var de *domain.Error
	require.ErrorAs(t, err, &de, "an oversized line surfaces as a classified read error")
	assert.Equal(t, domain.ErrDependencyUnavailable, de.Code)
}

func TestReadArchive_GetErrorsClassified(t *testing.T) {
	ctx := context.Background()
	nop := func(domain.ArchiveRecord) error { return nil }
	read := func(err error) error {
		return NewWithAPI(&fakeAPI{getErr: err}, "b", "", noPresign).ReadArchive(ctx, "b", "k", nop)
	}

	assert.ErrorIs(t, read(&types.NoSuchKey{}), port.ErrObjectMissing)
	assert.ErrorIs(t, read(&smithy.GenericAPIError{Code: "NoSuchKey"}), port.ErrObjectMissing)

	err := read(&smithy.GenericAPIError{Code: "AccessDenied"})
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrDependencyUnavailable, de.Code)
	assert.Contains(t, err.Error(), "s3 unavailable")
	assert.NotErrorIs(t, err, port.ErrObjectMissing)

	for _, ce := range []error{context.Canceled, context.DeadlineExceeded} {
		err = read(ce)
		assert.ErrorIs(t, err, ce)
		assert.False(t, errors.As(err, &de), "cancellation is not a dependency failure")
	}
}

func TestPutExport_SSEKMS(t *testing.T) {
	api := &fakeAPI{}
	s := NewWithAPI(api, "bucket", "alias/k", noPresign)
	require.NoError(t, s.PutExport(context.Background(), "exports/t/e.jsonl.gz", strings.NewReader("data"), 4))
	in := api.putIn
	assert.Equal(t, "bucket", aws.ToString(in.Bucket))
	assert.Equal(t, "exports/t/e.jsonl.gz", aws.ToString(in.Key))
	assert.Equal(t, int64(4), aws.ToInt64(in.ContentLength))
	assert.Equal(t, "application/x-ndjson", aws.ToString(in.ContentType))
	assert.Equal(t, "gzip", aws.ToString(in.ContentEncoding))
	assert.Equal(t, types.ServerSideEncryptionAwsKms, in.ServerSideEncryption)
	assert.Equal(t, "alias/k", aws.ToString(in.SSEKMSKeyId))
	assert.Equal(t, "data", string(api.putB))
}

func TestPutExport_NoKMSAndErrors(t *testing.T) {
	api := &fakeAPI{}
	s := NewWithAPI(api, "bucket", "", noPresign)
	require.NoError(t, s.PutExport(context.Background(), "k", strings.NewReader(""), 0))
	assert.Empty(t, api.putIn.ServerSideEncryption)
	assert.Nil(t, api.putIn.SSEKMSKeyId)

	api.putErr = errors.New("503")
	err := s.PutExport(context.Background(), "k", strings.NewReader(""), 0)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrDependencyUnavailable, de.Code)
}

func TestPresignExport(t *testing.T) {
	var gotIn *awss3.GetObjectInput
	var gotTTL time.Duration
	s := NewWithAPI(&fakeAPI{}, "bucket", "", func(_ context.Context, in *awss3.GetObjectInput, ttl time.Duration) (string, error) {
		gotIn, gotTTL = in, ttl
		return "https://signed", nil
	})
	url, err := s.PresignExport(context.Background(), "exports/k", 15*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, "https://signed", url)
	assert.Equal(t, "bucket", aws.ToString(gotIn.Bucket))
	assert.Equal(t, "exports/k", aws.ToString(gotIn.Key))
	assert.Equal(t, 15*time.Minute, gotTTL)

	s = NewWithAPI(&fakeAPI{}, "bucket", "", func(context.Context, *awss3.GetObjectInput, time.Duration) (string, error) {
		return "", errors.New("no creds")
	})
	_, err = s.PresignExport(context.Background(), "k", time.Minute)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrDependencyUnavailable, de.Code)
}

// New wires a real SigV4 presigner; presigning is offline, so a static-cred
// client proves the URL targets the export bucket/key with the TTL.
func TestNew_PresignsOffline(t *testing.T) {
	client := awss3.New(awss3.Options{
		Region:       "ap-south-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint: aws.String("http://localhost:4573"),
		UsePathStyle: true,
	})
	s := New(client, "iam-audit-archive", "")
	url, err := s.PresignExport(context.Background(), "exports/t/e.jsonl.gz", 15*time.Minute)
	require.NoError(t, err)
	assert.Contains(t, url, "http://localhost:4573/iam-audit-archive/exports/t/e.jsonl.gz?")
	assert.Contains(t, url, "X-Amz-Expires=900")
	assert.Contains(t, url, "X-Amz-Signature=")

	bad := awss3.New(awss3.Options{Region: "ap-south-1"}) // no credentials
	_, err = New(bad, "b", "").PresignExport(context.Background(), "k", time.Minute)
	assert.Error(t, err)
}
