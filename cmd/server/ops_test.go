package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/config"
)

// dlqURLs derives <queue>-dlq for every configured queue and skips a queue
// whose URL is unset (its consumer is disabled too).
func TestDLQURLs(t *testing.T) {
	cfg := config.Server{Queues: []config.Queue{
		{Name: "user-audit-q", URL: "http://sqs/000/user-audit-q"},
		{Name: "auth-audit-q", URL: ""},
		{Name: "tender-audit-q", URL: "http://sqs/000/tender-audit-q"},
	}}
	got := dlqURLs(cfg)
	want := map[string]string{
		"user-audit-q-dlq":   "http://sqs/000/user-audit-q-dlq",
		"tender-audit-q-dlq": "http://sqs/000/tender-audit-q-dlq",
	}
	if len(got) != len(want) {
		t.Fatalf("dlqURLs = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(dlqURLs(config.Server{})) != 0 {
		t.Error("no queues → no DLQs")
	}
}

// sqsDepth reads ApproximateNumberOfMessages via GetQueueAttributes (SQS
// JSON protocol), against the emulator-endpoint client newSQSClient builds.
func TestSQSDepth(t *testing.T) {
	var reply atomic.Value
	reply.Store(`{"Attributes":{"ApproximateNumberOfMessages":"7"}}`)
	var status atomic.Int32
	status.Store(http.StatusOK)
	var target, gotURL atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target.Store(r.Header.Get("X-Amz-Target"))
		body, _ := io.ReadAll(r.Body)
		var in struct {
			QueueURL       string   `json:"QueueUrl"`
			AttributeNames []string `json:"AttributeNames"`
		}
		_ = json.Unmarshal(body, &in)
		gotURL.Store(in.QueueURL + "|" + strings.Join(in.AttributeNames, ","))
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, reply.Load().(string))
	}))
	defer srv.Close()

	cfg := config.Server{}
	cfg.AWSEndpoint = srv.URL
	d := sqsDepth{c: newSQSClient(staticAWS(), cfg)}
	ctx := context.Background()

	n, err := d.Depth(ctx, srv.URL+"/000000000000/user-audit-q-dlq")
	if err != nil || n != 7 {
		t.Fatalf("depth = %d, err = %v", n, err)
	}
	if got := target.Load(); got != "AmazonSQS.GetQueueAttributes" {
		t.Errorf("X-Amz-Target = %v", got)
	}
	if got := gotURL.Load(); got != srv.URL+"/000000000000/user-audit-q-dlq|ApproximateNumberOfMessages" {
		t.Errorf("request = %v", got)
	}

	reply.Store(`{"Attributes":{}}`) // attribute missing → parse error, not a silent 0
	if _, err := d.Depth(ctx, srv.URL+"/q"); err == nil {
		t.Error("missing attribute must be an error")
	}
	reply.Store(`{"__type":"com.amazonaws.sqs#QueueDoesNotExist","message":"no queue"}`)
	status.Store(http.StatusBadRequest)
	if _, err := d.Depth(ctx, srv.URL+"/q"); err == nil {
		t.Error("an SQS error must be returned")
	}
}
