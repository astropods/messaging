package logctx

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestFromContextAddsStructuredTraceID(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	ctx := WithTraceparent(
		context.Background(),
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-vendor-data",
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
