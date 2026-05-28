package store

import (
	"context"
	"testing"
	"time"
)

func TestMemoryFeedbackStore_RoundTrip(t *testing.T) {
	s := NewMemoryFeedbackStore(time.Hour)
	if err := s.SetTraceID(context.Background(), "ts-1", "trace-abc"); err != nil {
		t.Fatalf("SetTraceID: %v", err)
	}
	got, err := s.GetTraceID(context.Background(), "ts-1")
	if err != nil {
		t.Fatalf("GetTraceID: %v", err)
	}
	if got != "trace-abc" {
		t.Fatalf("got %q; want trace-abc", got)
	}
}

func TestMemoryFeedbackStore_MissingKey_ReturnsEmpty(t *testing.T) {
	s := NewMemoryFeedbackStore(time.Hour)
	got, err := s.GetTraceID(context.Background(), "no-such-key")
	if err != nil {
		t.Fatalf("GetTraceID: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q; want empty string", got)
	}
}

func TestMemoryFeedbackStore_EmptyTraceID_Deletes(t *testing.T) {
	s := NewMemoryFeedbackStore(time.Hour)
	_ = s.SetTraceID(context.Background(), "ts-1", "trace-abc")
	_ = s.SetTraceID(context.Background(), "ts-1", "")
	got, _ := s.GetTraceID(context.Background(), "ts-1")
	if got != "" {
		t.Fatalf("empty SetTraceID should delete; got %q", got)
	}
}

func TestMemoryFeedbackStore_ExpiredEntry_GCed(t *testing.T) {
	s := NewMemoryFeedbackStore(10 * time.Millisecond)
	_ = s.SetTraceID(context.Background(), "ts-1", "trace-abc")
	if s.Size() != 1 {
		t.Fatalf("after Set, Size = %d; want 1", s.Size())
	}
	time.Sleep(20 * time.Millisecond)
	got, _ := s.GetTraceID(context.Background(), "ts-1")
	if got != "" {
		t.Fatalf("expired Get should return empty; got %q", got)
	}
	if s.Size() != 0 {
		t.Fatalf("expired entry should be evicted on read; Size = %d", s.Size())
	}
}

func TestMemoryFeedbackStore_DefaultTTL_WhenZero(t *testing.T) {
	s := NewMemoryFeedbackStore(0)
	if s.ttl != DefaultFeedbackTTL {
		t.Fatalf("ttl=0 should fall back to DefaultFeedbackTTL; got %v", s.ttl)
	}
}

func TestMemoryFeedbackStore_EmptyKey_NoOp(t *testing.T) {
	s := NewMemoryFeedbackStore(time.Hour)
	if err := s.SetTraceID(context.Background(), "", "trace"); err != nil {
		t.Fatalf("SetTraceID with empty key: %v", err)
	}
	if s.Size() != 0 {
		t.Fatalf("empty key should not be stored; Size = %d", s.Size())
	}
}
