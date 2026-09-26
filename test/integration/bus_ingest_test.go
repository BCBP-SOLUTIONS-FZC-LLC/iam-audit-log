//go:build integration

package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsglue "github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/inbound/consumer"
	glueadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/glue"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/test/fixtures"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/logger"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// busStack is the real Phase 3 pipeline: platform-events consumer on floci
// → Glue codec (registry-resolved) → IngestService.IngestBus → Postgres as
// audit_app. Each test runs it on its own queue(s).
type busStack struct {
	env  *env
	seed *dbseed.Pool
}

func newBusStack(t *testing.T, queueNames ...string) *busStack {
	t.Helper()
	e := newEnv(t)
	ctx := context.Background()
	roles := fixtures.CreateRoles(t, fixtures.StartPostgres(t))
	require.NoError(t, pgadapter.RunMigrations(ctx, roles.SuperDSN, nil))
	cfg, _ := pgadapter.AppPoolConfig(roles.AppDSN, nil, nil)
	pool, err := pgcommon.NewPool(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	seed, err := dbseed.New(ctx, roles.SuperDSN)
	require.NoError(t, err)
	t.Cleanup(seed.Close)

	ingest := service.NewIngestService(pgadapter.NewAuditRepository(pool), func() (string, error) {
		id, err := uuid.NewV7()
		return id.String(), err
	}, 8192, 500, nil).WithBus(service.BusIngestConfig{
		Plans: pgadapter.NewPlanWindowRepository(pool), DefaultWindowDays: 365,
	})

	want := map[string]bool{}
	for _, n := range queueNames {
		want[n] = true
	}
	var queues []consumer.Queue
	for _, q := range config.InboundQueues() {
		if want[q.Name] {
			queues = append(queues, consumer.Queue{Name: q.Name, URL: e.queueURL(t, q.Name), Topic: q.Topic, Consumer: q.Consumer, Concurrency: 2})
		}
	}
	log, err := logger.NewLogger("test")
	require.NoError(t, err)
	build := func(url string, h events.Handler, opts ...events.ConsumerOption) (events.Consumer, error) {
		return events.NewSQSConsumerWithClient(events.SQSConfig{QueueURL: url, Region: "ap-south-1", Logger: log, WaitSeconds: 1}, e.sqs, h, opts...)
	}
	codec := glueadapter.NewCodec(glueadapter.NewRegistryResolver(awsglue.NewFromConfig(e.floci.AWS)))
	fleet, err := consumer.NewFleet(queues, build, codec, nil,
		func(q consumer.Queue) events.Handler { return consumer.Handler(q, ingest) },
		events.WithVisibilityTimeout(time.Second))
	require.NoError(t, err)
	runCtx, cancel := context.WithCancel(ctx)
	fleet.Start(runCtx, nil)
	t.Cleanup(func() { cancel(); _ = fleet.Stop() })
	return &busStack{env: e, seed: seed}
}

type row struct {
	EntryType, Tier, Mode, Topic, ActorType, SourceType, SourceService string
	ActorID, TargetType, TargetID                                      *string
	Meta                                                               string
}

func (s *busStack) waitRow(t *testing.T, sourceEventID string) row {
	t.Helper()
	var r row
	eventually(t, 45*time.Second, func() bool {
		err := s.seed.QueryRow(context.Background(), `SELECT entry_type, retention_tier::text, ingest_mode::text,
			coalesce(source_topic,''), actor_type::text, source_event_type, source_service, actor_id::text,
			target_type, target_id, metadata::text FROM audit_events WHERE source_event_id = $1`, sourceEventID).
			Scan(&r.EntryType, &r.Tier, &r.Mode, &r.Topic, &r.ActorType, &r.SourceType, &r.SourceService, &r.ActorID, &r.TargetType, &r.TargetID, &r.Meta)
		return err == nil
	}, "event never persisted: "+sourceEventID)
	return r
}

func (s *busStack) count(t *testing.T, sourceEventID string) int {
	t.Helper()
	var n int
	require.NoError(t, s.seed.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE source_event_id = $1`, sourceEventID).Scan(&n))
	return n
}

func rawEnvelope(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	base := map[string]any{"source": "iam-org-membership", "specversion": "1", "tenant_id": tenantA,
		"time": time.Now().UTC().Format(time.RFC3339Nano)}
	for k, v := range fields {
		base[k] = v
	}
	b, err := json.Marshal(base)
	require.NoError(t, err)
	return b
}

const memberUser = "7f3c2d1e-9a8b-4c5d-8e7f-0a1b2c3d4e5f"

// §14 bus → Postgres: a producer's raw SNS publish is persisted as one
// audit_events row with the §7.1 classification and full provenance
// (AL-INV-5); a redelivery of the same envelope id is a no-op (AL-INV-4).
func TestBus_PublishedEventPersistedOnceDespiteRedelivery_ALINV4_ALINV5(t *testing.T) {
	s := newBusStack(t, "membership-audit-q")
	id := uuid.NewString()
	body := rawEnvelope(t, map[string]any{"id": id, "type": "DepartmentMembershipGranted", "actor": "iam-system",
		"subject": memberUser, "data": map[string]any{"user_id": memberUser, "department_id": uuid.NewString(), "level": "approver", "actor_id": memberUser}})
	s.env.publish(t, "iam-membership-events", body)
	s.env.publish(t, "iam-membership-events", body) // at-least-once redelivery

	r := s.waitRow(t, id)
	assert.Equal(t, "membership.department.granted", r.EntryType)
	assert.Equal(t, "compliance_7y", r.Tier)
	assert.Equal(t, "bus", r.Mode)
	assert.Equal(t, "iam.membership.events", r.Topic)
	assert.Equal(t, "iam_system", r.ActorType, "D-8 rule 2")
	assert.Equal(t, "DepartmentMembershipGranted", r.SourceType)
	assert.Equal(t, "iam-org-membership", r.SourceService)
	require.NotNil(t, r.TargetID)
	assert.Equal(t, memberUser, *r.TargetID)

	time.Sleep(3 * time.Second) // let the duplicate be consumed
	assert.Equal(t, 1, s.count(t, id), "redelivery must not create a second row")
}

// AL-EVT-4: an unrecognized type is persisted as <domain>.unknown, not dropped.
func TestBus_UnknownTypePersisted_ALEVT4(t *testing.T) {
	s := newBusStack(t, "tender-audit-q")
	id := uuid.NewString()
	s.env.publish(t, "tender-events", rawEnvelope(t, map[string]any{"id": id, "type": "TenderShredded", "source": "tender-svc",
		"actor": memberUser, "data": map[string]any{"tender_id": "t-1"}}))
	r := s.waitRow(t, id)
	assert.Equal(t, "tender.unknown", r.EntryType)
	assert.Equal(t, "security_3y", r.Tier)
	assert.Equal(t, "user", r.ActorType)
}

// D-7: a Workflow Engine wire type is classified via its alias.
func TestBus_WorkflowWireAlias_D7(t *testing.T) {
	s := newBusStack(t, "wf-workflow-audit-q")
	id := uuid.NewString()
	s.env.publish(t, "wf-workflow-events", rawEnvelope(t, map[string]any{"id": id, "type": "workflow.task.sla-breached",
		"source": "workflow-execution-svc", "data": map[string]any{"task_id": "k-1", "workflow_instance_id": "i-1"}}))
	r := s.waitRow(t, id)
	assert.Equal(t, "workflow.task.sla_breached", r.EntryType)
	assert.Contains(t, r.Meta, `"_actor_unattributed": true`, "D-8 rule 4")
}

// §4.2: a plan-carrying billing event updates tenant_plan_window.
func TestBus_PlanProjection(t *testing.T) {
	s := newBusStack(t, "billing-audit-q")
	id := uuid.NewString()
	s.env.publish(t, "billing-events", rawEnvelope(t, map[string]any{"id": id, "type": "TenantPlanChanged", "source": "billing",
		"data": map[string]any{"plan": "enterprise"}}))
	s.waitRow(t, id)
	var plan string
	eventually(t, 10*time.Second, func() bool {
		return s.seed.QueryRow(context.Background(), `SELECT plan_code FROM tenant_plan_window WHERE tenant_id = $1`, tenantA).Scan(&plan) == nil
	}, "tenant_plan_window not projected")
	assert.Equal(t, "enterprise", plan)
}

// registerSchema registers a JSON schema in a floci Glue registry and
// returns its schema-version id (what a producer's GlueCodec embeds).
func (s *busStack) registerSchema(t *testing.T, registry, name, def string) string {
	t.Helper()
	out, err := awsglue.NewFromConfig(s.env.floci.AWS).CreateSchema(context.Background(), &awsglue.CreateSchemaInput{
		RegistryId: &gluetypes.RegistryId{RegistryName: aws.String(registry)}, SchemaName: aws.String(name),
		DataFormat: gluetypes.DataFormatJson, Compatibility: gluetypes.CompatibilityNone, SchemaDefinition: aws.String(def),
	})
	require.NoError(t, err)
	return aws.ToString(out.SchemaVersionId)
}

// glueEnvelope builds the wire shape a Glue-enabled producer publishes:
// data = base64(18-byte header + JSON), dataschema = schema-version id.
func glueEnvelope(t *testing.T, id, typ, versionID string, payload []byte) []byte {
	t.Helper()
	v := uuid.MustParse(versionID)
	framed := append(append([]byte{0x03, 0x00}, v[:]...), payload...)
	return rawEnvelope(t, map[string]any{"id": id, "type": typ, "source": "iam-delegation", "actor": "iam-system",
		"dataschema": versionID, "data": base64.StdEncoding.EncodeToString(framed)})
}

const delegationSchema = `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object",
 "required":["delegation_id","delegator_id","delegate_id"],"additionalProperties":true,
 "properties":{"delegation_id":{"type":"string"},"delegator_id":{"type":"string"},"delegate_id":{"type":"string"}}}`

// AL-D12 / §7.3.1: a Glue-framed payload (dataschema populated) is decoded
// and validated against the producer's registered schema version, then
// persisted with the decoded payload as metadata.
func TestBus_GlueFramedPayloadDecodedAndValidated_ALD12(t *testing.T) {
	s := newBusStack(t, "delegation-audit-q")
	version := s.registerSchema(t, "iam-delegation-events", "DelegationStarted", delegationSchema)
	id := uuid.NewString()
	payload := []byte(`{"delegation_id":"d-1","delegator_id":"a","delegate_id":"b","scope":"all"}`)
	s.env.publish(t, "iam-delegation-events", glueEnvelope(t, id, "DelegationStarted", version, payload))

	r := s.waitRow(t, id)
	assert.Equal(t, "delegation.started", r.EntryType)
	assert.Contains(t, r.Meta, `"delegation_id": "d-1"`)
	require.NotNil(t, r.TargetID)
	assert.Equal(t, "d-1", *r.TargetID)
}

// §7.3.1 / AL-EVT-4: a payload that fails its registered schema is a decode
// failure — never persisted, redelivered, then redriven to the DLQ for
// operator replay (RB-1).
func TestBus_SchemaViolationRedrivesToDLQ_ALEVT4(t *testing.T) {
	s := newBusStack(t, "serviceaccount-audit-q")
	version := s.registerSchema(t, "iam-serviceaccount-events", "ServiceAccountRevoked",
		`{"type":"object","required":["principal_id"],"properties":{"principal_id":{"type":"string"}}}`)
	id := uuid.NewString()
	s.env.publish(t, "iam-serviceaccount-events", glueEnvelope(t, id, "ServiceAccountRevoked", version, []byte(`{"principal_id":42}`)))

	dlq := s.env.queueURL(t, "serviceaccount-audit-q-dlq")
	eventually(t, 90*time.Second, func() bool { return s.env.approxCount(t, dlq) == 1 }, "schema-violating message never reached the DLQ")
	assert.Equal(t, 0, s.count(t, id), "a decode failure must never be persisted")
}

// AL-INV-2: a bus row and a direct-write row share one table, one RLS
// policy and one shape — only ingest_mode/source_topic differ.
func TestBus_SameRowShapeAsDirectWrite_ALINV2(t *testing.T) {
	s := newBusStack(t, "usage-audit-q")
	id := uuid.NewString()
	s.env.publish(t, "usage-events", rawEnvelope(t, map[string]any{"id": id, "type": "TenantQuotaWarning", "source": "usage-metering",
		"data": map[string]any{"quota": "llm_tokens", "percent": 80}}))
	r := s.waitRow(t, id)
	assert.Equal(t, "usage.quota.warning", r.EntryType)
	assert.Equal(t, "access_90d", r.Tier)

	var cols int
	require.NoError(t, s.seed.QueryRow(context.Background(), `SELECT count(*) FROM audit_events
		WHERE source_event_id = $1 AND id IS NOT NULL AND occurred_at IS NOT NULL AND recorded_at IS NOT NULL
		  AND action IS NOT NULL AND jsonb_typeof(metadata) = 'object'`, id).Scan(&cols))
	assert.Equal(t, 1, cols, "every NOT NULL column populated exactly as a direct-write row")
}
