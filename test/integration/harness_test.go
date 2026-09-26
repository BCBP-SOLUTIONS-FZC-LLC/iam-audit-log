//go:build integration

// Package integration_test is the cross-layer tier (LLD §14 "Consumer" /
// "Reconciler" rows), mirroring iam-org-membership/test/integration: real
// SNS→SQS wiring on floci, provisioned by the repo's own
// scripts/init-floci.sh, consumed through platform-events exactly as the
// Phase 3 consumer fleet will be.
//
// Phase 0 covers the transport contract the fleet relies on: the
// provisioned topology matches LLD §7.1, raw SNS delivery reaches the
// platform-events consumer intact, un-acked messages redrive to the DLQ at
// maxReceiveCount=5, and the known malformed-envelope gap (decision D-3).
package integration_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/test/fixtures"
)

const (
	account = "000000000000"
	tenantA = "11111111-1111-1111-1111-111111111111"
)

type env struct {
	floci *fixtures.Floci
	sqs   *sqs.Client
	sns   *sns.Client
}

// shared is one floci per package run (started in TestMain); each test
// uses its own queue(s), so tests stay isolated.
var shared *fixtures.Floci

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(0)
	}
	f, stop, err := fixtures.NewFloci(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	shared = f
	code := m.Run()
	stop()
	os.Exit(code)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return &env{floci: shared, sqs: sqs.NewFromConfig(shared.AWS), sns: sns.NewFromConfig(shared.AWS)}
}

func (e *env) queueURL(t *testing.T, name string) string {
	t.Helper()
	out, err := e.sqs.GetQueueUrl(context.Background(), &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
	require.NoError(t, err, name)
	return aws.ToString(out.QueueUrl)
}

func (e *env) topicARN(name string) string {
	return "arn:aws:sns:ap-south-1:" + account + ":" + name
}

// publish sends body to topic the way a producer's SNS publisher does.
func (e *env) publish(t *testing.T, topic string, body []byte) {
	t.Helper()
	_, err := e.sns.Publish(context.Background(), &sns.PublishInput{
		TopicArn: aws.String(e.topicARN(topic)),
		Message:  aws.String(string(body)),
	})
	require.NoError(t, err)
}

// envelope builds a platform CloudEvents-style envelope (LLD §7.4).
func envelope(t *testing.T, id, typ, tenant string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"id": id, "type": typ, "source": "iam-event-consumer", "specversion": "1.0",
		"time": time.Now().UTC().Format(time.RFC3339Nano), "tenant_id": tenant,
		"data": map[string]any{"user_id": "u-1"},
	})
	require.NoError(t, err)
	return b
}

// approxCount returns ApproximateNumberOfMessages(+NotVisible) for url.
func (e *env) approxCount(t *testing.T, url string) int {
	t.Helper()
	out, err := e.sqs.GetQueueAttributes(context.Background(), &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(url),
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeNameApproximateNumberOfMessages,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	require.NoError(t, err)
	n := 0
	for _, k := range []sqstypes.QueueAttributeName{
		sqstypes.QueueAttributeNameApproximateNumberOfMessages,
		sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
	} {
		var v int
		_ = json.Unmarshal([]byte(out.Attributes[string(k)]), &v)
		n += v
	}
	return n
}

func eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal(msg)
}
