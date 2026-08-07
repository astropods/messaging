package web

import (
	"fmt"
	"strconv"
	"testing"
	"time"

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

// The per-conversation turn-state map is bounded like stopped so a stream that
// never reaches END/error and is never stopped (e.g. an agent that crashes
// mid-turn) can't leak state without bound.
func TestTurnTrackerPartialMapBounded(t *testing.T) {
	tr := newTurnTracker()
	for i := 0; i < maxTrackedStops+50; i++ {
		tr.record("conv-"+strconv.Itoa(i), contentChunk(pb.ContentChunk_DELTA, "x"))
	}

	tr.mu.Lock()
	n := len(tr.turns)
	tr.mu.Unlock()
	if n > maxTrackedStops {
		t.Fatalf("turn-state map size %d exceeds cap %d", n, maxTrackedStops)
	}
}

// isStreaming reflects an in-flight turn: true once a chunk is recorded, false
// before any chunk, after the turn is cleared (END/error/stopped-END), and — key
// for the cancel path — as soon as the turn is stopped, even though stop() keeps
// the turnState around for gating.
func TestTurnTrackerIsStreaming(t *testing.T) {
	tr := newTurnTracker()
	if tr.isStreaming("c") {
		t.Fatal("no turn recorded yet: isStreaming must be false")
	}
	tr.record("c", contentChunk(pb.ContentChunk_START, "hi"))
	if !tr.isStreaming("c") {
		t.Fatal("after a chunk: isStreaming must be true")
	}
	tr.clear("c")
	if tr.isStreaming("c") {
		t.Fatal("after clear: isStreaming must be false")
	}
}

// A stopped turn must report not-streaming immediately: stop() keeps the
// turnState (so the gate can still drop the agent's trailing chunks) but the
// turn is done from the client's perspective. If isStreaming stayed true, a
// cancelled turn whose agent honored the abort (no END chunk to clear it) would
// report assistant_streaming forever and reopen on reload.
func TestTurnTrackerIsStreamingFalseAfterStop(t *testing.T) {
	tr := newTurnTracker()
	tr.record("c", contentChunk(pb.ContentChunk_START, "partial"))
	if !tr.isStreaming("c") {
		t.Fatal("precondition: streaming before stop")
	}
	tr.stop("c")
	if tr.isStreaming("c") {
		t.Fatal("after stop: isStreaming must be false even though turnState lingers")
	}
}

// dueForPersist throttles progressive persistence: the first check for a turn
// fires (nothing persisted yet), an immediate re-check is throttled, and after
// the interval elapses it fires again. clear() resets the turn.
func TestTurnTrackerDueForPersist(t *testing.T) {
	tr := newTurnTracker()
	tr.record("c", contentChunk(pb.ContentChunk_START, "hi"))

	if !tr.dueForPersist("c", 50*time.Millisecond) {
		t.Fatal("first persist check should fire (never persisted this turn)")
	}
	if tr.dueForPersist("c", 50*time.Millisecond) {
		t.Fatal("immediate re-check should be throttled")
	}
	time.Sleep(60 * time.Millisecond)
	if !tr.dueForPersist("c", 50*time.Millisecond) {
		t.Fatal("after the interval the check should fire again")
	}

	// Untracked conversation never fires.
	if tr.dueForPersist("never-seen", time.Millisecond) {
		t.Fatal("dueForPersist must be false for an untracked conversation")
	}
}

// The idle reaper fires when a started turn produces no activity within the
// window, and activity (touch/record) resets it.
func TestTurnTrackerIdleReaper(t *testing.T) {
	fired := make(chan string, 1)
	tr := newTurnTracker()
	tr.setIdleReaper(60*time.Millisecond, func(conv string) { fired <- conv })

	tr.startTurn("c")
	// Keep it alive with activity below the window.
	for i := 0; i < 5; i++ {
		time.Sleep(20 * time.Millisecond)
		tr.touch("c")
	}
	select {
	case <-fired:
		t.Fatal("reaper fired while activity kept resetting it")
	default:
	}

	// Go quiet: the reaper must fire.
	select {
	case conv := <-fired:
		if conv != "c" {
			t.Fatalf("reaper fired for %q, want %q", conv, "c")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("reaper did not fire after the idle window elapsed")
	}
}

// clear() stops the idle timer so a completed turn is never reaped.
func TestTurnTrackerIdleReaperStoppedByClear(t *testing.T) {
	fired := make(chan string, 1)
	tr := newTurnTracker()
	tr.setIdleReaper(40*time.Millisecond, func(conv string) { fired <- conv })

	tr.startTurn("c")
	tr.clear("c")
	select {
	case <-fired:
		t.Fatal("reaper fired after clear")
	case <-time.After(120 * time.Millisecond):
	}
}

// failActive returns the buffered partial once, then reports no active turn; a
// user-stopped turn is never claimed (the stop path already finalized it).
func TestTurnTrackerFailActive(t *testing.T) {
	tr := newTurnTracker()
	tr.record("c", contentChunk(pb.ContentChunk_START, "half "))
	tr.record("c", contentChunk(pb.ContentChunk_DELTA, "done"))

	_, partial, ok := tr.failActive("c")
	if !ok || partial != "half done" {
		t.Fatalf("failActive = (%q, %v), want (%q, true)", partial, ok, "half done")
	}
	if _, _, ok := tr.failActive("c"); ok {
		t.Fatal("second failActive must report no active turn")
	}

	tr.record("c2", contentChunk(pb.ContentChunk_START, "x"))
	tr.stop("c2")
	if _, _, ok := tr.failActive("c2"); ok {
		t.Fatal("failActive must not claim a user-stopped turn")
	}
}

// failActive hands back a queued "write your own reply" so an abnormal end (reap /
// disconnect) can deliver it as a follow-up turn rather than dropping it.
func TestTurnTracker_FailActiveReturnsPending(t *testing.T) {
	tr := newTurnTracker()
	tr.startTurn("c")
	tr.enterAwaiting("c")
	tr.setPendingRespond("c", "alice", "Tuesday at 2pm")

	pending, _, ok := tr.failActive("c")
	if !ok || pending == nil || pending.text != "Tuesday at 2pm" || pending.userID != "alice" {
		t.Fatalf("failActive pending = %+v (ok=%v), want the queued respond", pending, ok)
	}
}

// After an abnormal reap, a slow-but-alive agent's trailing output must be gated
// (dropped) rather than resurrecting the finalized turn; a genuine new START
// lifts the gate. Matches the user-stop gate.
func TestTurnTrackerFailActiveGatesLateOutput(t *testing.T) {
	tr := newTurnTracker()
	tr.record("c", contentChunk(pb.ContentChunk_START, "working"))
	if _, _, ok := tr.failActive("c"); !ok {
		t.Fatal("failActive should claim the active turn")
	}
	// A late continuation chunk (not START) is dropped, so record() never runs.
	if drop := tr.gateContent("c", false); !drop {
		t.Fatal("late non-START output after a reap must be gated (dropped)")
	}
	if tr.isStreaming("c") {
		t.Fatal("a reaped turn must not be resurrected by late output")
	}
	// A genuine new turn (START) lifts the gate.
	if drop := tr.gateContent("c", true); drop {
		t.Fatal("a new START must lift the gate")
	}
}

// A turn started after a lingering user-stop drop-gate must still be reap-eligible:
// the gate blocks the previous turn's trailing output, but the fresh turn's idle
// watchdog and disconnect finalization must be able to end it. Regression for the
// hang where a stop the agent honored by going silent left the gate resident and
// disabled both safety nets for the next turn.
func TestTurnTracker_FreshTurnReapableDespiteStopGate(t *testing.T) {
	tr := newTurnTracker()
	// A turn the user stops; the agent honors it by going silent (no END), so the
	// drop-gate lingers.
	tr.record("c", contentChunk(pb.ContentChunk_START, "partial"))
	tr.stop("c")
	if _, _, ok := tr.failActive("c"); ok {
		t.Fatal("a user-stopped turn must not be reaped")
	}

	// The user sends again — a fresh turn is in flight while the gate still lingers.
	tr.startTurn("c")
	if !tr.isStreaming("c") {
		t.Fatal("a fresh turn after a stop gate should report streaming")
	}
	if got := tr.activeConversations(); len(got) != 1 || got[0] != "c" {
		t.Fatalf("fresh turn should be listed active for disconnect reaping, got %v", got)
	}
	// The fresh turn hangs — the idle reaper / disconnect must be able to claim it.
	if _, _, ok := tr.failActive("c"); !ok {
		t.Fatal("a fresh turn after a stop gate must be reapable")
	}
	// The gate still drops the previous turn's trailing output until a new START.
	if drop := tr.gateContent("c", false); !drop {
		t.Fatal("stop gate should still drop trailing output until a START")
	}
}

// A stale END from a stopped turn's generation must not wipe a freshly-resent
// turn's tracking. clearStoppedTurn preserves the fresh (non-userStopped) turn so
// its watchdog and disconnect finalization still work if it then hangs.
func TestTurnTracker_GatedEndKeepsFreshResentTurn(t *testing.T) {
	tr := newTurnTracker()
	// Turn 1 is user-stopped; the agent hasn't sent its END yet, so the gate lingers.
	tr.record("c", contentChunk(pb.ContentChunk_START, "t1"))
	tr.stop("c")
	// The user resends: turn 2 is armed while the gate still lingers.
	tr.startTurn("c")
	// Turn 1's straggler END arrives, gated (not a START); the handler's gated-END
	// cleanup must preserve turn 2.
	if drop := tr.gateContent("c", false); !drop {
		t.Fatal("stopped turn's straggler must be gated")
	}
	tr.clearStoppedTurn("c")
	if !tr.isStreaming("c") {
		t.Fatal("fresh resent turn was wiped by the stale END cleanup")
	}
	if got := tr.activeConversations(); len(got) != 1 || got[0] != "c" {
		t.Fatalf("fresh turn should remain active, got %v", got)
	}
	if _, _, ok := tr.failActive("c"); !ok {
		t.Fatal("fresh turn must remain reapable after the stale END cleanup")
	}
}

// enterAwaiting suspends the idle reaper (the agent is legitimately blocked on
// the user, not stalled); resume re-arms it.
func TestTurnTracker_AwaitingSuspendsReaper(t *testing.T) {
	fired := make(chan string, 1)
	tr := newTurnTracker()
	tr.setIdleReaper(40*time.Millisecond, func(conv string) { fired <- conv })

	tr.startTurn("c")
	tr.enterAwaiting("c")
	select {
	case <-fired:
		t.Fatal("reaper fired while awaiting an interaction response")
	case <-time.After(120 * time.Millisecond):
	}

	tr.resume("c")
	select {
	case conv := <-fired:
		if conv != "c" {
			t.Fatalf("reaper fired for %q, want %q", conv, "c")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("reaper did not fire after resume re-armed it")
	}
}

// endTurn hands back a queued "write your own reply" and clears the turn; with
// nothing queued it returns nil.
func TestTurnTracker_EndTurnReturnsPending(t *testing.T) {
	tr := newTurnTracker()
	tr.startTurn("c")
	tr.enterAwaiting("c")
	tr.setPendingRespond("c", "alice", "Tuesday at 2pm")

	pending := tr.endTurn("c")
	if pending == nil || pending.userID != "alice" || pending.text != "Tuesday at 2pm" {
		t.Fatalf("endTurn pending: got %+v, want alice/Tuesday at 2pm", pending)
	}
	if tr.isStreaming("c") {
		t.Error("turn should be cleared after endTurn")
	}

	tr.startTurn("c")
	if p := tr.endTurn("c"); p != nil {
		t.Errorf("endTurn with no queued respond: got %+v, want nil", p)
	}
}
