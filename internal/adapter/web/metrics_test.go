package web

import (
	"testing"
	"time"

	"github.com/astropods/messaging/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newMetricsConn(id, convID string) *SSEConnection {
	return &SSEConnection{
		ID:             id,
		ConversationID: convID,
		EventChan:      make(chan SSEEvent, 10),
		Done:           make(chan struct{}),
		CreatedAt:      time.Now(),
	}
}

func TestMetrics_WebConnections_Add(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)

	before := testutil.ToFloat64(metrics.WebActiveConnections)
	cm.Add(newMetricsConn("c1", "conv-m1"))

	if got := testutil.ToFloat64(metrics.WebActiveConnections) - before; got != 1 {
		t.Errorf("WebActiveConnections after Add: expected +1, got +%v", got)
	}

	cm.Remove("conv-m1", "c1")
}

func TestMetrics_WebConnections_Remove(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	cm.Add(newMetricsConn("c2", "conv-m2"))

	before := testutil.ToFloat64(metrics.WebActiveConnections)
	cm.Remove("conv-m2", "c2")

	if got := testutil.ToFloat64(metrics.WebActiveConnections) - before; got != -1 {
		t.Errorf("WebActiveConnections after Remove: expected -1, got %v", got)
	}
}

func TestMetrics_WebConnections_CloseAll(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	cm.Add(newMetricsConn("c3", "conv-m3"))
	cm.Add(newMetricsConn("c4", "conv-m3"))
	cm.Add(newMetricsConn("c5", "conv-m4"))

	cm.CloseAll()

	if got := testutil.ToFloat64(metrics.WebActiveConnections); got != 0 {
		t.Errorf("WebActiveConnections after CloseAll: expected 0, got %v", got)
	}
}
