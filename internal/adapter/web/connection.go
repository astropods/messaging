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
	// maxBufferedEventsPerConv bounds the per-conversation resume buffer. A turn
	// is a few hundred delta chunks at most; keeping the newest this-many lets a
	// client that reconnects within a turn replay everything it missed. Older
	// events fall out — a reconnect whose Last-Event-ID predates the window gets
	// the buffer's tail (still includes the terminal finish, the only event whose
	// loss strands the UI).
	maxBufferedEventsPerConv = 512
	// maxBufferedConversations caps total resume buffers so an idle process can't
	// accumulate them without bound; the least-recently-updated is evicted first.
	maxBufferedConversations = 2048
)

// bufferedEvent is a broadcast event tagged with its per-conversation sequence
// number (the SSE `id:`), retained for replay on reconnect.
type bufferedEvent struct {
	seq   uint64
	event SSEEvent
}

// convEventBuffer is the resume log for one conversation: a monotonic sequence
// counter and a bounded, oldest-first ring of recent events.
type convEventBuffer struct {
	lastSeq uint64
	events  []bufferedEvent
	updated time.Time
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
	// eventBuffers holds the resume log per conversation. Guarded by mu together
	// with connections: Broadcast appends+fans out and AddWithResume
	// registers+snapshots under the same lock, so an event is never both replayed
	// from the buffer and delivered live to the same connection.
	eventBuffers map[string]*convEventBuffer
	mu           sync.RWMutex
	heartbeat    time.Duration
	stopChan     chan struct{}
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

// AddWithResume registers a connection and atomically snapshots the events it
// missed — those buffered with a sequence greater than lastEventID (0 replays
// the whole retained buffer). Registration and snapshot share mu with Broadcast,
// so any event either appears in the returned replay slice (buffered before this
// call) or is delivered live to conn.EventChan (broadcast after) — never both.
func (cm *ConnectionManager) AddWithResume(conn *SSEConnection, lastEventID uint64) []SSEEvent {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.connections[conn.ConversationID] == nil {
		cm.connections[conn.ConversationID] = make(map[string]*SSEConnection)
	}
	cm.connections[conn.ConversationID][conn.ID] = conn
	metrics.WebActiveConnections.Inc()

	buf := cm.eventBuffers[conn.ConversationID]
	if buf == nil {
		return nil
	}
	// A cursor beyond the buffer's current max sequence can't name a position
	// within this buffer: it predates an eviction+recreate (evictOldestBufferLocked
	// drops the buffer, and the next broadcast restarts the sequence at 1) or is
	// otherwise stale. Comparing seqs would then match nothing and suppress replay
	// of the current turn's events — including its terminal finish, re-stranding the
	// UI. Replay the whole retained ring instead: these events belong to a turn the
	// client has not seen (its cursor is from an earlier, evicted one), so there is
	// no double-delivery, and a retained finish still releases the client.
	if lastEventID > buf.lastSeq {
		missed := make([]SSEEvent, 0, len(buf.events))
		for _, be := range buf.events {
			missed = append(missed, be.event)
		}
		return missed
	}
	var missed []SSEEvent
	for _, be := range buf.events {
		if be.seq > lastEventID {
			missed = append(missed, be.event)
		}
	}
	return missed
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

// Broadcast tags an event with the next per-conversation sequence id, retains it
// in the resume buffer, then sends it to all connections for the conversation.
// The id lets a reconnecting client replay from its Last-Event-ID; buffering lets
// an event survive a window with zero connections (the finish broadcast during a
// reconnect gap that previously stranded the UI).
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

// bufferEventLocked assigns the event the next monotonic sequence number for the
// conversation (as its SSE id) and appends it to the bounded resume buffer,
// returning the id-tagged event to broadcast. Caller must hold mu.
func (cm *ConnectionManager) bufferEventLocked(conversationID string, event SSEEvent) SSEEvent {
	buf := cm.eventBuffers[conversationID]
	if buf == nil {
		if len(cm.eventBuffers) >= maxBufferedConversations {
			cm.evictOldestBufferLocked()
		}
		buf = &convEventBuffer{}
		cm.eventBuffers[conversationID] = buf
	}
	buf.lastSeq++
	event.ID = strconv.FormatUint(buf.lastSeq, 10)
	buf.events = append(buf.events, bufferedEvent{seq: buf.lastSeq, event: event})
	if len(buf.events) > maxBufferedEventsPerConv {
		buf.events = buf.events[len(buf.events)-maxBufferedEventsPerConv:]
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
		delete(cm.eventBuffers, oldestID)
	}
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

	// The turn is terminal here (a client "stop generating"): HandleCancel has
	// already broadcast the terminal finish, and no further events will follow.
	// Drop the resume buffer rather than hold its full delta ring resident until
	// LRU eviction — a late reconnect falls back to the store-derived terminal
	// replay. Done unconditionally: a stop can arrive with no live connection.
	delete(cm.eventBuffers, conversationID)

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
