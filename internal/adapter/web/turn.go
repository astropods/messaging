package web

import "sync"

// turnTracker records which conversations have a stopped in-flight turn so the
// web adapter can drop the agent's remaining output after a client "stop
// generating".
//
// The agent's model call runs in the agent process, so the sidecar can't halt
// generation itself. Dropping the agent's late chunks keeps a non-cooperating
// agent's continued output from reaching the client — and the astro-server chat
// store that tees the stream — after the user has stopped. State is keyed by
// conversation and reset when the next user message begins a new turn.
type turnTracker struct {
	mu      sync.Mutex
	stopped map[string]bool
}

func newTurnTracker() *turnTracker {
	return &turnTracker{stopped: make(map[string]bool)}
}

// begin resets state for a new turn on conversationID (called when the user
// sends a message), clearing any prior stopped flag.
func (t *turnTracker) begin(conversationID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.stopped, conversationID)
}

// stop marks the current turn on conversationID stopped.
func (t *turnTracker) stop(conversationID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped[conversationID] = true
}

// isStopped reports whether the current turn on conversationID was stopped.
func (t *turnTracker) isStopped(conversationID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopped[conversationID]
}

// clear drops state for a conversation's turn (called when a turn ends normally
// so the map doesn't grow unbounded).
func (t *turnTracker) clear(conversationID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.stopped, conversationID)
}
