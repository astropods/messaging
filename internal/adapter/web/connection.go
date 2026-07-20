package web

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/astropods/messaging/internal/metrics"
)

const (
	// Per-conversation resume buffer bounds (newest kept, oldest dropped).
	maxBufferedEventsPerConv = 512
	maxBufferedBytesPerConv  = 256 * 1024
	// maxBufferedConversations caps total buffers; least-recently-updated evicted first.
	maxBufferedConversations = 2048
)

// bufferedEvent is a broadcast event tagged with its per-conversation sequence
// number (the SSE `id:`), retained for replay on reconnect.
type bufferedEvent struct {
	seq   uint64
	event SSEEvent
}

// convEventBuffer is one conversation's resume log: a monotonic counter and a
// bounded ring, segmented by turn so a resume only ever replays the live turn.
type convEventBuffer struct {
	lastSeq uint64
	events  []bufferedEvent
	updated time.Time
	bytes   int // retained payload bytes, for the byte cap
	// endedTurn: last retained event was terminal; the next event drops the segment.
	endedTurn bool
	// turnStartSeq: seq of the current segment's first event. A cursor below it
	// predates the live turn (the client crossed a boundary) and gets a finish.
	turnStartSeq uint64
}

// SSEConnection represents an active SSE connection
type SSEConnection struct {
	ID             string
	ConversationID string
	EventChan      chan SSEEvent
	Done           chan struct{}
	CreatedAt      time.Time
	LastEventAt    time.Time
}

// ConnectionManager tracks active SSE connections by conversation ID and retains
// a bounded per-conversation resume buffer so a reconnecting client can replay
// events missed while disconnected.
type ConnectionManager struct {
	connections map[string]map[string]*SSEConnection // conversationID -> connID -> connection
	// eventBuffers is the per-conversation resume log, guarded by mu with
	// connections so Broadcast (append+fan-out) and AddWithResume (register+
	// snapshot) are atomic — an event is never both replayed and delivered live.
	eventBuffers map[string]*convEventBuffer
	// seqFloor is the high-water mark of every dropped buffer's lastSeq. A recreated
	// buffer resumes numbering above it, keeping per-conversation seqs monotonic
	// across eviction so a stale cursor never aliases a foreign turn's deltas.
	seqFloor  uint64
	mu        sync.RWMutex
	heartbeat time.Duration
	stopChan  chan struct{}
}

// NewConnectionManager creates a new connection manager
func NewConnectionManager(heartbeatInterval time.Duration) *ConnectionManager {
	cm := &ConnectionManager{
		connections:  make(map[string]map[string]*SSEConnection),
		eventBuffers: make(map[string]*convEventBuffer),
		heartbeat:    heartbeatInterval,
		stopChan:     make(chan struct{}),
	}
	return cm
}

// AddWithResume registers a connection and snapshots missed events (seq >
// lastEventID) atomically with Broadcast, so an event is either returned or
// delivered live, never both. caughtUp: cursor covers the latest event.
// crossedBoundary: cursor predates the current turn.
func (cm *ConnectionManager) AddWithResume(conn *SSEConnection, lastEventID uint64) (missed []SSEEvent, caughtUp, crossedBoundary bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.connections[conn.ConversationID] == nil {
		cm.connections[conn.ConversationID] = make(map[string]*SSEConnection)
	}
	cm.connections[conn.ConversationID][conn.ID] = conn
	metrics.WebActiveConnections.Inc()

	buf := cm.eventBuffers[conn.ConversationID]
	if buf == nil {
		return nil, false, false
	}
	// Cursor the buffer can't contiguously replay — before the segment (crossed a
	// boundary), past the max (an eviction+recreate reset the sequence to 1), or in a
	// hole the cap evicted mid-turn (next expected seq predates the oldest retained).
	// Release it to reconcile from history rather than deliver a gapped or foreign
	// delta stream.
	if lastEventID > 0 && (lastEventID < buf.turnStartSeq || lastEventID > buf.lastSeq ||
		(len(buf.events) > 0 && lastEventID+1 < buf.events[0].seq)) {
		return nil, false, true
	}
	for _, be := range buf.events {
		if be.seq > lastEventID {
			missed = append(missed, be.event)
		}
	}
	return missed, lastEventID == buf.lastSeq, false
}

// Start begins the heartbeat goroutine for connection liveness
func (cm *ConnectionManager) Start(ctx context.Context) {
	go cm.heartbeatLoop(ctx)
}

// Stop stops the connection manager
func (cm *ConnectionManager) Stop() {
	close(cm.stopChan)
}

// Add registers a new SSE connection
func (cm *ConnectionManager) Add(conn *SSEConnection) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.connections[conn.ConversationID] == nil {
		cm.connections[conn.ConversationID] = make(map[string]*SSEConnection)
	}
	cm.connections[conn.ConversationID][conn.ID] = conn
	metrics.WebActiveConnections.Inc()

	slog.Debug(fmt.Sprintf("[Web] SSE connection added: id=%s, conversation=%s", conn.ID, conn.ConversationID))
}

// Remove unregisters an SSE connection
func (cm *ConnectionManager) Remove(conversationID, connID string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if conns, ok := cm.connections[conversationID]; ok {
		if conn, exists := conns[connID]; exists {
			close(conn.Done)
			delete(conns, connID)
			metrics.WebActiveConnections.Dec()
			slog.Debug(fmt.Sprintf("[Web] SSE connection removed: id=%q, conversation=%q", connID, conversationID)) //nolint:gosec // G706 false positive: %q escapes control characters
		}
		if len(conns) == 0 {
			delete(cm.connections, conversationID)
		}
	}
}

// Broadcast tags an event with the next sequence id, retains it in the resume
// buffer, then fans it out to the conversation's connections. Buffering lets an
// event survive a window with zero connections (the lost-finish that stranded the UI).
func (cm *ConnectionManager) Broadcast(conversationID string, event SSEEvent) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	event = cm.bufferEventLocked(conversationID, event)

	conns, ok := cm.connections[conversationID]
	if !ok {
		return
	}

	for _, conn := range conns {
		select {
		case conn.EventChan <- event:
			conn.LastEventAt = time.Now()
		default:
			// Channel full, connection may be slow
			slog.Warn(fmt.Sprintf("[Web] Event channel full for connection %s", conn.ID))
		}
	}
}

// bufferEventLocked tags the event with the next sequence id and appends it to the
// buffer, segmenting by turn and bounding by count and bytes. Caller must hold mu.
func (cm *ConnectionManager) bufferEventLocked(conversationID string, event SSEEvent) SSEEvent {
	buf := cm.eventBuffers[conversationID]
	if buf == nil {
		if len(cm.eventBuffers) >= maxBufferedConversations {
			cm.evictOldestBufferLocked()
		}
		buf = &convEventBuffer{lastSeq: cm.seqFloor}
		cm.eventBuffers[conversationID] = buf
	}
	// A terminal event segments the turn: the next event starts a fresh segment so
	// a resume can't replay across a turn boundary. lastSeq keeps climbing.
	if buf.endedTurn {
		buf.events = buf.events[:0]
		buf.bytes = 0
		buf.endedTurn = false
	}
	buf.lastSeq++
	if len(buf.events) == 0 {
		buf.turnStartSeq = buf.lastSeq
	}
	event.ID = strconv.FormatUint(buf.lastSeq, 10)
	buf.events = append(buf.events, bufferedEvent{seq: buf.lastSeq, event: event})
	buf.bytes += len(event.Data)
	for len(buf.events) > 1 &&
		(len(buf.events) > maxBufferedEventsPerConv || buf.bytes > maxBufferedBytesPerConv) {
		buf.bytes -= len(buf.events[0].event.Data)
		buf.events = buf.events[1:]
	}
	if event.Event == EventFinish || event.Event == EventError {
		buf.endedTurn = true
	}
	buf.updated = time.Now()
	return event
}

// evictOldestBufferLocked drops the least-recently-updated conversation buffer to
// keep the total bounded. Caller must hold mu.
func (cm *ConnectionManager) evictOldestBufferLocked() {
	var oldestID string
	var oldest time.Time
	for id, buf := range cm.eventBuffers {
		if oldestID == "" || buf.updated.Before(oldest) {
			oldestID, oldest = id, buf.updated
		}
	}
	if oldestID != "" {
		cm.dropBufferLocked(oldestID)
	}
}

// dropBufferLocked removes a conversation's buffer, carrying its lastSeq into
// seqFloor so a later recreate keeps the sequence monotonic. Caller must hold mu.
func (cm *ConnectionManager) dropBufferLocked(conversationID string) {
	buf := cm.eventBuffers[conversationID]
	if buf == nil {
		return
	}
	if buf.lastSeq > cm.seqFloor {
		cm.seqFloor = buf.lastSeq
	}
	delete(cm.eventBuffers, conversationID)
}

// LatestSeq returns the highest buffered sequence id for a conversation (0 if
// none). Used to tag a synthetic finish so a reconnecting client's cursor advances
// past it rather than replaying the same stale id.
func (cm *ConnectionManager) LatestSeq(conversationID string) uint64 {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	if buf := cm.eventBuffers[conversationID]; buf != nil {
		return buf.lastSeq
	}
	return 0
}

// GetConnectionCount returns the number of active connections for a conversation
func (cm *ConnectionManager) GetConnectionCount(conversationID string) int {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	if conns, ok := cm.connections[conversationID]; ok {
		return len(conns)
	}
	return 0
}

// GetTotalConnections returns the total number of active connections
func (cm *ConnectionManager) GetTotalConnections() int {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	total := 0
	for _, conns := range cm.connections {
		total += len(conns)
	}
	return total
}

// heartbeatLoop sends periodic heartbeat events to all connections
func (cm *ConnectionManager) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(cm.heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-cm.stopChan:
			return
		case <-ticker.C:
			cm.sendHeartbeats()
		}
	}
}

// sendHeartbeats sends a heartbeat event to all connections
func (cm *ConnectionManager) sendHeartbeats() {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	heartbeatEvent := SSEEvent{
		Event: EventHeartbeat,
		Data:  `{"type":"heartbeat"}`,
	}

	for _, conns := range cm.connections {
		for _, conn := range conns {
			select {
			case conn.EventChan <- heartbeatEvent:
			default:
				// Skip if channel is full
			}
		}
	}
}

// CloseConversation closes all SSE connections for a single conversation. It is
// used on a client "stop generating" so the stream ends immediately for every
// reader — including the astro-server chat-store persister, whose detached tee
// keeps reading (and re-marking the turn active) until the upstream SSE closes.
// Closing here makes that tee unwind so a stopped turn leaves no lingering
// writer that could resurrect it on the next turn.
func (cm *ConnectionManager) CloseConversation(conversationID string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	// Stop is terminal — drop the resume buffer (a late reconnect falls back to the
	// store-derived finish). Unconditional: a stop can arrive with no live connection.
	cm.dropBufferLocked(conversationID)

	conns, ok := cm.connections[conversationID]
	if !ok {
		return
	}
	for connID, conn := range conns {
		close(conn.Done)
		delete(conns, connID)
		metrics.WebActiveConnections.Dec()
	}
	delete(cm.connections, conversationID)
	slog.Debug(fmt.Sprintf("[Web] SSE connections closed for conversation=%q", conversationID)) //nolint:gosec // G706 false positive: %q escapes control characters
}

// CloseAll closes all connections (for shutdown)
func (cm *ConnectionManager) CloseAll() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	for convID, conns := range cm.connections {
		for connID, conn := range conns {
			close(conn.Done)
			delete(conns, connID)
		}
		delete(cm.connections, convID)
	}
	metrics.WebActiveConnections.Set(0)

	slog.Info("[Web] All SSE connections closed")
}
