package web

import (
	"fmt"
	"strconv"
	"testing"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// Locks in the START-gate invariant: a stopped conversation drops every chunk
// until the agent begins a fresh turn (a START chunk), and the gate is NOT
// lifted by anything else. This is the load-bearing rule that keeps a stopped
// turn's trailing output from bleeding into the next message.
func TestTurnTracker_GateInvariant(t *testing.T) {
	tr := newTurnTracker()
	const conv = "c1"

	// Never stopped -> nothing is gated.
	if tr.gateContent(conv, false) {
		t.Fatalf("unstopped conversation must not gate a chunk")
	}
	if tr.gateContent(conv, true) {
		t.Fatalf("unstopped conversation must not gate a START")
	}

	// Stopped -> non-START chunks are dropped, repeatedly.
	tr.stop(conv)
	if !tr.gateContent(conv, false) {
		t.Fatalf("stopped conversation must drop a non-START chunk")
	}
	if !tr.gateContent(conv, false) {
		t.Fatalf("stopped conversation must keep dropping until a START")
	}

	// A START lifts the gate and is itself not dropped.
	if tr.gateContent(conv, true) {
		t.Fatalf("START must not be dropped and must lift the gate")
	}

	// After the START, subsequent non-START chunks flow again.
	if tr.gateContent(conv, false) {
		t.Fatalf("after START the gate must be lifted")
	}
}

// clear() lifts the gate directly (used on the stopped turn's terminal chunk so
// the entry doesn't linger for a stopped-then-abandoned conversation).
func TestTurnTracker_Clear(t *testing.T) {
	tr := newTurnTracker()
	const conv = "c1"

	tr.stop(conv)
	if !tr.gateContent(conv, false) {
		t.Fatalf("expected gate active after stop")
	}
	tr.clear(conv)
	if tr.gateContent(conv, false) {
		t.Fatalf("clear must lift the gate")
	}
	// clear on an unknown conversation is a no-op.
	tr.clear("never-seen")
}

// The stopped map is bounded so stopped-then-abandoned conversations can't grow
// it without limit over a long-lived process.
func TestTurnTracker_StoppedMapIsBounded(t *testing.T) {
	tr := newTurnTracker()
	for i := 0; i < maxTrackedStops+100; i++ {
		tr.stop(fmt.Sprintf("c-%d", i))
	}
	tr.mu.Lock()
	n := len(tr.stopped)
	tr.mu.Unlock()
	if n > maxTrackedStops {
		t.Fatalf("stopped map exceeded cap: got %d, want <= %d", n, maxTrackedStops)
	}
}

func contentChunk(typ pb.ContentChunk_ChunkType, s string) *pb.ContentChunk {
	return &pb.ContentChunk{Type: typ, Content: s}
}

// record() accumulates the streamed reply across a turn (so a completed turn
// persists the full text, not the empty END chunk), and START/REPLACE reset the
// buffer for a fresh turn / full-snapshot chunk.
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

// stop() returns the partial text buffered so far (what the user saw), which the
// cancel path persists.
func TestTurnTrackerStopReturnsPartial(t *testing.T) {
	tr := newTurnTracker()
	tr.record("c", contentChunk(pb.ContentChunk_START, "half "))
	tr.record("c", contentChunk(pb.ContentChunk_DELTA, "written"))
	if got := tr.stop("c"); got != "half written" {
		t.Fatalf("stop() partial = %q, want %q", got, "half written")
	}
}

// The partial map is bounded like stopped so a stream that never reaches
// END/error and is never stopped (e.g. an agent that crashes mid-turn) can't
// leak buffers without bound.
func TestTurnTrackerPartialMapBounded(t *testing.T) {
	tr := newTurnTracker()
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
