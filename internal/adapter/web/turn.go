package web

import (
	"strings"
	"sync"
	"time"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// maxTrackedStops bounds the turnTracker's stopped and turns maps. Entries are
// normally short-lived — cleared when a turn ends (a terminal chunk), when a new
// turn starts (a START chunk), or on stop. This cap is a safety valve for the
// degenerate cases where a turn neither terminates nor is resumed and the
// conversation is abandoned (a stopped-but-never-resumed turn, or a stream that
// dies mid-turn), so neither map can grow without bound over a long-lived process.
const maxTrackedStops = 4096

// turnState is the per-conversation streaming state for one in-flight turn: the
// partial assistant text seen so far and the last time it was progressively
// persisted (used to throttle mid-stream chat-store writes).
type turnState struct {
	partial     strings.Builder
	lastPersist time.Time
}

// turnTracker records per-conversation streaming turn state so the web adapter
// can (a) reconstruct the partial assistant text a client has seen so far — to
// persist progressively and on a stop — and (b) gate a stopped conversation's
// output.
//
// The agent's model call runs in the agent process, so the sidecar can't halt
// generation — a non-cooperating agent keeps producing chunks after a stop.
// Once a turn is stopped, the tracker drops that conversation's chunks so the
// late/complete output neither reaches the client nor is persisted to the
// deployment-local chat store.
//
// A stopped conversation stays gated until the agent starts a NEW turn, signalled
// by a START content chunk (the web/text and audio paths always emit one at the
// head of a turn — see messaging-bridge). Lifting the gate only on START — rather
// than on the next user send — prevents the stopped turn's trailing output from
// bleeding into the following message on the same conversation. (With an agent
// that runs turns concurrently rather than serially, straggler chunks emitted
// after the new turn's START can still slip through; fully halting generation
// requires agent/SDK cooperation.)
type turnTracker struct {
	mu      sync.Mutex
	turns   map[string]*turnState
	stopped map[string]bool
}

func newTurnTracker() *turnTracker {
	return &turnTracker{
		turns:   make(map[string]*turnState),
		stopped: make(map[string]bool),
	}
}

// record folds a streamed content chunk into the partial buffer, mirroring the
// client's reconstruction: START (a fresh turn) and REPLACE reset the buffer,
// everything else appends. This buffered text is what a mid-stream stop persists.
func (t *turnTracker) record(conversationID string, chunk *pb.ContentChunk) {
	if chunk == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.turns[conversationID]
	if st == nil {
		// Safety valve, mirroring stop()'s bound on the stopped map: a turn that
		// streams some content but never reaches END/error and is never stopped
		// (e.g. the agent crashes mid-stream) leaves its state resident, so cap
		// the map so those can't leak without bound over a long-lived process.
		if len(t.turns) >= maxTrackedStops {
			for k := range t.turns {
				delete(t.turns, k)
				break
			}
		}
		st = &turnState{}
		t.turns[conversationID] = st
	}
	if chunk.Type == pb.ContentChunk_START || chunk.Type == pb.ContentChunk_REPLACE {
		st.partial.Reset()
	}
	st.partial.WriteString(chunk.Content)
}

// content returns the text buffered for a conversation's in-flight turn — the
// full assistant reply streamed so far — without mutating turn state. Used to
// persist a turn's reply, since agents stream the reply as delta chunks and
// typically send an empty END, so a single chunk's content is not the full reply.
func (t *turnTracker) content(conversationID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if st := t.turns[conversationID]; st != nil {
		return st.partial.String()
	}
	return ""
}

// dueForPersist reports whether enough time has elapsed since this conversation's
// last progressive persist (or it has never persisted this turn), recording now
// when it returns true. Throttles mid-stream chat-store writes to roughly one per
// interval instead of one per token. Returns false when no turn is being tracked.
func (t *turnTracker) dueForPersist(conversationID string, interval time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.turns[conversationID]
	if st == nil {
		return false
	}
	now := time.Now()
	if st.lastPersist.IsZero() || now.Sub(st.lastPersist) >= interval {
		st.lastPersist = now
		return true
	}
	return false
}

// isStreaming reports whether a turn is actively streaming for conversationID —
// i.e. at least one assistant chunk has been recorded and the turn has not yet
// ended (clear) or been stopped. The read handlers use this to report
// assistant_streaming: because the assistant row is now persisted progressively,
// "the latest persisted message is the user's" no longer distinguishes an
// in-flight turn from a finished one.
//
// A stopped conversation is NOT streaming even though its turnState lingers:
// stop() keeps the entry so the gate can drop the agent's trailing chunks, but
// from the client's perspective the turn is done. Without excluding stopped
// here, a cancelled turn whose agent honored the abort (so no END chunk ever
// arrives to clear the entry) would report assistant_streaming forever, and a
// reload would reopen the finished turn.
func (t *turnTracker) isStreaming(conversationID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.turns[conversationID] != nil && !t.stopped[conversationID]
}

// stop marks the in-flight turn on conversationID stopped and returns the partial
// text buffered so far (what the user saw), for persistence.
func (t *turnTracker) stop(conversationID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Safety valve: if the map has grown past the cap (a leak of stopped-then-
	// abandoned conversations that never terminated or resumed), evict an
	// arbitrary entry before adding. Normal usage stays far below the cap
	// because entries are cleared on the turn's terminal chunk or next START.
	if _, exists := t.stopped[conversationID]; !exists && len(t.stopped) >= maxTrackedStops {
		for k := range t.stopped {
			delete(t.stopped, k)
			break
		}
	}
	t.stopped[conversationID] = true
	if st := t.turns[conversationID]; st != nil {
		return st.partial.String()
	}
	return ""
}

// gateContent reports whether a streamed content chunk should be dropped. A
// stopped conversation drops every chunk until a START arrives, which marks a
// fresh turn and lifts the gate. Returns true to drop the chunk.
func (t *turnTracker) gateContent(conversationID string, isStart bool) (drop bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.stopped[conversationID] {
		return false
	}
	if isStart {
		delete(t.stopped, conversationID)
		return false
	}
	return true
}

// clear drops all state for a conversation's turn (called when a turn ends
// normally so the maps don't grow unbounded).
func (t *turnTracker) clear(conversationID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.turns, conversationID)
	delete(t.stopped, conversationID)
}
