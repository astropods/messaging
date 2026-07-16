package traceutil

import (
	"encoding/hex"
	"strings"
)

const zeroTraceID = "00000000000000000000000000000000"

// IDFromTraceparent extracts the trace ID from a W3C traceparent, including
// future-version values with additional fields. Invalid and zero IDs are
// omitted so platform metadata only exposes searchable traces.
func IDFromTraceparent(traceparent string) string {
	parts := strings.Split(strings.TrimSpace(traceparent), "-")
	if len(parts) < 4 || len(parts[0]) != 2 || strings.EqualFold(parts[0], "ff") {
		return ""
	}
	if _, err := hex.DecodeString(parts[0]); err != nil {
		return ""
	}
	if len(parts[1]) != 32 || parts[1] == zeroTraceID {
		return ""
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return ""
	}
	return strings.ToLower(parts[1])
}
