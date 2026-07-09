package feedbacklog

import (
	"context"
	"errors"
	"net/http"
	"testing"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type failingHTTPClient struct {
	calls int
}

func (c *failingHTTPClient) Do(req *http.Request) (*http.Response, error) {
	c.calls++
	return nil, errors.New("unexpected HTTP call")
}

func TestBuildRequestThumbsUp(t *testing.T) {
	req, ok := buildRequest(&pb.PlatformFeedback{
		ConversationId: "C123-1700000000.000001",
		ResponseId:     "1700000000.000002",
		TraceContext: &pb.TraceContext{
			Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		},
		Timestamp: timestamppb.Now(),
		User:      &pb.User{Id: "U123", Username: "alice"},
		Feedback: &pb.PlatformFeedback_Reaction{
			Reaction: &pb.MessageReaction{
				Type:  pb.MessageReaction_THUMBS_UP,
				Added: true,
			},
		},
	})
	if !ok {
		t.Fatal("expected request to be built")
	}
	if req.Source != "slack" {
		t.Fatalf("source: got %q", req.Source)
	}
	if req.TraceContext.Traceparent == "" {
		t.Fatal("expected traceparent")
	}
	if req.Feedback.Kind != "thumbs_up" {
		t.Fatalf("feedback: got %+v", req.Feedback)
	}
	if req.User.ID != "U123" || req.User.Username != "alice" {
		t.Fatalf("user: got %+v", req.User)
	}
}

func TestBuildRequestComment(t *testing.T) {
	req, ok := buildRequest(&pb.PlatformFeedback{
		ConversationId: "C123-1700000000.000001",
		ResponseId:     "1700000000.000002",
		TraceContext: &pb.TraceContext{
			Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			Tracestate:  "vendor=value",
		},
		User: &pb.User{Id: "U123", Username: "alice"},
		Feedback: &pb.PlatformFeedback_Text{
			Text: &pb.TextFeedback{
				Text:   "  useful but incomplete  ",
				Prompt: "What did you think of this reply?",
			},
		},
	})
	if !ok {
		t.Fatal("expected request to be built")
	}
	if req.TraceContext.Traceparent == "" {
		t.Fatal("expected traceparent")
	}
	if req.TraceContext.Tracestate != "vendor=value" {
		t.Fatalf("tracestate: got %q", req.TraceContext.Tracestate)
	}
	if req.Feedback.Kind != "comment" {
		t.Fatalf("feedback kind: got %q", req.Feedback.Kind)
	}
	if req.Feedback.Text != "useful but incomplete" {
		t.Fatalf("feedback text: got %q", req.Feedback.Text)
	}
}

func TestRecordSkipsHTTPWhenTraceContextMissing(t *testing.T) {
	httpClient := &failingHTTPClient{}
	client := &Client{
		httpClient: httpClient,
		serverURL:  "https://astro.example",
		token:      "token",
	}

	err := client.Record(context.Background(), &pb.PlatformFeedback{
		Feedback: &pb.PlatformFeedback_Reaction{
			Reaction: &pb.MessageReaction{Type: pb.MessageReaction_THUMBS_UP},
		},
	})
	if err != nil {
		t.Fatalf("Record returned error: %v", err)
	}
	if httpClient.calls != 0 {
		t.Fatalf("expected no HTTP calls, got %d", httpClient.calls)
	}
}

func TestBuildRequestSkipsMissingTraceContext(t *testing.T) {
	_, ok := buildRequest(&pb.PlatformFeedback{
		Feedback: &pb.PlatformFeedback_Reaction{
			Reaction: &pb.MessageReaction{Type: pb.MessageReaction_THUMBS_UP},
		},
	})
	if ok {
		t.Fatal("expected missing trace context to be skipped")
	}
}

func TestBuildRequestSkipsMissingResponseID(t *testing.T) {
	_, ok := buildRequest(&pb.PlatformFeedback{
		TraceContext: &pb.TraceContext{
			Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		},
		User: &pb.User{Id: "U123"},
		Feedback: &pb.PlatformFeedback_Reaction{
			Reaction: &pb.MessageReaction{Type: pb.MessageReaction_THUMBS_UP},
		},
	})
	if ok {
		t.Fatal("expected missing response id to be skipped")
	}
}

func TestBuildRequestSkipsMissingUser(t *testing.T) {
	_, ok := buildRequest(&pb.PlatformFeedback{
		ResponseId: "1700000000.000002",
		TraceContext: &pb.TraceContext{
			Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		},
		Feedback: &pb.PlatformFeedback_Reaction{
			Reaction: &pb.MessageReaction{Type: pb.MessageReaction_THUMBS_UP},
		},
	})
	if ok {
		t.Fatal("expected missing user to be skipped")
	}
}
