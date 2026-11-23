package web

import (
	"strconv"
	"time"
)

const (
	busSSE         = "web.sse"
	busClose       = "web.close"
	busWatch       = "web.watch"
	busReplay      = "web.replay"
	busStop        = "web.stop"
	busStopped     = "web.stopped"
	busAnswer      = "web.answer"
	busTurnStarted = "web.turn_started"
)

// watchTTL outlasts two watch renewals, so one lost renewal keeps the relay.
const watchTTL = 3 * watchRenewInterval

const watchRenewInterval = 10 * time.Second

type relayFunc func(kind, conversationID string, data any)

type sseMessage struct {
	Events []SSEEvent `json:"events"`
}

type replayMessage struct {
	To     string     `json:"to"`
	Events []SSEEvent `json:"events"`
}

// SetRelay connects the manager to other sidecars. Call before Start.
func (cm *ConnectionManager) SetRelay(fn relayFunc) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.relay = fn
	cm.watchers = map[string]map[string]time.Time{}
}

// relayLocked publishes only while another sidecar watches the conversation.
func (cm *ConnectionManager) relayLocked(conversationID, kind string, data any) {
	if cm.relay == nil {
		return
	}
	pods := cm.watchers[conversationID]
	now := time.Now()
	live := false
	for pod, exp := range pods {
		if now.After(exp) {
			delete(pods, pod)
			continue
		}
		live = true
	}
	if len(pods) == 0 {
		delete(cm.watchers, conversationID)
	}
	if live {
		cm.relay(kind, conversationID, data)
	}
}

// Watch registers pod as watching conversationID. With replay, pod also gets the
// live turn's buffered events, queued ahead of any later relay and sent even when empty.
func (cm *ConnectionManager) Watch(conversationID, pod string, replay bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if cm.relay == nil {
		return
	}
	pods := cm.watchers[conversationID]
	if pods == nil {
		pods = map[string]time.Time{}
		cm.watchers[conversationID] = pods
	}
	pods[pod] = time.Now().Add(watchTTL)
	if !replay {
		return
	}
	var events []SSEEvent
	if buf := cm.eventBuffers[conversationID]; buf != nil && !buf.endedTurn {
		events = make([]SSEEvent, 0, len(buf.events))
		for _, be := range buf.events {
			events = append(events, be.event)
		}
	}
	cm.relay(busReplay, conversationID, replayMessage{To: pod, Events: events})
}

// Mirror applies events another sidecar broadcast, keeping that sidecar's event ids
// so a client's Last-Event-ID resumes on either sidecar. A replay replaces the
// local buffer, whose ids may come from this sidecar's own counter.
func (cm *ConnectionManager) Mirror(conversationID string, events []SSEEvent, replay bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if replay {
		delete(cm.eventBuffers, conversationID)
	}
	for _, ev := range events {
		seq, err := strconv.ParseUint(ev.ID, 10, 64)
		if err != nil || seq == 0 {
			continue
		}
		buf := cm.eventBuffers[conversationID]
		if buf == nil {
			if len(cm.eventBuffers) >= maxBufferedConversations {
				cm.evictOldestBufferLocked()
			}
			buf = &convEventBuffer{lastSeq: seq - 1}
			cm.eventBuffers[conversationID] = buf
		}
		if seq <= buf.lastSeq {
			continue
		}
		// A hole would let a resume replay around missing events, so restart the segment.
		// A live connection already rendered around the hole, so release it to reconcile from history.
		if seq > buf.lastSeq+1 {
			if !replay && len(cm.connections[conversationID]) > 0 {
				finish := NewFinishEvent("")
				finish.ID = strconv.FormatUint(seq-1, 10)
				cm.fanOutLocked(conversationID, finish)
				cm.closeLocked(conversationID)
				buf = &convEventBuffer{}
				cm.eventBuffers[conversationID] = buf
			}
			buf.events = buf.events[:0]
			buf.bytes = 0
			buf.endedTurn = false
			buf.lastSeq = seq - 1
		}
		cm.fanOutLocked(conversationID, cm.bufferEventLocked(conversationID, ev))
	}
}

// CloseMirrored closes the conversation's connections because its owner closed them.
func (cm *ConnectionManager) CloseMirrored(conversationID string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.closeLocked(conversationID)
}

// ConnectedConversations lists conversations with at least one local SSE connection.
func (cm *ConnectionManager) ConnectedConversations() []string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	ids := make([]string, 0, len(cm.connections))
	for id := range cm.connections {
		ids = append(ids, id)
	}
	return ids
}
