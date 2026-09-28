package main

import (
	"context"
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

// Against an emulator endpoint the store uses path-style addressing on that
// endpoint; in AWS it uses the regional virtual-hosted endpoint.
func TestBuildStore_EndpointAddressing(t *testing.T) {
	ctx := context.Background()
	cfg := config.Server{}
	cfg.ArchiveBucket, cfg.ArchiveKMSKey = "iam-audit-archive", "alias/iam-audit-archive"

	url, err := buildStore(staticAWS(), cfg).PresignExport(ctx, "exports/t/e.jsonl.gz", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "https://iam-audit-archive.s3.ap-south-1.amazonaws.com/exports/t/e.jsonl.gz?") {
		t.Errorf("AWS url = %s", url)
	}

	cfg.AWSEndpoint = "http://localhost:4573"
	url, err = buildStore(staticAWS(), cfg).PresignExport(ctx, "exports/t/e.jsonl.gz", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "http://localhost:4573/iam-audit-archive/exports/t/e.jsonl.gz?") {
		t.Errorf("emulator url = %s", url)
	}
}
