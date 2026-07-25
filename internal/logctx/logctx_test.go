package logctx

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestTraceIDFromTraceparent(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	got, ok := TraceIDFromTraceparent(
		"00-" + traceID + "-00f067aa0ba902b7-01",
	)
	if !ok || got != traceID {
		t.Fatalf("TraceIDFromTraceparent() = %q, %v; want %q, true", got, ok, traceID)
	}
}

func TestTraceIDFromTraceparentRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{
		"",
		"not-a-traceparent",
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
	} {
		if got, ok := TraceIDFromTraceparent(value); ok {
			t.Errorf("TraceIDFromTraceparent(%q) = %q, true; want false", value, got)
		}
	}
}

func TestFromContextAddsStructuredTraceID(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	ctx := WithTraceparent(
		context.Background(),
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	)
	FromContext(ctx).Info("agent response")

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("decode log record: %v", err)
	}
	if got := record["trace_id"]; got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace_id = %v; want W3C trace ID", got)
	}
}
