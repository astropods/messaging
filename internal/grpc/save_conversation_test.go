package grpc

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/store"
	"github.com/astropods/messaging/internal/store/sqlite"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func newSaveTestServer(t *testing.T) (*Server, *sqlite.Store) {
	t.Helper()
	cs, err := sqlite.Open(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatalf("open chat store: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	s := NewServer(":0", store.NewThreadHistoryStore(100, 50, time.Hour), store.NewMemoryStore(), nil)
	s.SetChatStore(cs)
	return s, cs
}

func saveReq(msgs ...*pb.SavedMessage) *pb.SaveConversationRequest {
	return &pb.SaveConversationRequest{
		UserId:         "user_1",
		IdempotencyKey: "slack:C1:111.0001",
		Title:          "Thread",
		SourceLabel:    "#eng",
		SourceUrl:      "https://slack/x",
		Messages:       msgs,
	}
}

func TestSaveConversation_WritesCopyAndReportsCreated(t *testing.T) {
	s, cs := newSaveTestServer(t)

	resp, err := s.SaveConversation(t.Context(), saveReq(
		&pb.SavedMessage{Role: "user", Author: "Ada", Content: "hello"},
		&pb.SavedMessage{Role: "assistant", Content: "hi"},
	))
	if err != nil {
		t.Fatalf("SaveConversation: %v", err)
	}
	if resp.Status != pb.SaveConversationResponse_CREATED {
		t.Fatalf("status = %v, want CREATED", resp.Status)
	}
	if want := sqlite.DeriveSavedConversationID("user_1", "slack:C1:111.0001"); resp.ConversationId != want {
		t.Fatalf("conversation id = %q, want the derived %q", resp.ConversationId, want)
	}

	msgs, err := cs.ListMessages(t.Context(), resp.ConversationId)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d (%v)", len(msgs), err)
	}
	if conv, _ := cs.Get(t.Context(), resp.ConversationId); conv == nil || conv.SourceLabel != "#eng" {
		t.Fatalf("source label not stored: %+v", conv)
	}
}

// The status is the whole point of making this unary: an agent that syncs on
// every source message has to learn the user replied in the copy, or it will
// keep calling and never understand why nothing changes.
func TestSaveConversation_ReportsDivergenceToTheAgent(t *testing.T) {
	s, cs := newSaveTestServer(t)

	first, err := s.SaveConversation(t.Context(), saveReq(
		&pb.SavedMessage{Role: "user", Content: "slack one"},
	))
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	if _, err := cs.AppendMessage(t.Context(), first.ConversationId, "user_1", "user", "my note", ""); err != nil {
		t.Fatalf("user turn: %v", err)
	}

	resp, err := s.SaveConversation(t.Context(), saveReq(
		&pb.SavedMessage{Role: "user", Content: "slack one"},
		&pb.SavedMessage{Role: "user", Content: "slack two"},
	))
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if resp.Status != pb.SaveConversationResponse_SKIPPED_DIVERGED {
		t.Fatalf("status = %v, want SKIPPED_DIVERGED", resp.Status)
	}

	msgs, _ := cs.ListMessages(t.Context(), first.ConversationId)
	if len(msgs) != 2 || msgs[1].Content != "my note" {
		t.Fatalf("the user's turn must survive, got %+v", msgs)
	}
}

func TestSaveConversation_AgentCanChooseAppendOrReplace(t *testing.T) {
	for _, tc := range []struct {
		mode pb.SaveConversationRequest_OnConflict
		want pb.SaveConversationResponse_Status
		msgs int
	}{
		{pb.SaveConversationRequest_APPEND, pb.SaveConversationResponse_APPENDED, 3},
		{pb.SaveConversationRequest_REPLACE, pb.SaveConversationResponse_REPLACED, 1},
	} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			s, cs := newSaveTestServer(t)
			first, _ := s.SaveConversation(t.Context(), saveReq(
				&pb.SavedMessage{Role: "user", Content: "slack one"}))
			if _, err := cs.AppendMessage(t.Context(), first.ConversationId, "user_1", "user", "my note", ""); err != nil {
				t.Fatalf("user turn: %v", err)
			}

			req := saveReq(&pb.SavedMessage{Role: "user", Content: "slack two"})
			req.OnConflict = tc.mode
			resp, err := s.SaveConversation(t.Context(), req)
			if err != nil {
				t.Fatalf("save: %v", err)
			}
			if resp.Status != tc.want {
				t.Fatalf("status = %v, want %v", resp.Status, tc.want)
			}
			if msgs, _ := cs.ListMessages(t.Context(), first.ConversationId); len(msgs) != tc.msgs {
				t.Fatalf("expected %d messages, got %d", tc.msgs, len(msgs))
			}
		})
	}
}

// The user id is what makes the copy reachable. A raw Slack id or an arbitrary
// string would write a conversation no session can ever open.
func TestSaveConversation_RejectsNonAstroUserID(t *testing.T) {
	s, _ := newSaveTestServer(t)

	for _, uid := range []string{"U07ABCDEF", "", "alice@example.com"} {
		req := saveReq(&pb.SavedMessage{Role: "user", Content: "x"})
		req.UserId = uid
		_, err := s.SaveConversation(t.Context(), req)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("user id %q: got %v, want InvalidArgument", uid, err)
		}
	}
}

func TestSaveConversation_RejectsUnknownRoleAndMissingKey(t *testing.T) {
	s, _ := newSaveTestServer(t)

	if _, err := s.SaveConversation(t.Context(), saveReq(
		&pb.SavedMessage{Role: "system", Content: "x"},
	)); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown role: got %v, want InvalidArgument", err)
	}

	req := saveReq(&pb.SavedMessage{Role: "user", Content: "x"})
	req.IdempotencyKey = ""
	if _, err := s.SaveConversation(t.Context(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty key: got %v, want InvalidArgument", err)
	}
}

// AsTime() on a nil timestamp is the unix epoch, which would date every
// timestamp-less turn to 1970 and sort the copy to the bottom of the sidebar.
func TestSaveConversation_MissingTimestampUsesNow(t *testing.T) {
	s, cs := newSaveTestServer(t)
	before := time.Now().Add(-time.Minute)

	resp, err := s.SaveConversation(t.Context(), saveReq(
		&pb.SavedMessage{Role: "user", Content: "no timestamp"},
		&pb.SavedMessage{Role: "assistant", Content: "has one",
			Timestamp: timestamppb.New(before.Add(time.Second))},
	))
	if err != nil {
		t.Fatalf("SaveConversation: %v", err)
	}

	conv, _ := cs.Get(t.Context(), resp.ConversationId)
	if conv == nil || conv.UpdatedAt.Before(before) {
		t.Fatalf("expected a current conversation timestamp, got %+v", conv)
	}
	if msgs, _ := cs.ListMessages(t.Context(), resp.ConversationId); len(msgs) != 2 {
		t.Fatalf("expected both messages stored, got %d", len(msgs))
	}
}

func TestSaveConversation_FailsClosedWithoutAStore(t *testing.T) {
	s := NewServer(":0", store.NewThreadHistoryStore(100, 50, time.Hour), store.NewMemoryStore(), nil)
	if _, err := s.SaveConversation(t.Context(), saveReq()); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("got %v, want FailedPrecondition", err)
	}
}
