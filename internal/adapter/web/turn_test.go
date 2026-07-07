package web

// Covers turnTracker: record() accumulates the streamed reply (so a completed
// turn persists the full text, not the empty END chunk) and START/REPLACE reset
// the buffer, and the partial map is bounded like stopped so an interrupted
// stream can't leak buffers.
//
//	go test ./internal/adapter/web -run TestTurnTracker -v

import (
	"strconv"
	"testing"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

func contentChunk(typ pb.ContentChunk_ChunkType, s string) *pb.ContentChunk {
	return &pb.ContentChunk{Type: typ, Content: s}
}

func TestTurnTrackerContentAccumulatesAndResets(t *testing.T) {
	tr := newTurnTracker()

	tr.record("c", contentChunk(pb.ContentChunk_START, "Hi"))
	tr.record("c", contentChunk(pb.ContentChunk_DELTA, " there"))
	tr.record("c", contentChunk(pb.ContentChunk_END, "")) // agents typically end empty
	if got := tr.content("c"); got != "Hi there" {
		t.Fatalf("content = %q, want %q (END must not blank the buffer)", got, "Hi there")
	}

	// START begins a fresh turn: the buffer resets.
	tr.record("c", contentChunk(pb.ContentChunk_START, "New"))
	if got := tr.content("c"); got != "New" {
		t.Fatalf("content after START = %q, want %q", got, "New")
	}

	// REPLACE also resets (full-snapshot chunk).
	tr.record("c", contentChunk(pb.ContentChunk_REPLACE, "Full"))
	if got := tr.content("c"); got != "Full" {
		t.Fatalf("content after REPLACE = %q, want %q", got, "Full")
	}

	// clear drops the buffer entirely.
	tr.clear("c")
	if got := tr.content("c"); got != "" {
		t.Fatalf("content after clear = %q, want empty", got)
	}
}

func TestTurnTrackerStopReturnsPartial(t *testing.T) {
	tr := newTurnTracker()
	tr.record("c", contentChunk(pb.ContentChunk_START, "half "))
	tr.record("c", contentChunk(pb.ContentChunk_DELTA, "written"))
	if got := tr.stop("c"); got != "half written" {
		t.Fatalf("stop() partial = %q, want %q", got, "half written")
	}
}

func TestTurnTrackerPartialMapBounded(t *testing.T) {
	tr := newTurnTracker()
	// Simulate many turns that stream but never reach END/error and are never
	// stopped, so clear() is never called for them.
	for i := 0; i < maxTrackedStops+50; i++ {
		tr.record("conv-"+strconv.Itoa(i), contentChunk(pb.ContentChunk_DELTA, "x"))
	}

	tr.mu.Lock()
	n := len(tr.partial)
	tr.mu.Unlock()
	if n > maxTrackedStops {
		t.Fatalf("partial map size %d exceeds cap %d", n, maxTrackedStops)
	}
}
