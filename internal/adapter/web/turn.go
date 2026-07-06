package web

import "sync"

// maxTrackedStops bounds the turnTracker's stopped map. Entries are normally
// short-lived — cleared when the stopped generation ends (a terminal chunk) or
// when a new turn starts (a START chunk). This cap is a safety valve for the
// degenerate case where a stopped turn neither terminates nor is resumed and
// the conversation is abandoned, so the map can't grow without bound over a
// long-lived process.
const maxTrackedStops = 4096

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
}

// clear removes any gate state for conversationID. Called when the stopped
// generation reaches a terminal chunk, so the entry doesn't linger for a
// stopped-then-abandoned conversation.
func (t *turnTracker) clear(conversationID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.stopped, conversationID)
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
