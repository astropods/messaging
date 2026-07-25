// Package logctx carries W3C trace correlation through request-scoped logging.
package logctx

import (
	"context"
	"encoding/hex"
	"log/slog"
	"strings"
)

type traceIDKey struct{}

// WithTraceparent returns a context carrying the trace ID from a valid W3C
// traceparent value. Invalid or absent values leave the context unchanged.
func WithTraceparent(ctx context.Context, traceparent string) context.Context {
	traceID, ok := TraceIDFromTraceparent(traceparent)
	if !ok {
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

// TraceIDFromTraceparent extracts and validates the trace ID portion of a W3C
// traceparent header.
func TraceIDFromTraceparent(traceparent string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(traceparent), "-")
	if len(parts) != 4 ||
		strings.EqualFold(parts[0], "ff") ||
		!validHex(parts[0], 2, true) ||
		!validHex(parts[1], 32, false) ||
		!validHex(parts[2], 16, false) ||
		!validHex(parts[3], 2, true) {
		return "", false
	}
	return strings.ToLower(parts[1]), true
}

func validHex(value string, length int, allowZero bool) bool {
	if len(value) != length {
		return false
	}
	if _, err := hex.DecodeString(value); err != nil {
		return false
	}
	return allowZero || strings.Trim(value, "0") != ""
}
