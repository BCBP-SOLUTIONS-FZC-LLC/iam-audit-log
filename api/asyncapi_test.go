package api

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// AL-INV-10 / AL-EVT-1: the embedded contract declares only receive
// operations — one per inbound queue (LLD §7.1, §7.3) — and zero send.
func TestAsyncAPI_NoSendOperations_ALINV10(t *testing.T) {
	var spec struct {
		AsyncAPI string `yaml:"asyncapi"`
		Channels map[string]struct {
			Address string `yaml:"address"`
		} `yaml:"channels"`
		Operations map[string]struct {
			Action string `yaml:"action"`
		} `yaml:"operations"`
	}
	if err := yaml.Unmarshal(AsyncAPISpec, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.AsyncAPI != "3.0.0" {
		t.Fatalf("asyncapi version %q", spec.AsyncAPI)
	}
	for name, op := range spec.Operations {
		if op.Action != "receive" {
			t.Errorf("operation %s has action %q — only receive is allowed (AL-INV-10)", name, op.Action)
		}
	}
	want := []string{
		"auth-audit-q", "user-audit-q", "membership-audit-q", "tenant-audit-q",
		"delegation-audit-q", "serviceaccount-audit-q", "tender-audit-q",
		"billing-audit-q", "usage-audit-q", "wf-workflow-audit-q", "wf-template-audit-q",
	}
	got := map[string]bool{}
	for _, ch := range spec.Channels {
		got[ch.Address] = true
	}
	for _, q := range want {
		if !got[q] {
			t.Errorf("missing channel for %s", q)
		}
	}
	if len(spec.Channels) != len(want) || len(spec.Operations) != len(want) {
		t.Errorf("channels=%d operations=%d, want %d each", len(spec.Channels), len(spec.Operations), len(want))
	}
}
