//go:build integration || e2e

package fixtures

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// FlociImage is the LocalStack-compatible emulator the sibling IAM services
// use (docker-compose.yml runs the same image).
const FlociImage = "floci/floci:2.1.0-compat"

// Floci is a running emulator provisioned by scripts/init-floci.sh — the
// exact ready-hook docker-compose mounts, so tests exercise the real dev
// topology rather than a test-only copy.
type Floci struct {
	Endpoint string
	AWS      aws.Config
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// StartFloci boots a per-test floci (see NewFloci); the container is
// terminated when t finishes.
func StartFloci(t testing.TB) *Floci {
	t.Helper()
	if testing.Short() {
		t.Skip("floci testcontainer skipped in -short mode")
	}
	f, stop, err := NewFloci(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return f
}

// NewFloci boots floci with scripts/init-floci.sh as its ready hook and
// waits until the hook's LAST resource (the iam-serviceaccount-events Glue
// registry) exists — the same readiness signal docker-compose uses. The
// returned stop func terminates the container. Usable from TestMain to
// share one emulator across a package.
func NewFloci(ctx context.Context) (*Floci, func(), error) {
	script := filepath.Join(repoRoot(), "scripts", "init-floci.sh")
	if _, err := os.Stat(script); err != nil {
		return nil, nil, fmt.Errorf("init script: %w", err)
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        FlociImage,
			ExposedPorts: []string{"4566/tcp"},
			Env: map[string]string{
				"FLOCI_DEFAULT_REGION":             "ap-south-1",
				"FLOCI_DEFAULT_ACCOUNT_ID":         "000000000000",
				"FLOCI_INIT_HOOKS_TIMEOUT_SECONDS": "120",
			},
			Files: []testcontainers.ContainerFile{{
				HostFilePath:      script,
				ContainerFilePath: "/etc/floci/init/ready.d/init-floci.sh",
				FileMode:          0o755,
			}},
			WaitingFor: wait.ForListeningPort("4566/tcp").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("start floci: %w", err)
	}
	// WithoutCancel: cleanup must still run after the caller's ctx ends.
	stopCtx := context.WithoutCancel(ctx)
	stop := func() {
		if err := c.Terminate(stopCtx); err != nil {
			fmt.Fprintf(os.Stderr, "terminate floci: %v\n", err)
		}
	}
	endpoint, err := c.PortEndpoint(ctx, "4566/tcp", "http")
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("floci endpoint: %w", err)
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("ap-south-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
		awsconfig.WithBaseEndpoint(endpoint),
	)
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("aws config: %w", err)
	}
	g := glue.NewFromConfig(cfg)
	deadline := time.Now().Add(120 * time.Second)
	for {
		_, err := g.GetRegistry(ctx, &glue.GetRegistryInput{RegistryId: registryID("iam-serviceaccount-events")})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			stop()
			return nil, nil, fmt.Errorf("init-floci.sh did not finish: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return &Floci{Endpoint: endpoint, AWS: cfg}, stop, nil
}
