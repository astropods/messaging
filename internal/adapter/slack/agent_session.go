package slack

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/astropods/messaging/internal/metrics"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// maxStoppedTurns bounds stoppedTurns for agents that never start another turn.
const maxStoppedTurns = 1000

// handleAgentSessionStopped handles a press of Slack's Stop button on an agent session.
func (a *SlackAdapter) handleAgentSessionStopped(ctx context.Context, ev *slackevents.AgentSessionStoppedEvent) {
	metrics.SlackEvents.WithLabelValues("session_stopped").Inc()
	conversationID := a.stoppedConversation(ev.Channel, ev.ThreadTimestamp)
	a.stopTurn(conversationID)
	slog.Debug("[Slack] Agent session stopped",
		"conversation_id", conversationID, "user_id", ev.User)

	// Slack leaves the session processing, with its Stop button, until the app moves it.
	if a.client != nil {
		if _, err := a.client.SetAgentSessionStatusContext(ctx, slack.AgentSessionSetStatusParameters{
			Status:    slack.AgentSessionStatusActive,
			ChannelID: ev.Channel,
			ThreadTS:  ev.ThreadTimestamp,
		}); err != nil {
			slog.Warn("[Slack] Could not end the stopped agent session",
				"channel_id", ev.Channel, "thread_ts", ev.ThreadTimestamp, "err", err)
		}
	}

	a.forwardFeedback(ctx, &pb.PlatformFeedback{
		ConversationId: conversationID,
		Timestamp:      timestamppb.Now(),
		User:           &pb.User{Id: ev.User},
		Feedback: &pb.PlatformFeedback_StreamControl{
			StreamControl: &pb.StreamControl{
				Action: pb.StreamControl_STOP,
				Reason: "user stopped generation",
			},
		},
	})
}

// A top-level DM conversation is keyed by the channel, but its session by the message.
func (a *SlackAdapter) stoppedConversation(channelID, threadTS string) string {
	if threadTS == "" {
		return channelID
	}
	threaded := fmt.Sprintf("%s-%s", channelID, threadTS)
	if !isDMChannel(channelID) {
		return threaded
	}
	a.bufferMu.Lock()
	defer a.bufferMu.Unlock()
	_, threadBusy := a.contentBuffers[threaded]
	_, dmBusy := a.contentBuffers[channelID]
	if dmBusy && !threadBusy {
		return channelID
	}
	return threaded
}

func (a *SlackAdapter) stopTurn(conversationID string) {
	a.bufferMu.Lock()
	defer a.bufferMu.Unlock()
	delete(a.contentBuffers, conversationID)
	delete(a.traceBuffers, conversationID)
	if a.stoppedTurns == nil {
		a.stoppedTurns = make(map[string]struct{})
	}
	if _, ok := a.stoppedTurns[conversationID]; !ok && len(a.stoppedTurns) >= maxStoppedTurns {
		for id := range a.stoppedTurns {
			delete(a.stoppedTurns, id)
			break
		}
	}
	a.stoppedTurns[conversationID] = struct{}{}
}

func (a *SlackAdapter) dropStopped(response *pb.AgentResponse) bool {
	a.bufferMu.Lock()
	defer a.bufferMu.Unlock()
	id := response.GetConversationId()
	if _, ok := a.stoppedTurns[id]; !ok {
		return false
	}
	switch payload := response.Payload.(type) {
	case *pb.AgentResponse_Status:
		return true
	case *pb.AgentResponse_Content:
		switch payload.Content.GetType() {
		case pb.ContentChunk_START:
			delete(a.stoppedTurns, id)
			return false
		case pb.ContentChunk_END:
			delete(a.stoppedTurns, id)
		}
		return true
	case *pb.AgentResponse_Error:
		delete(a.stoppedTurns, id)
		return true
	}
	return false
}
