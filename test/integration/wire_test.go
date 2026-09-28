//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/logger"
)

// startConsumer runs a platform-events SQS consumer on url the way the
// Phase 3 fleet will (events.NewSQSConsumerWithClient — the only sanctioned
// consumer constructor, check-forbidden-events-bypass.sh).
func (e *env) startConsumer(t *testing.T, url string, h events.Handler, opts ...events.ConsumerOption) {
	t.Helper()
	log, err := logger.NewLogger("test")
	require.NoError(t, err)
	c, err := events.NewSQSConsumerWithClient(
		events.SQSConfig{QueueURL: url, Region: "ap-south-1", Logger: log, WaitSeconds: 1},
		e.sqs, h, opts...,
	)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = c.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = c.Stop()
	})
}

// A producer's raw SNS publish reaches the platform-events handler with the
// envelope intact — id (the AL-INV-4 dedup key, never rewritten, AL-EVT-5),
// type, tenant — and a nil return acks (deletes) the message.
func TestWire_RawSNSDeliveryReachesConsumerIntact_ALEVT5(t *testing.T) {
	e := newEnv(t)
	url := e.queueURL(t, "auth-audit-q")

	var (
		mu  sync.Mutex
		got []events.Envelope[json.RawMessage]
	)
	e.startConsumer(t, url, func(_ context.Context, env events.Envelope[json.RawMessage]) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, env)
		return nil
	})

	const id = "6f1e0e8a-1c2b-5c7a-9a3d-000000000001"
	e.publish(t, "iam-auth-events", envelope(t, id, "LoginSuccess", tenantA))

	eventually(t, 30*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	}, "consumer never received the event")
	mu.Lock()
	env := got[0]
	mu.Unlock()
	assert.Equal(t, id, env.ID)
	assert.Equal(t, "LoginSuccess", env.Type)
	assert.Equal(t, tenantA, env.TenantID)
	assert.JSONEq(t, `{"user_id":"u-1"}`, string(env.Payload))
	eventually(t, 30*time.Second, func() bool { return e.approxCount(t, url) == 0 }, "acked message was not deleted")
}

// AL-EVT-4 / RB-1 transport contract: a message the handler never acks is
// redelivered and, at maxReceiveCount=5, lands in <queue>-dlq (never
// dropped). The dead-letter handler fires on the 5th delivery and returns
// an error so SQS's native redrive — not the library — moves it (the
// sibling pattern the Phase 3 fleet adopts).
func TestWire_UnackedMessageRedrivesToDLQ_ALEVT4(t *testing.T) {
	e := newEnv(t)
	url := e.queueURL(t, "user-audit-q")
	dlq := e.queueURL(t, "user-audit-q-dlq")
	// The floci is shared per package: drop anything earlier tests left on
	// this queue pair so the DLQ count and the delivery count are ours alone.
	for _, q := range []string{url, dlq} {
		_, err := e.sqs.PurgeQueue(context.Background(), &sqs.PurgeQueueInput{QueueUrl: aws.String(q)})
		require.NoError(t, err)
	}
	const eventID = "0190a1b2-0000-7000-8000-000000000002"

	var deliveries, deadLettered atomic.Int32
	e.startConsumer(t, url,
		func(_ context.Context, env events.Envelope[json.RawMessage]) error {
			if env.ID == eventID {
				deliveries.Add(1)
			}
			return errors.New("transient: db down")
		},
		events.WithVisibilityTimeout(time.Second),
		events.WithMaxReceiveCount(4),
		events.WithDeadLetterHandler(func(context.Context, events.Envelope[json.RawMessage]) error {
			deadLettered.Add(1)
			return errors.New("leave for SQS redrive")
		}),
	)

	e.publish(t, "iam-user-events", envelope(t, eventID, "UserUpdated", tenantA))

	eventually(t, 90*time.Second, func() bool { return e.approxCount(t, dlq) == 1 }, "message never reached the DLQ")
	// Receives 1..3 reach the handler; receive 4 (= maxReceiveCount) goes to
	// the dead-letter handler, which returns an error so SQS redrives on 5.
	assert.GreaterOrEqual(t, deliveries.Load(), int32(3), "handler must see the redeliveries before the threshold")
	assert.GreaterOrEqual(t, deadLettered.Load(), int32(1), "dead-letter handler must fire at the threshold, before redrive")
	assert.GreaterOrEqual(t, deliveries.Load()+deadLettered.Load(), int32(4), "every receive up to maxReceiveCount is observed")
}

// Decision D-3 (BUILD_PLAN §C): platform-events v1.4.0 deletes a message
// whose body is not a valid envelope — no retry, no DLQ, handler never
// called. This test pins the library behavior so the Critical
// events_consumed_total{status="malformed"} alert stays justified; if an
// upstream fix starts leaving such messages for the DLQ, this test fails
// and the D-3 workaround can be retired.
func TestWire_MalformedEnvelopeDeletedByLibrary_KnownGapD3(t *testing.T) {
	e := newEnv(t)
	url := e.queueURL(t, "tenant-audit-q")
	dlq := e.queueURL(t, "tenant-audit-q-dlq")

	var calls atomic.Int32
	e.startConsumer(t, url, func(context.Context, events.Envelope[json.RawMessage]) error {
		calls.Add(1)
		return nil
	}, events.WithVisibilityTimeout(time.Second))

	e.publish(t, "iam-tenant-events", []byte("{not an envelope"))

	eventually(t, 30*time.Second, func() bool { return e.approxCount(t, url) == 0 }, "malformed message was not consumed")
	time.Sleep(2 * time.Second)
	assert.Zero(t, calls.Load(), "handler must never see a malformed body")
	assert.Zero(t, e.approxCount(t, dlq), "known gap D-3: the library deletes it instead of DLQ'ing")
}
