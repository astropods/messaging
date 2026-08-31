package grpc

import (
	"testing"
	"time"

	"github.com/astropods/messaging/internal/store"
)

// AgentConnected is the readiness signal a client gates its composer on, so it
// has to agree with the registry findStreamForConversation falls back to. If
// the two read different keys, readiness reports true while sends still 424.
func TestAgentConnected_AgreesWithTheStreamASendFallsBackTo(t *testing.T) {
	server := NewServer(":0", store.NewThreadHistoryStore(100, 50, time.Hour), store.NewMemoryStore(), nil)

	if server.AgentConnected() {
		t.Fatal("reported connected with no stream registered")
	}
	if server.findStreamForConversation("some-conversation") != nil {
		t.Fatal("found a fallback stream with none registered")
	}

	server.streamsMu.Lock()
	server.streams["agent-stream"] = &conversationStream{
		stream:         &captureStream{},
		conversationID: "agent-stream",
	}
	server.streamsMu.Unlock()

	if !server.AgentConnected() {
		t.Error("reported not connected after the shared stream registered")
	}
	if server.findStreamForConversation("some-conversation") == nil {
		t.Error("a send would not find the stream readiness reports")
	}
}
