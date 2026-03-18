package web

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/astropods/messaging/internal/metrics"
)

// SSEConnection represents an active SSE connection
type SSEConnection struct {
	ID             string
	ConversationID string
	EventChan      chan SSEEvent
	Done           chan struct{}
	CreatedAt      time.Time
	LastEventAt    time.Time
}

// ConnectionManager tracks active SSE connections by conversation ID
type ConnectionManager struct {
	connections map[string]map[string]*SSEConnection // conversationID -> connID -> connection
	mu          sync.RWMutex
	heartbeat   time.Duration
	stopChan    chan struct{}
}

// NewConnectionManager creates a new connection manager
func NewConnectionManager(heartbeatInterval time.Duration) *ConnectionManager {
	cm := &ConnectionManager{
		connections: make(map[string]map[string]*SSEConnection),
		heartbeat:   heartbeatInterval,
		stopChan:    make(chan struct{}),
	}
	return cm
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

	log.Printf("[Web] SSE connection added: id=%s, conversation=%s", conn.ID, conn.ConversationID)
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
			log.Printf("[Web] SSE connection removed: id=%q, conversation=%q", connID, conversationID) //nolint:gosec // G706 false positive: %q escapes control characters
		}
		if len(conns) == 0 {
			delete(cm.connections, conversationID)
		}
	}
}

// Broadcast sends an event to all connections for a conversation
func (cm *ConnectionManager) Broadcast(conversationID string, event SSEEvent) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

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
			log.Printf("[Web] Event channel full for connection %s", conn.ID)
		}
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

	log.Println("[Web] All SSE connections closed")
}
