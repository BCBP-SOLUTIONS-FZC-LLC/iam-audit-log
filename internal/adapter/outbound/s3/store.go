package s3

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// API is the subset of *s3.Client the store uses.
type API interface {
	GetObject(ctx context.Context, in *awss3.GetObjectInput, opts ...func(*awss3.Options)) (*awss3.GetObjectOutput, error)
	PutObject(ctx context.Context, in *awss3.PutObjectInput, opts ...func(*awss3.Options)) (*awss3.PutObjectOutput, error)
}

// ObjectLock configures archive writes (LLD §10.4, §15.4).
type ObjectLock struct {
	// Mode is AUDIT_ARCHIVE_OBJECT_LOCK_MODE (COMPLIANCE in every
	// non-dev environment, enforced by config); "" sends no lock headers.
	Mode string
	// Verify requires ArchiveChecksum to see the object's lock mode and a
	// retain-until at least the tier's. It is off only against an emulator
	// that does not echo lock headers.
	Verify bool
}

// Store reads archive objects and writes/presigns export objects in the
// archive bucket (LLD §15.4, §5.4 AL-3/AL-4).
type Store struct {
	api     API
	presign func(ctx context.Context, in *awss3.GetObjectInput, ttl time.Duration) (string, error)
	bucket  string
	kmsKey  string
	lock    ObjectLock
}

var (
	_ port.ArchiveReader = (*Store)(nil)
	_ port.ExportStore   = (*Store)(nil)
	_ port.ArchiveStore  = (*Store)(nil)
)

// WithObjectLock returns s configured for archive writes.
func (s *Store) WithObjectLock(l ObjectLock) *Store {
	c := *s
	c.lock = l
	return &c
}

// Bucket implements port.ArchiveStore.
func (s *Store) Bucket() string { return s.bucket }

// PutArchive implements port.ArchiveStore: an SSE-KMS object with Object
// Lock retained until retainUntil. The bucket is versioned (Object Lock
// requires it), so re-archiving a key adds a version and never replaces a
// locked one (D-20).
func (s *Store) PutArchive(ctx context.Context, key string, body io.ReadSeeker, size int64, retainUntil time.Time) error {
	in := &awss3.PutObjectInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(key),
		Body:            body,
		ContentLength:   aws.Int64(size),
		ContentType:     aws.String("application/x-ndjson"),
		ContentEncoding: aws.String("gzip"),
	}
	if s.kmsKey != "" {
		in.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		in.SSEKMSKeyId = aws.String(s.kmsKey)
	}
	if s.lock.Mode != "" {
		in.ObjectLockMode = types.ObjectLockMode(s.lock.Mode)
		in.ObjectLockRetainUntilDate = aws.Time(retainUntil.UTC())
	}
	if _, err := s.api.PutObject(ctx, in); err != nil {
		return classify(err)
	}
	return nil
}

// ArchiveChecksum implements port.ArchiveStore: it GETs the object's
// current version, hashes the body, and (when Verify is set) checks the
// Object Lock echoed back covers retainUntil.
func (s *Store) ArchiveChecksum(ctx context.Context, key string, retainUntil time.Time) (sum string, err error) {
	out, err := s.api.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return "", classify(err)
	}
	defer func() { err = errors.Join(err, out.Body.Close()) }()
	if s.lock.Verify {
		if string(out.ObjectLockMode) != s.lock.Mode {
			return "", fmt.Errorf("archive %s: object lock mode %q, want %q", key, out.ObjectLockMode, s.lock.Mode)
		}
		if out.ObjectLockRetainUntilDate == nil || out.ObjectLockRetainUntilDate.Before(retainUntil.Truncate(time.Second)) {
			return "", fmt.Errorf("archive %s: object lock retain-until %v is before %v", key, out.ObjectLockRetainUntilDate, retainUntil)
		}
	}
	h := sha256.New()
	if _, err := io.Copy(h, out.Body); err != nil {
		return "", classify(fmt.Errorf("archive %s: read: %w", key, err))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// New wires a Store over client. kmsKey "" omits SSE-KMS (dev/floci only;
// config never defaults it empty).
func New(client *awss3.Client, bucket, kmsKey string) *Store {
	pc := awss3.NewPresignClient(client)
	return &Store{
		api: client, bucket: bucket, kmsKey: kmsKey,
		presign: func(ctx context.Context, in *awss3.GetObjectInput, ttl time.Duration) (string, error) {
			req, err := pc.PresignGetObject(ctx, in, awss3.WithPresignExpires(ttl))
			if err != nil {
				return "", err
			}
			return req.URL, nil
		},
	}
}

// NewWithAPI wires a Store over a fake API (tests); presign returns a
// deterministic pseudo-URL.
func NewWithAPI(api API, bucket, kmsKey string, presign func(ctx context.Context, in *awss3.GetObjectInput, ttl time.Duration) (string, error)) *Store {
	return &Store{api: api, presign: presign, bucket: bucket, kmsKey: kmsKey}
}

// maxRecordBytes bounds one JSONL line (metadata ≤ 8 KiB plus the row).
const maxRecordBytes = 1 << 20

// ReadArchive implements port.ArchiveReader: GET → gunzip → one
// ArchiveRecord per line.
func (s *Store) ReadArchive(ctx context.Context, bucket, key string, fn func(domain.ArchiveRecord) error) (err error) {
	out, err := s.api.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return classify(err)
	}
	defer func() { err = errors.Join(err, out.Body.Close()) }()
	zr, err := gzip.NewReader(out.Body)
	if err != nil {
		return fmt.Errorf("archive %s: gzip: %w", key, err)
	}
	defer func() { err = errors.Join(err, zr.Close()) }()
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 64<<10), maxRecordBytes)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var rec domain.ArchiveRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return fmt.Errorf("archive %s: decode record: %w", key, err)
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return classify(fmt.Errorf("archive %s: read: %w", key, err))
	}
	return nil
}

// PutExport implements port.ExportStore (SSE-KMS unless kmsKey is empty).
func (s *Store) PutExport(ctx context.Context, key string, body io.ReadSeeker, size int64) error {
	in := &awss3.PutObjectInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(key),
		Body:            body,
		ContentLength:   aws.Int64(size),
		ContentType:     aws.String("application/x-ndjson"),
		ContentEncoding: aws.String("gzip"),
	}
	if s.kmsKey != "" {
		in.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		in.SSEKMSKeyId = aws.String(s.kmsKey)
	}
	if _, err := s.api.PutObject(ctx, in); err != nil {
		return classify(err)
	}
	return nil
}

// PresignExport implements port.ExportStore (decision D-11: a fresh
// short-lived URL per AL-4 poll).
func (s *Store) PresignExport(ctx context.Context, key string, ttl time.Duration) (string, error) {
	url, err := s.presign(ctx, &awss3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}, ttl)
	if err != nil {
		return "", classify(err)
	}
	return url, nil
}

// classify maps a missing object to port.ErrObjectMissing and every other
// S3 failure to 503 dependency_unavailable (§5.5: "S3 (archived read)
// unavailable"), keeping the cause for logs.
func classify(err error) error {
	var nsk *types.NoSuchKey
	var api smithy.APIError
	if errors.As(err, &nsk) || (errors.As(err, &api) && api.ErrorCode() == "NoSuchKey") {
		return fmt.Errorf("%w: %w", port.ErrObjectMissing, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &unavailable{cause: err}
}

type unavailable struct{ cause error }

func (u *unavailable) Error() string { return "s3 unavailable: " + u.cause.Error() }
func (u *unavailable) Unwrap() []error {
	return []error{domain.NewError(domain.ErrDependencyUnavailable, "archive storage unavailable"), u.cause}
}
