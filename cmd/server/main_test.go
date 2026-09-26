package main

import (
	"strings"
	"testing"
)

type warnRec struct{ msgs []string }

func (w *warnRec) Warn(msg string, _ map[string]any) { w.msgs = append(w.msgs, msg) }

func TestRun_MissingConfigExits1(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("DATABASE_URL", "")
	if code := run(); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}

// AL-INV-10: publisher-side config is never read, only warned about.
func TestWarnForbiddenPublisherConfig_ALINV10(t *testing.T) {
	t.Setenv("SNS_TOPIC_ARN", "")
	t.Setenv("OUTBOX_DATABASE_URL", "")
	w := &warnRec{}
	warnForbiddenPublisherConfig(w)
	if len(w.msgs) != 0 {
		t.Fatalf("unexpected warnings: %v", w.msgs)
	}

	t.Setenv("SNS_TOPIC_ARN", "arn:aws:sns:ap-south-1:1:t")
	t.Setenv("OUTBOX_DATABASE_URL", "postgres://x")
	warnForbiddenPublisherConfig(w)
	if len(w.msgs) != 2 || !strings.Contains(w.msgs[0], "AL-INV-10") {
		t.Fatalf("warnings = %v", w.msgs)
	}
}

// LLD §4.2: audit_events.id is a UUIDv7 (time-ordered).
func TestNewUUIDv7(t *testing.T) {
	id, err := newUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 36 || id[14] != '7' {
		t.Fatalf("%q is not a UUIDv7", id)
	}
}
