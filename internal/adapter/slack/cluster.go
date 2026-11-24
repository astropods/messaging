package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/astropods/messaging/internal/cluster"
	"github.com/astropods/messaging/internal/metrics"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/slack-go/slack"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const busSessionStopped = "slack.session_stopped"

const slackEventPruneInterval = 10 * time.Minute

// Bus is the part of cluster.Bus the Slack adapter uses.
type Bus interface {
	Publish(kind, conversationID string, data any)
	Handle(kind string, fn func(cluster.Envelope))
}

// EventClaimer records Slack event ids shared by every sidecar.
type EventClaimer interface {
	ClaimSlackEvent(ctx context.Context, eventID string) (bool, error)
	PruneSlackEvents(ctx context.Context) error
}

type sessionStopMessage struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
	User     string `json:"user"`
}

type clusterLink struct {
	bus     Bus
	claimer EventClaimer

	pruneMu   sync.Mutex
	lastPrune time.Time
}

// SetCluster shares event dedup and Stop presses with the other sidecars. Call before Start.
func (a *SlackAdapter) SetCluster(bus Bus, claimer EventClaimer) {
	if bus == nil || claimer == nil {
		return
	}
	a.cluster = &clusterLink{bus: bus, claimer: claimer}
	bus.Handle(busSessionStopped, func(env cluster.Envelope) {
		var m sessionStopMessage
		if err := json.Unmarshal(env.Data, &m); err != nil {
			slog.Warn("slack cluster: bad session stop dropped", "error", err)
			return
		}
		if !a.runsTurn(env.Conversation, m.Channel, m.ThreadTS) {
			return
		}
		go a.applySessionStop(context.Background(), env.Conversation, m.Channel, m.ThreadTS, m.User)
	})
}

// firstDelivery reports whether no sidecar has handled eventID yet. A failed claim reads as first.
func (a *SlackAdapter) firstDelivery(ctx context.Context, payload json.RawMessage) bool {
	if a.cluster == nil || len(payload) == 0 {
		return true
	}
	var ids struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(payload, &ids); err != nil || ids.EventID == "" {
		return true
	}
	a.cluster.maybePrune(ctx)
	first, err := a.cluster.claimer.ClaimSlackEvent(ctx, ids.EventID)
	if err != nil {
		slog.Warn("slack cluster: claim event failed", "event_id", ids.EventID, "error", err)
		return true
	}
	if !first {
		slog.Debug("slack cluster: duplicate event dropped", "event_id", ids.EventID)
		metrics.MessagesDropped.WithLabelValues("slack", "cluster_dedup").Inc()
	}
	return first
}

func (c *clusterLink) maybePrune(ctx context.Context) {
	c.pruneMu.Lock()
	due := time.Since(c.lastPrune) >= slackEventPruneInterval
	if due {
		c.lastPrune = time.Now()
	}
	c.pruneMu.Unlock()
	if !due {
		return
	}
	go func() {
		if err := c.claimer.PruneSlackEvents(context.WithoutCancel(ctx)); err != nil {
			slog.Warn("slack cluster: prune events failed", "error", err)
		}
	}()
}

// runsTurn reports whether this sidecar is streaming or holding the session for the thread.
func (a *SlackAdapter) runsTurn(conversationID, channelID, threadTS string) bool {
	a.bufferMu.Lock()
	defer a.bufferMu.Unlock()
	_, buffering := a.contentBuffers[conversationID]
	_, open := a.openSessions[fmt.Sprintf("%s-%s", channelID, threadTS)]
	return buffering || open
}

func (a *SlackAdapter) applySessionStop(ctx context.Context, conversationID, channelID, threadTS, userID string) {
	a.stopTurn(conversationID)
	a.forwardFeedback(ctx, &pb.PlatformFeedback{
		ConversationId: conversationID,
		Timestamp:      timestamppb.Now(),
		User:           &pb.User{Id: userID},
		Feedback: &pb.PlatformFeedback_StreamControl{
			StreamControl: &pb.StreamControl{
				Action: pb.StreamControl_STOP,
				Reason: "user stopped generation",
			},
		},
	})
	// Slack leaves the session processing, with its Stop button, until the app moves it.
	a.bufferMu.Lock()
	delete(a.openSessions, fmt.Sprintf("%s-%s", channelID, threadTS))
	a.bufferMu.Unlock()
	a.setSessionStatus(ctx, channelID, threadTS, slack.AgentSessionStatusActive)
}
