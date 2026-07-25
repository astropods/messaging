package traceutil

import "testing"

func TestIDFromTraceparent(t *testing.T) {
	tests := []struct {
		name        string
		traceparent string
		want        string
	}{
		{
			name:        "valid",
			traceparent: "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01",
			want:        "4bf92f3577b34da6a3ce929d0e0e4736",
		},
		{
			name:        "future version with additional fields",
			traceparent: "01-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01-vendor-data",
			want:        "4bf92f3577b34da6a3ce929d0e0e4736",
		},
		{name: "empty"},
		{name: "malformed", traceparent: "not-a-traceparent"},
		{name: "invalid version length", traceparent: "000-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		{name: "invalid version hex", traceparent: "zz-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		{name: "forbidden ff version", traceparent: "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		{name: "invalid trace ID", traceparent: "00-not-a-trace-id-00f067aa0ba902b7-01"},
		{
			name:        "zero trace",
			traceparent: "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IDFromTraceparent(tt.traceparent); got != tt.want {
				t.Fatalf("IDFromTraceparent() = %q, want %q", got, tt.want)
			}
		})
	}
}
