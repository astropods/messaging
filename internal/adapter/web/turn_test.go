package web

import (
	"fmt"
	"testing"
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
