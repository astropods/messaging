// Package logctx carries W3C trace correlation through request-scoped logging.
package logctx

import (
	"context"
	"log/slog"

	"github.com/astropods/messaging/internal/traceutil"
)

type traceIDKey struct{}

// WithTraceparent returns a context carrying the trace ID from a valid W3C
// traceparent value. Invalid or absent values leave the context unchanged.
func WithTraceparent(ctx context.Context, traceparent string) context.Context {
	traceID := traceutil.IDFromTraceparent(traceparent)
	if traceID == "" {
		return ctx
	}
	return context.WithValue(ctx, traceIDKey{}, traceID)
}

// FromContext returns the default logger enriched with trace_id when the
// context carries valid trace correlation.
func FromContext(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if traceID, ok := ctx.Value(traceIDKey{}).(string); ok && traceID != "" {
			return slog.Default().With("trace_id", traceID)
		}
	}
	return slog.Default()
}
