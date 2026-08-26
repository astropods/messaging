package grpc

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/store"
	"github.com/astropods/messaging/internal/store/sqlite"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	_ "modernc.org/sqlite"
)

func newSaveTestServer(t *testing.T) (*Server, *sqlite.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat.db")
	cs, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("open chat store: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	s := NewServer(":0", store.NewThreadHistoryStore(100, 50, time.Hour), store.NewMemoryStore(), nil)
	s.SetChatStore(cs)
	return s, cs, path
}

func TestHandleSaveConversation_WritesCopy(t *testing.T) {
	s, cs, _ := newSaveTestServer(t)

	if err := s.handleSaveConversation(t.Context(), &pb.SaveConversation{
		UserId:         "user_1",
		IdempotencyKey: "slack:C1:111.0001",
		Title:          "Thread",
		SourceLabel:    "#eng",
		SourceUrl:      "https://slack/x",
		Messages: []*pb.SavedMessage{
			{Role: "user", Author: "Ada", Content: "hello"},
			{Role: "assistant", Content: "hi"},
		},
	}); err != nil {
		t.Fatalf("handleSaveConversation: %v", err)
	}

	id := sqlite.DeriveSavedConversationID("user_1", "slack:C1:111.0001")
	msgs, err := cs.ListMessages(t.Context(), id)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d (%v)", len(msgs), err)
	}
	conv, _ := cs.Get(t.Context(), id)
	if conv == nil || conv.SourceLabel != "#eng" {
		t.Fatalf("source label not stored: %+v", conv)
	}
}

// The user id is what makes the copy reachable. A raw Slack id or an arbitrary
// string would write a conversation no session can ever open.
func TestHandleSaveConversation_RejectsNonAstroUserID(t *testing.T) {
	s, cs, _ := newSaveTestServer(t)

	for _, uid := range []string{"U07ABCDEF", "", "alice@example.com"} {
		if err := s.handleSaveConversation(t.Context(), &pb.SaveConversation{
			UserId:         uid,
			IdempotencyKey: "k1",
			Messages:       []*pb.SavedMessage{{Role: "user", Content: "x"}},
		}); err != nil {
			t.Fatalf("handleSaveConversation(%q): %v", uid, err)
		}
		if conv, _ := cs.Get(t.Context(), sqlite.DeriveSavedConversationID(uid, "k1")); conv != nil {
			t.Fatalf("user id %q should have been rejected", uid)
		}
	}
}

func TestHandleSaveConversation_SkipsUnknownRoles(t *testing.T) {
	s, cs, _ := newSaveTestServer(t)

	if err := s.handleSaveConversation(t.Context(), &pb.SaveConversation{
		UserId:         "user_1",
		IdempotencyKey: "k1",
		Messages: []*pb.SavedMessage{
			{Role: "system", Content: "dropped"},
			{Role: "user", Content: "kept"},
		},
	}); err != nil {
		t.Fatalf("handleSaveConversation: %v", err)
	}

	msgs, _ := cs.ListMessages(t.Context(), sqlite.DeriveSavedConversationID("user_1", "k1"))
	if len(msgs) != 1 || msgs[0].Content != "kept" {
		t.Fatalf("expected only the known role stored, got %+v", msgs)
	}
}

// AsTime() on a nil timestamp is the unix epoch, which would date every
// timestamp-less turn to 1970 and sort the copy to the bottom of the sidebar.
func TestHandleSaveConversation_MissingTimestampUsesNow(t *testing.T) {
	s, cs, path := newSaveTestServer(t)
	before := time.Now().Add(-time.Minute)

	if err := s.handleSaveConversation(t.Context(), &pb.SaveConversation{
		UserId:         "user_1",
		IdempotencyKey: "k1",
		Messages: []*pb.SavedMessage{
			{Role: "user", Content: "no timestamp"},
			{Role: "assistant", Content: "has one", Timestamp: timestamppb.New(before.Add(time.Second))},
		},
	}); err != nil {
		t.Fatalf("handleSaveConversation: %v", err)
	}

	id := sqlite.DeriveSavedConversationID("user_1", "k1")
	rows, err := cs.ListMessages(t.Context(), id)
	if err != nil || len(rows) != 2 {
		t.Fatalf("expected 2 messages: %d %v", len(rows), err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var createdMs int64
	if err := db.QueryRowContext(t.Context(),
		`SELECT created_at FROM messages WHERE conversation_id = ? ORDER BY seq LIMIT 1`, id,
	).Scan(&createdMs); err != nil {
		t.Fatalf("read created_at: %v", err)
	}
	if createdMs < before.UnixMilli() {
		t.Fatalf("expected a current timestamp, got %d (%s)", createdMs, time.UnixMilli(createdMs))
	}
}

// A save must never take down the agent stream: every in-flight turn rides on it.
func TestHandleSaveConversation_NoStoreIsNotAnError(t *testing.T) {
	s := NewServer(":0", store.NewThreadHistoryStore(100, 50, time.Hour), store.NewMemoryStore(), nil)
	if err := s.handleSaveConversation(t.Context(), &pb.SaveConversation{
		UserId: "user_1", IdempotencyKey: "k1",
	}); err != nil {
		t.Fatalf("expected a no-op, got %v", err)
	}
	if err := s.handleSaveConversation(t.Context(), nil); err != nil {
		t.Fatalf("expected a no-op for nil, got %v", err)
	}
}

// The payload reaches the store through routeAgentResponse, the single funnel
// every agent payload passes. Without the branch there it would fall through to
// the adapter broadcast and the save would silently never happen.
func TestRouteAgentResponse_SaveConversationReachesTheStore(t *testing.T) {
	s, cs, _ := newSaveTestServer(t)

	if err := s.routeAgentResponse(t.Context(), &pb.AgentResponse{
		ConversationId: "slack-thread-not-a-chat-conversation",
		Payload: &pb.AgentResponse_SaveConversation{
			SaveConversation: &pb.SaveConversation{
				UserId:         "user_1",
				IdempotencyKey: "k1",
				Title:          "Thread",
				Messages:       []*pb.SavedMessage{{Role: "user", Content: "hello"}},
			},
		},
	}); err != nil {
		t.Fatalf("routeAgentResponse: %v", err)
	}

	conv, err := cs.Get(t.Context(), sqlite.DeriveSavedConversationID("user_1", "k1"))
	if err != nil || conv == nil {
		t.Fatalf("expected the copy stored, got %+v (%v)", conv, err)
	}
}
