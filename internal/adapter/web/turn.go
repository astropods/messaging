package web

import "sync"

// turnTracker gates a conversation's streamed agent output around a client
// "stop generating".
//
// The agent's model call runs in the agent process, so the sidecar can't halt
// generation — a non-cooperating agent keeps producing chunks after a stop.
// Once a turn is stopped, the tracker drops that conversation's chunks so the
// late/complete output neither reaches the client nor the astro-server chat
// store stream tee.
//
// Crucially, a stopped conversation stays gated until the agent starts a NEW
// turn, signalled by a START content chunk (the web/text and audio paths always
// emit one at the head of a turn — see messaging-bridge). Lifting the gate only
// on START — rather than on the next user send — prevents the stopped turn's
// trailing output from bleeding into the following message on the same
// conversation. (With an agent that runs turns concurrently rather than
// serially, straggler chunks emitted after the new turn's START can still slip
// through; fully halting generation requires agent/SDK cooperation.)
type turnTracker struct {
	mu      sync.Mutex
	stopped map[string]bool
}

func newTurnTracker() *turnTracker {
	return &turnTracker{stopped: make(map[string]bool)}
}

// stop marks the in-flight turn on conversationID stopped.
func (t *turnTracker) stop(conversationID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped[conversationID] = true
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
