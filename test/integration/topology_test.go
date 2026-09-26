//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/config"
)

// The dev topology scripts/init-floci.sh provisions matches LLD §7.1 and
// the service's own frozen queue catalog: every inbound queue exists, has
// <queue>-dlq, and redrives at maxReceiveCount=5.
func TestTopology_QueuesAndDLQsMatchCatalog(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, q := range config.InboundQueues() {
		url := e.queueURL(t, q.Name)
		dlqURL := e.queueURL(t, q.Name+"-dlq")

		dlqAttrs, err := e.sqs.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl: aws.String(dlqURL), AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
		})
		require.NoError(t, err)

		attrs, err := e.sqs.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl: aws.String(url), AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameRedrivePolicy},
		})
		require.NoError(t, err)
		var redrive struct {
			Target string `json:"deadLetterTargetArn"`
			Max    any    `json:"maxReceiveCount"`
		}
		require.NoError(t, json.Unmarshal([]byte(attrs.Attributes["RedrivePolicy"]), &redrive), q.Name)
		assert.Equal(t, dlqAttrs.Attributes["QueueArn"], redrive.Target, q.Name)
		assert.EqualValues(t, "5", toString(redrive.Max), "%s: maxReceiveCount (HLD §9.1)", q.Name)
	}
}

// AL-EVT-2: every audit subscription is catch-all (no filter policy) and
// uses raw delivery (platform-events requires RawMessageDelivery=true).
func TestTopology_SubscriptionsAreCatchAllRawDelivery_ALEVT2(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	subs, err := e.sns.ListSubscriptions(ctx, &sns.ListSubscriptionsInput{})
	require.NoError(t, err)

	byQueue := map[string]string{}
	for _, s := range subs.Subscriptions {
		arn := aws.ToString(s.Endpoint)
		byQueue[arn[len("arn:aws:sqs:ap-south-1:"+account+":"):]] = aws.ToString(s.SubscriptionArn)
	}
	for _, q := range config.InboundQueues() {
		subARN, ok := byQueue[q.Name]
		require.True(t, ok, "no SNS subscription for %s", q.Name)
		attrs, err := e.sns.GetSubscriptionAttributes(ctx, &sns.GetSubscriptionAttributesInput{SubscriptionArn: aws.String(subARN)})
		require.NoError(t, err)
		assert.Equal(t, "true", attrs.Attributes["RawMessageDelivery"], q.Name)
		assert.Empty(t, attrs.Attributes["FilterPolicy"], "%s must be catch-all (AL-EVT-2)", q.Name)
	}
}

// LLD §15.4: the archive bucket exists with Object Lock enabled.
func TestTopology_ArchiveBucketHasObjectLock(t *testing.T) {
	e := newEnv(t)
	out, err := s3.NewFromConfig(e.floci.AWS, func(o *s3.Options) { o.UsePathStyle = true }).
		GetObjectLockConfiguration(context.Background(), &s3.GetObjectLockConfigurationInput{Bucket: aws.String("iam-audit-archive")})
	require.NoError(t, err)
	require.NotNil(t, out.ObjectLockConfiguration)
	assert.Equal(t, s3types.ObjectLockEnabledEnabled, out.ObjectLockConfiguration.ObjectLockEnabled)
}

// LLD §7.3.1: the six iam.* Glue registries exist (read-only for this service).
func TestTopology_GlueRegistries(t *testing.T) {
	e := newEnv(t)
	g := glue.NewFromConfig(e.floci.AWS)
	for _, r := range []string{
		"iam-auth-events", "iam-user-events", "iam-membership-events",
		"iam-tenant-events", "iam-delegation-events", "iam-serviceaccount-events",
	} {
		_, err := g.GetRegistry(context.Background(), &glue.GetRegistryInput{RegistryId: &gluetypes.RegistryId{RegistryName: aws.String(r)}})
		assert.NoError(t, err, r)
	}
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return json.Number(jsonNum(x)).String()
	default:
		return ""
	}
}

func jsonNum(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}
