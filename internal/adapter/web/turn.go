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
// partial assistant text seen so far, the last time it was progressively
// persisted (used to throttle mid-stream chat-store writes), and the idle
// watchdog that reaps the turn if the agent goes silent.
type turnState struct {
	partial      strings.Builder
	lastPersist  time.Time
	idleTimer    *time.Timer
	idleDeadline time.Time
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
	mu          sync.Mutex
	turns       map[string]*turnState
	stopped     map[string]bool
	idleTimeout time.Duration
	onIdle      func(conversationID string)
}

func newTurnTracker() *turnTracker {
	return &turnTracker{
		turns:   make(map[string]*turnState),
		stopped: make(map[string]bool),
	}
}

// setIdleReaper enables the idle watchdog: a tracked turn with no activity for
// timeout invokes onIdle. Zero timeout leaves it disabled.
func (t *turnTracker) setIdleReaper(timeout time.Duration, onIdle func(conversationID string)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.idleTimeout = timeout
	t.onIdle = onIdle
}

// armIdleLocked (re)starts a turn's idle watchdog. Caller holds mu. It records a
// deadline and (re)schedules a single reused timer; the callback re-checks the
// deadline so activity landing in the window between the timer firing and its
// callback acquiring the lock reschedules the reap rather than ending a turn that
// just showed life.
func (t *turnTracker) armIdleLocked(conversationID string, st *turnState) {
	if t.idleTimeout <= 0 || t.onIdle == nil {
		return
	}
	st.idleDeadline = time.Now().Add(t.idleTimeout)
	if st.idleTimer == nil {
		st.idleTimer = time.AfterFunc(t.idleTimeout, func() { t.fireIdle(conversationID) })
		return
	}
	st.idleTimer.Reset(t.idleTimeout)
}

// fireIdle is the idle timer's callback. It reaps the turn only once the deadline
// has actually elapsed; if activity extended the deadline after the timer fired,
// it reschedules for the time remaining instead of reaping.
func (t *turnTracker) fireIdle(conversationID string) {
	t.mu.Lock()
	st := t.turns[conversationID]
	if st == nil || t.stopped[conversationID] {
		t.mu.Unlock()
		return
	}
	if remaining := time.Until(st.idleDeadline); remaining > 0 {
		if st.idleTimer != nil {
			st.idleTimer.Reset(remaining)
		}
		t.mu.Unlock()
		return
	}
	onIdle := t.onIdle
	t.mu.Unlock()
	if onIdle != nil {
		onIdle(conversationID)
	}
}

func stopIdleLocked(st *turnState) {
	if st != nil && st.idleTimer != nil {
		st.idleTimer.Stop()
	}
}

// startTurn marks a turn in flight when the user's message is forwarded, arming
// the idle watchdog so an agent that hangs before its first chunk is still reaped.
func (t *turnTracker) startTurn(conversationID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.turns[conversationID]
	if st == nil {
		if len(t.turns) >= maxTrackedStops {
			for k, old := range t.turns {
				stopIdleLocked(old)
				delete(t.turns, k)
				break
			}
		}
		st = &turnState{}
		t.turns[conversationID] = st
	}
	t.armIdleLocked(conversationID, st)
}

// touch resets the idle watchdog on any agent activity for a tracked, non-stopped
// turn. No-op otherwise.
func (t *turnTracker) touch(conversationID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped[conversationID] {
		return
	}
	if st := t.turns[conversationID]; st != nil {
		t.armIdleLocked(conversationID, st)
	}
}

// failActive atomically ends a tracked turn for abnormal termination (idle
// timeout or agent disconnect), returning the buffered partial. ok=false when no
// turn is active or it was user-stopped, so a terminal event fires exactly once.
func (t *turnTracker) failActive(conversationID string) (partial string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.turns[conversationID]
	if st == nil || t.stopped[conversationID] {
		return "", false
	}
	stopIdleLocked(st)
	partial = st.partial.String()
	delete(t.turns, conversationID)
	return partial, true
}

// activeConversations lists conversations with a turn in flight (not stopped).
func (t *turnTracker) activeConversations() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	ids := make([]string, 0, len(t.turns))
	for id := range t.turns {
		if !t.stopped[id] {
			ids = append(ids, id)
		}
	}
	return ids
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
			for k, old := range t.turns {
				stopIdleLocked(old)
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
	t.armIdleLocked(conversationID, st)
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

// isStreaming reports whether a turn is actively streaming for conversationID.
// Read handlers combine it with the persisted state to report assistant_streaming.
// A stopped conversation is excluded even though its turnState lingers (stop()
// keeps it to gate trailing chunks): otherwise a cancelled turn whose agent
// honored the abort (no END to clear the entry) would report streaming forever.
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
		stopIdleLocked(st)
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
	if st := t.turns[conversationID]; st != nil {
		stopIdleLocked(st)
	}
	delete(t.turns, conversationID)
	delete(t.stopped, conversationID)
}
