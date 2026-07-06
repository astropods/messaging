package web

import (
	"strings"
	"sync"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// turnTracker records per-conversation streaming turn state so the web adapter
// can (a) reconstruct the partial assistant text a client has seen so far and
// (b) enforce a client-initiated stop.
//
// Because the agent's model call runs in the agent process (not the sidecar),
// the sidecar cannot halt generation on its own. Instead, when a turn is
// stopped it drops the agent's subsequent chunks: they are neither streamed to
// the browser nor persisted, so a non-cooperating agent's late/full reply can't
// overwrite the partial the user actually saw. State is keyed by conversation
// and reset when a new user message starts a fresh turn.
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

// begin resets state for a new turn on conversationID (called when the user
// sends a message), clearing any prior stopped flag and partial buffer.
func (t *turnTracker) begin(conversationID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.partial[conversationID] = &strings.Builder{}
	delete(t.stopped, conversationID)
}

// record folds a streamed content chunk into the partial buffer, mirroring the
// client's reconstruction: REPLACE resets the buffer, everything else appends.
func (t *turnTracker) record(conversationID string, chunk *pb.ContentChunk) {
	if chunk == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.partial[conversationID]
	if b == nil {
		b = &strings.Builder{}
		t.partial[conversationID] = b
	}
	if chunk.Type == pb.ContentChunk_REPLACE {
		b.Reset()
	}
	b.WriteString(chunk.Content)
}

// stop marks the current turn stopped and returns the partial text accumulated
// so far. isStopped reports true until the next begin.
func (t *turnTracker) stop(conversationID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped[conversationID] = true
	if b := t.partial[conversationID]; b != nil {
		return b.String()
	}
	return ""
}

// isStopped reports whether the current turn on conversationID was stopped.
func (t *turnTracker) isStopped(conversationID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopped[conversationID]
}

// clear drops all state for a conversation's turn (called when a turn ends
// normally so the maps don't grow unbounded).
func (t *turnTracker) clear(conversationID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.partial, conversationID)
	delete(t.stopped, conversationID)
}
