//go:build integration

package integration_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/service"
)

// queueDepth mirrors cmd/server's sqsDepth (unexported in package main):
// GetQueueAttributes → ApproximateNumberOfMessages.
type queueDepth struct{ c *sqs.Client }

func (d queueDepth) Depth(ctx context.Context, url string) (int64, error) {
	out, err := d.c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(url),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameApproximateNumberOfMessages},
	})
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(out.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessages)], 10, 64)
}

type opsRec struct {
	mu  sync.Mutex
	dlq map[string]int64
}

func (r *opsRec) SetOpsStats(domain.OpsStats) {}
func (r *opsRec) SetDLQDepth(q string, d int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dlq == nil {
		r.dlq = map[string]int64{}
	}
	r.dlq[q] = d
}

func (r *opsRec) get(q string) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.dlq[q]
	return d, ok
}

type noStats struct{}

func (noStats) OpsStats(context.Context, domain.OpsStatsQuery) (domain.OpsStats, error) {
	return domain.OpsStats{}, nil
}

// D-21 / RB-1: the OpsMonitor publishes a real DLQ's depth from floci. The
// floci is shared per package, so the DLQ is purged before and after.
func TestOps_DLQDepthGauge_RB1(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dlq := e.queueURL(t, "billing-audit-q-dlq")
	purge := func() {
		_, err := e.sqs.PurgeQueue(ctx, &sqs.PurgeQueueInput{QueueUrl: aws.String(dlq)})
		require.NoError(t, err)
	}
	purge()
	t.Cleanup(purge)

	rec := &opsRec{}
	mon := service.NewOpsMonitor(noStats{}, queueDepth{c: e.sqs}, rec,
		service.OpsMonitorConfig{Interval: 5 * time.Second, DLQs: map[string]string{"billing-audit-q-dlq": dlq}}, nil)

	mon.Collect(ctx)
	d, ok := rec.get("billing-audit-q-dlq")
	require.True(t, ok, "the DLQ gauge must be set even when empty")
	require.Zero(t, d, "purged DLQ reads 0")

	for _, body := range []string{`{"lost":1}`, `{"lost":2}`} {
		_, err := e.sqs.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(dlq), MessageBody: aws.String(body)})
		require.NoError(t, err)
	}
	eventually(t, 30*time.Second, func() bool {
		mon.Collect(ctx)
		d, _ := rec.get("billing-audit-q-dlq")
		return d >= 2
	}, "DLQ depth gauge never reached 2")
}
