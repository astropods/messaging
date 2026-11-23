package slack

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/astropods/messaging/internal/metrics"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// maxTrackedTurns bounds stoppedTurns and openSessions for turns that never finish.
const maxTrackedTurns = 1000

// handleAgentSessionStopped handles a press of Slack's Stop button on an agent session.
func (a *SlackAdapter) handleAgentSessionStopped(ctx context.Context, ev *slackevents.AgentSessionStoppedEvent) {
	metrics.SlackEvents.WithLabelValues("session_stopped").Inc()
	conversationID := a.stoppedConversation(ev.Channel, ev.ThreadTimestamp)
	slog.Debug("[Slack] Agent session stopped",
		"conversation_id", conversationID, "user_id", ev.User)
	if a.cluster != nil && !a.runsTurn(conversationID, ev.Channel, ev.ThreadTimestamp) {
		a.cluster.bus.Publish(busSessionStopped, conversationID, sessionStopMessage{
			Channel: ev.Channel, ThreadTS: ev.ThreadTimestamp, User: ev.User,
		})
	}
	a.applySessionStop(ctx, conversationID, ev.Channel, ev.ThreadTimestamp, ev.User)
}

// Slack shows the Stop button only while the session is processing.
func (a *SlackAdapter) openSession(ctx context.Context, channelID, threadTS string) {
	if !a.setSessionStatus(ctx, channelID, threadTS, slack.AgentSessionStatusProcessing) {
		return
	}
	a.bufferMu.Lock()
	defer a.bufferMu.Unlock()
	if a.openSessions == nil {
		a.openSessions = make(map[string]struct{})
	}
	addBounded(a.openSessions, fmt.Sprintf("%s-%s", channelID, threadTS), struct{}{})
}

func (a *SlackAdapter) endSession(ctx context.Context, channelID, threadTS string) {
	key := fmt.Sprintf("%s-%s", channelID, threadTS)
	a.bufferMu.Lock()
	_, open := a.openSessions[key]
	delete(a.openSessions, key)
	a.bufferMu.Unlock()
	if open {
		a.setSessionStatus(ctx, channelID, threadTS, slack.AgentSessionStatusActive)
	}
}

func (a *SlackAdapter) setSessionStatus(ctx context.Context, channelID, threadTS, status string) bool {
	if a.client == nil || threadTS == "" {
		return false
	}
	if _, err := a.client.SetAgentSessionStatusContext(ctx, slack.AgentSessionSetStatusParameters{
		Status:    status,
		ChannelID: channelID,
		ThreadTS:  threadTS,
	}); err != nil {
		slog.Debug("[Slack] Could not set agent session status",
			"channel_id", channelID, "thread_ts", threadTS, "status", status, "err", err)
		return false
	}
	return true
}

func addBounded[V any](m map[string]V, key string, value V) {
	if _, ok := m[key]; !ok && len(m) >= maxTrackedTurns {
		for k := range m {
			delete(m, k)
			break
		}
	}
	m[key] = value
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
		a.stoppedTurns = make(map[string]bool)
	}
	addBounded(a.stoppedTurns, conversationID, true)
}

// An agent may send the next turn's statuses before its START.
func (a *SlackAdapter) allowStatuses(conversationID string) {
	a.bufferMu.Lock()
	defer a.bufferMu.Unlock()
	if _, ok := a.stoppedTurns[conversationID]; ok {
		a.stoppedTurns[conversationID] = false
	}
}

func (a *SlackAdapter) turnStopped(conversationID string) bool {
	a.bufferMu.Lock()
	defer a.bufferMu.Unlock()
	_, stopped := a.stoppedTurns[conversationID]
	return stopped
}

func (a *SlackAdapter) statusesStopped(conversationID string) bool {
	a.bufferMu.Lock()
	defer a.bufferMu.Unlock()
	return a.stoppedTurns[conversationID]
}

func (a *SlackAdapter) dropStopped(response *pb.AgentResponse) bool {
	a.bufferMu.Lock()
	defer a.bufferMu.Unlock()
	id := response.GetConversationId()
	dropStatuses, ok := a.stoppedTurns[id]
	if !ok {
		return false
	}
	switch payload := response.Payload.(type) {
	case *pb.AgentResponse_Status:
		return dropStatuses
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
