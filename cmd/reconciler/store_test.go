package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/config"
)

func staticAWS() aws.Config {
	return aws.Config{Region: "ap-south-1", Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", "")}
}

// In AWS the archive store addresses the regional virtual-hosted bucket.
func TestBuildArchiveStore_AWSAddressing(t *testing.T) {
	cfg := config.Reconciler{}
	cfg.ArchiveBucket, cfg.ArchiveKMSKey, cfg.ArchiveObjectLockMode = "iam-audit-archive", "alias/k", "COMPLIANCE"
	s := buildArchiveStore(staticAWS(), cfg)
	if s.Bucket() != "iam-audit-archive" {
		t.Errorf("bucket = %s", s.Bucket())
	}
	url, err := s.PresignExport(context.Background(), "security_3y/t/2026/03/p.jsonl.gz", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "https://iam-audit-archive.s3.ap-south-1.amazonaws.com/security_3y/") {
		t.Errorf("AWS url = %s", url)
	}
}

// Against an emulator endpoint: path-style addressing, and lock headers
// are not required back (a GET without them still verifies).
func TestBuildArchiveStore_EmulatorSkipsLockVerify(t *testing.T) {
	body := []byte("archived")
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	cfg := config.Reconciler{}
	cfg.ArchiveBucket, cfg.ArchiveKMSKey, cfg.ArchiveObjectLockMode = "iam-audit-archive", "alias/k", "COMPLIANCE"
	cfg.AWSEndpoint = srv.URL
	sum, err := buildArchiveStore(staticAWS(), cfg).ArchiveChecksum(context.Background(), "security_3y/t/p.jsonl.gz", time.Now().AddDate(3, 0, 0))
	if err != nil {
		t.Fatalf("emulator verify must not require lock headers: %v", err)
	}
	want := sha256.Sum256(body)
	if sum != hex.EncodeToString(want[:]) {
		t.Errorf("sum = %s", sum)
	}
	if path != "/iam-audit-archive/security_3y/t/p.jsonl.gz" {
		t.Errorf("path-style request path = %s", path)
	}
}
