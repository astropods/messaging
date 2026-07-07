package web

import (
	"strings"
	"sync"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// maxTrackedStops bounds the turnTracker's stopped and partial maps. Entries are
// normally short-lived — cleared when a turn ends (a terminal chunk), when a new
// turn starts (a START chunk), or on stop. This cap is a safety valve for the
// degenerate cases where a turn neither terminates nor is resumed and the
// conversation is abandoned (a stopped-but-never-resumed turn, or a stream that
// dies mid-turn), so neither map can grow without bound over a long-lived process.
const maxTrackedStops = 4096

// turnTracker records per-conversation streaming turn state so the web adapter
// can (a) reconstruct the partial assistant text a client has seen so far — to
// persist on a stop — and (b) gate a stopped conversation's output.
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
	partial map[string]*strings.Builder
	stopped map[string]bool
}

func newTurnTracker() *turnTracker {
	return &turnTracker{
		partial: make(map[string]*strings.Builder),
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
	b := t.partial[conversationID]
	if b == nil {
		// Safety valve, mirroring stop()'s bound on the stopped map: a turn that
		// streams some content but never reaches END/error and is never stopped
		// (e.g. the agent crashes mid-stream) leaves its buffer resident, so cap
		// the map so those can't leak without bound over a long-lived process.
		if len(t.partial) >= maxTrackedStops {
			for k := range t.partial {
				delete(t.partial, k)
				break
			}
		}
		b = &strings.Builder{}
		t.partial[conversationID] = b
	}
	if chunk.Type == pb.ContentChunk_START || chunk.Type == pb.ContentChunk_REPLACE {
		b.Reset()
	}
	b.WriteString(chunk.Content)
}

// content returns the text buffered for a conversation's in-flight turn — the
// full assistant reply streamed so far — without mutating turn state. Used to
// persist a completed turn on its END chunk, since agents stream the reply as
// delta chunks and typically send an empty END, so the END chunk's own content
// is not the full reply.
func (t *turnTracker) content(conversationID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if b := t.partial[conversationID]; b != nil {
		return b.String()
	}
	return ""
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
	if b := t.partial[conversationID]; b != nil {
		return b.String()
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
	delete(t.partial, conversationID)
	delete(t.stopped, conversationID)
}
