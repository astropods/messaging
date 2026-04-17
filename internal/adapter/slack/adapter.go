package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/metrics"
	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/google/uuid"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SlackAdapter implements the adapter.Adapter interface for Slack
type SlackAdapter struct {
	client       *slack.Client
	socketClient *socketmode.Client
	config       adapter.Config
	msgHandler   adapter.MessageHandler
	rateLimiter  *RateLimiter
	stopChan     chan struct{}
	aiClient     *SlackAIClient

	// contentBuffers accumulates DELTA chunks per conversation so the adapter
	// can send a single complete message to Slack on END.
	contentBuffers map[string]string

	// actionableReactions is the set of emoji names forwarded to the agent.
	// Built from config at initialization; empty map means no reactions are forwarded.
	actionableReactions map[string]bool
}

// New creates a new Slack adapter
func New() *SlackAdapter {
	return &SlackAdapter{
		stopChan:       make(chan struct{}),
		contentBuffers: make(map[string]string),
	}
}

// Initialize sets up the Slack adapter with configuration
func (a *SlackAdapter) Initialize(ctx context.Context, config adapter.Config) error {
	a.config = config

	// Initialize Slack client
	a.client = slack.New(
		config.BotToken,
		slack.OptionAppLevelToken(config.AppToken),
	)

	// Initialize socket mode client if enabled
	if config.SocketMode {
		a.socketClient = socketmode.New(
			a.client,
			socketmode.OptionDebug(false),
		)
	}

	// Initialize rate limiter
	a.rateLimiter = NewRateLimiter(
		config.RateLimit.RequestsPerSecond,
		config.RateLimit.BurstSize,
	)

	a.aiClient = NewSlackAIClient(config.BotToken, config.DevMode)

	a.actionableReactions = make(map[string]bool, len(config.ActionableReactions))
	for _, r := range config.ActionableReactions {
		a.actionableReactions[r] = true
	}

	slog.Info(fmt.Sprintf("[Slack] Adapter initialized (Socket Mode: %v, actionable reactions: %v, allowed channels: %v, allowed user IDs: %v)",
		config.SocketMode, config.ActionableReactions, config.AllowedChannelIDs, config.AllowedUserIDs))
	return nil
}

// Start begins listening for Slack events
func (a *SlackAdapter) Start(ctx context.Context) error {
	if a.config.SocketMode {
		return a.startSocketMode(ctx)
	}
	return fmt.Errorf("webhook mode not implemented, use Socket Mode")
}

// startSocketMode starts the socket mode event listener
func (a *SlackAdapter) startSocketMode(ctx context.Context) error {
	slog.Info("[Slack] Starting Socket Mode connection...")

	// Start socket mode client in background (this initializes the Events channel)
	go func() {
		if err := a.socketClient.RunContext(ctx); err != nil {
			slog.Error(fmt.Sprintf("[Slack] Socket mode client error: %v", err))
		}
	}()

	// Listen for events from the now-initialized channel
	for {
		select {
		case <-ctx.Done():
			slog.Info("[Slack] Context cancelled, stopping event listener")
			return ctx.Err()
		case <-a.stopChan:
			slog.Info("[Slack] Stopping event listener")
			return nil
		case evt := <-a.socketClient.Events:
			a.handleSocketEvent(ctx, evt)
		}
	}
}

// handleSocketEvent processes incoming socket mode events
func (a *SlackAdapter) handleSocketEvent(ctx context.Context, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeConnecting:
		slog.Info("[Slack] Connecting to Slack...")

	case socketmode.EventTypeConnectionError:
		slog.Error(fmt.Sprintf("[Slack] Connection error: %v", evt.Data))

	case socketmode.EventTypeConnected:
		slog.Info("[Slack] Connected to Slack via Socket Mode")

	case socketmode.EventTypeEventsAPI:
		// Acknowledge the event
		a.socketClient.Ack(*evt.Request)

		// Handle the inner event
		eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			slog.Info("[Slack] Could not type cast event to EventsAPIEvent")
			return
		}

		a.handleInnerEvent(ctx, eventsAPIEvent.InnerEvent)

	case socketmode.EventTypeInteractive:
		// Acknowledge interactive events (buttons, modals, etc.)
		a.socketClient.Ack(*evt.Request)

		// Handle block actions (feedback buttons, etc.)
		callback, ok := evt.Data.(slack.InteractionCallback)
		if ok && callback.Type == slack.InteractionTypeBlockActions {
			a.handleBlockActions(ctx, &callback)
		} else {
			slog.Info("[Slack] Interactive event received (not yet handled)")
		}

	case socketmode.EventTypeSlashCommand:
		// Acknowledge slash commands
		a.socketClient.Ack(*evt.Request)
		slog.Info("[Slack] Slash command received (not yet handled)")

	case socketmode.EventTypeHello:
		// Hello event is just a connection acknowledgment, no action needed

	default:
		// Only log truly unknown event types at debug level
		if evt.Type != "" {
			slog.Info(fmt.Sprintf("[Slack] Unhandled event type: %s", evt.Type))
		}
	}
}

// handleInnerEvent processes the actual event data
func (a *SlackAdapter) handleInnerEvent(ctx context.Context, innerEvent slackevents.EventsAPIInnerEvent) {
	switch ev := innerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		a.handleMessage(ctx, ev)

	case *slackevents.AppMentionEvent:
		a.handleAppMention(ctx, ev)

	case *slackevents.ReactionAddedEvent:
		a.handleReactionAdded(ctx, ev)

	default:
		slog.Info(fmt.Sprintf("[Slack] Unhandled inner event type: %s", innerEvent.Type))
	}
}

// handleMessage processes message events
func (a *SlackAdapter) handleMessage(ctx context.Context, ev *slackevents.MessageEvent) {
	// Filter out bot messages
	if ev.BotID != "" {
		metrics.MessagesDropped.WithLabelValues("slack", "bot_filtered").Inc()
		return
	}

	// Filter out message subtypes we don't want to process
	if ev.SubType != "" && ev.SubType != "thread_broadcast" {
		return
	}

	// In public/private channels, only process thread replies (follow-ups).
	// Top-level channel messages are handled via app_mention events to avoid duplicates.
	if ev.Channel != "" && ev.Channel[0] != 'D' {
		if ev.ThreadTimeStamp == "" {
			slog.Info(fmt.Sprintf("[Slack] Ignoring top-level message in channel %s (will handle via app_mention)", ev.Channel))
			return
		}
		slog.Info(fmt.Sprintf("[Slack] Processing thread reply in channel %s, thread=%s", ev.Channel, ev.ThreadTimeStamp))
	}

	// Allowlist: if configured, only allow messages from allowed channels or users
	if !a.isAllowed(ev.Channel, ev.User) {
		slog.Info(fmt.Sprintf("[Slack] Message from disallowed channel=%s or user=%s", ev.Channel, ev.User))
		metrics.MessagesDropped.WithLabelValues("slack", "allowlist").Inc()
		a.sendNotEnabledMessage(ctx, ev.Channel, ev.ThreadTimeStamp)
		return
	}

	eventType := "thread_reply"
	if ev.Channel != "" && ev.Channel[0] == 'D' {
		eventType = "dm"
	}
	metrics.SlackEvents.WithLabelValues(eventType).Inc()

	slog.Info(fmt.Sprintf("[Slack] Message received: channel=%s, user=%s, text=%s", ev.Channel, ev.User, ev.Text))

	// Build conversation ID
	conversationID := ev.Channel
	if ev.ThreadTimeStamp != "" {
		conversationID = fmt.Sprintf("%s-%s", ev.Channel, ev.ThreadTimeStamp)
	}

	// Convert to pb.Message
	msg := &pb.Message{
		Id:             uuid.NewString(),
		Timestamp:      timestamppb.New(parseSlackTimestamp(ev.TimeStamp)),
		Platform:       "slack",
		Content:        ev.Text,
		ConversationId: conversationID,
		PlatformContext: &pb.PlatformContext{
			MessageId: ev.TimeStamp,
			ChannelId: ev.Channel,
			ThreadId:  ev.ThreadTimeStamp,
		},
		User: &pb.User{
			Id: ev.User,
		},
	}

	// Call handler if registered
	if a.msgHandler != nil {
		if err := a.msgHandler(ctx, msg); err != nil {
			slog.Error(fmt.Sprintf("[Slack] Error handling message: %v", err))
			a.sendErrorMessage(ctx, ev.Channel, ev.ThreadTimeStamp, err)
		}
	}
}

// handleBlockActions processes block action events (button clicks, etc.)
func (a *SlackAdapter) handleBlockActions(ctx context.Context, callback *slack.InteractionCallback) {
	slog.Info(fmt.Sprintf("[Slack] Block action received: type=%s, actions=%d", callback.Type, len(callback.ActionCallback.BlockActions)))

	for _, action := range callback.ActionCallback.BlockActions {
		slog.Info(fmt.Sprintf("[Slack] Action: id=%s, value=%s", action.ActionID, action.Value))

		// Handle feedback button clicks (Slack AI context_actions/feedback_buttons)
		if action.ActionID == "feedback_buttons" {
			feedbackType := action.Value // "positive_feedback" or "negative_feedback"
			slog.Info(fmt.Sprintf("[Slack] Feedback received: %s from user %s on message %s",
				feedbackType, callback.User.ID, callback.Message.Timestamp))

			// Use Slack emoji names (not emoji characters)
			emojiName := "thumbsup"
			if feedbackType == "negative_feedback" {
				emojiName = "thumbsdown"
			}

			// Remove feedback buttons from the message first
			if len(callback.Message.Blocks.BlockSet) > 0 {
				updatedBlocks := []slack.Block{}
				for _, block := range callback.Message.Blocks.BlockSet {
					// Filter out the context_actions block (Slack AI feedback block type)
					if block.BlockType() == "context_actions" {
						continue
					}
					updatedBlocks = append(updatedBlocks, block)
				}

				_, _, _, err := a.client.UpdateMessage(
					callback.Channel.ID,
					callback.Message.Timestamp,
					slack.MsgOptionBlocks(updatedBlocks...),
					slack.MsgOptionText(callback.Message.Text, false),
				)

				if err != nil {
					slog.Error(fmt.Sprintf("[Slack] Failed to remove feedback buttons: %v", err))
				} else {
					slog.Info("[Slack] Feedback buttons removed from message")
				}
			}

			// Acknowledge the feedback visually by adding a reaction
			err := a.client.AddReaction(emojiName, slack.ItemRef{
				Channel:   callback.Channel.ID,
				Timestamp: callback.Message.Timestamp,
			})

			if err != nil {
				slog.Error(fmt.Sprintf("[Slack] Failed to add reaction: %v", err))
			} else {
				slog.Info(fmt.Sprintf("[Slack] Feedback acknowledged with :%s: reaction", emojiName))
			}
		}
	}
}

// handleAppMention processes app mention events
func (a *SlackAdapter) handleAppMention(ctx context.Context, ev *slackevents.AppMentionEvent) {
	slog.Info(fmt.Sprintf("[Slack] App mentioned: channel=%s, user=%s, text=%s", ev.Channel, ev.User, ev.Text))

	// Allowlist: if configured, only allow mentions from allowed channels or users
	if !a.isAllowed(ev.Channel, ev.User) {
		slog.Info(fmt.Sprintf("[Slack] App mention from disallowed channel=%s or user=%s", ev.Channel, ev.User))
		metrics.MessagesDropped.WithLabelValues("slack", "allowlist").Inc()
		threadID := ev.ThreadTimeStamp
		if threadID == "" {
			threadID = ev.TimeStamp
		}
		a.sendNotEnabledMessage(ctx, ev.Channel, threadID)
		return
	}

	metrics.SlackEvents.WithLabelValues("mention").Inc()

	// Use ThreadTimeStamp if already in a thread, otherwise use the message's
	// own TimeStamp so the response creates a new thread under the mention.
	threadID := ev.ThreadTimeStamp
	if threadID == "" {
		threadID = ev.TimeStamp
	}

	conversationID := fmt.Sprintf("%s-%s", ev.Channel, threadID)
	text := stripMentions(ev.Text)

	slog.Info(fmt.Sprintf("[Slack] Setting loading state: channel=%s, threadTS=%s", ev.Channel, threadID))
	if err := a.aiClient.SetThreadStatus(ctx, ev.Channel, threadID, "Assistant is thinking...", "thinking_face"); err != nil {
		slog.Error(fmt.Sprintf("[Slack] ERROR: Failed to set loading state: %v", err))
	}

	// Convert to pb.Message
	msg := &pb.Message{
		Id:             uuid.NewString(),
		Timestamp:      timestamppb.New(parseSlackTimestamp(ev.TimeStamp)),
		Platform:       "slack",
		Content:        text,
		ConversationId: conversationID,
		PlatformContext: &pb.PlatformContext{
			MessageId: ev.TimeStamp,
			ChannelId: ev.Channel,
			ThreadId:  threadID,
		},
		User: &pb.User{
			Id: ev.User,
		},
	}

	// Call handler if registered
	if a.msgHandler != nil {
		if err := a.msgHandler(ctx, msg); err != nil {
			slog.Error(fmt.Sprintf("[Slack] Error handling mention: %v", err))
			a.sendErrorMessage(ctx, ev.Channel, threadID, err)
			// Clear loading state on error
			_ = a.aiClient.SetThreadStatus(ctx, ev.Channel, threadID, "", "")
		}
	}
}

// handleReactionAdded processes reaction_added events. Only reactions in the
// configured actionableReactions set are forwarded to the agent. If the set
// is empty (no reactions configured), all reactions are dropped.
func (a *SlackAdapter) handleReactionAdded(ctx context.Context, ev *slackevents.ReactionAddedEvent) {
	slog.Info(fmt.Sprintf("[Slack] Reaction added: emoji=%s, user=%s, channel=%s, item_ts=%s",
		ev.Reaction, ev.User, ev.Item.Channel, ev.Item.Timestamp))

	if !a.actionableReactions[ev.Reaction] {
		slog.Info(fmt.Sprintf("[Slack] Ignoring non-actionable reaction :%s:", ev.Reaction))
		return
	}

	metrics.SlackEvents.WithLabelValues("reaction").Inc()

	originalText := a.fetchMessageText(ctx, ev.Item.Channel, ev.Item.Timestamp)
	if originalText == "" {
		slog.Info("[Slack] Could not fetch original message for reaction, skipping")
		return
	}

	threadID := ev.Item.Timestamp
	conversationID := fmt.Sprintf("%s-%s", ev.Item.Channel, threadID)

	content := fmt.Sprintf("[reaction :%s: added by <@%s> on message]\n%s",
		ev.Reaction, ev.User, originalText)

	msg := &pb.Message{
		Id:             uuid.NewString(),
		Timestamp:      timestamppb.New(time.Now()),
		Platform:       "slack",
		Content:        content,
		ConversationId: conversationID,
		PlatformContext: &pb.PlatformContext{
			MessageId: ev.Item.Timestamp,
			ChannelId: ev.Item.Channel,
			ThreadId:  threadID,
		},
		User: &pb.User{
			Id: ev.User,
		},
	}

	if a.msgHandler != nil {
		if err := a.msgHandler(ctx, msg); err != nil {
			slog.Error(fmt.Sprintf("[Slack] Error handling reaction: %v", err))
			a.sendErrorMessage(ctx, ev.Item.Channel, threadID, err)
		}
	}
}

// fetchMessageText retrieves the text of a single message by channel + timestamp.
func (a *SlackAdapter) fetchMessageText(ctx context.Context, channelID, timestamp string) string {
	msgs, _, _, err := a.client.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: timestamp,
		Limit:     1,
		Inclusive:  true,
	})
	if err != nil {
		slog.Error(fmt.Sprintf("[Slack] Failed to fetch message %s in %s: %v", timestamp, channelID, err))
		return ""
	}
	for _, m := range msgs {
		if m.Timestamp == timestamp {
			return m.Text
		}
	}
	return ""
}

// sendErrorMessage posts user-facing errors to Slack. Infrastructure errors
// (e.g. agent not connected) are kept in logs only to avoid channel spam.
func (a *SlackAdapter) sendErrorMessage(ctx context.Context, channelID, threadTS string, err error) {
	if errors.Is(err, adapter.ErrNoAgentStream) {
		slog.Info(fmt.Sprintf("[Slack] Suppressed infrastructure error (not posting to channel): %v", err))
		return
	}

	content := fmt.Sprintf(":x: Error: %s", err.Error())
	if a.config.DevMode {
		content += "\n\n:test_tube: _Sent from dev environment_"
	}
	_, _, postErr := a.client.PostMessageContext(ctx, channelID,
		slack.MsgOptionText(content, false),
		slack.MsgOptionTS(threadTS),
	)
	if postErr != nil {
		slog.Error(fmt.Sprintf("[Slack] Error sending error message: %v", postErr))
	}
}

// sendNotEnabledMessage tells the user the app is not enabled for this channel or user
func (a *SlackAdapter) sendNotEnabledMessage(ctx context.Context, channelID, threadTS string) {
	content := "This app has not been enabled for this channel or user. Please contact your workspace admin to enable it."
	if a.config.DevMode {
		content += "\n\n:test_tube: _Sent from dev environment_"
	}
	_, _, postErr := a.client.PostMessageContext(ctx, channelID,
		slack.MsgOptionText(content, false),
		slack.MsgOptionTS(threadTS),
	)
	if postErr != nil {
		slog.Error(fmt.Sprintf("[Slack] Error sending not-enabled message: %v", postErr))
	}
}

// isAllowed returns true if the message should be dispatched (no allowlist, or channel/user allowed)
func (a *SlackAdapter) isAllowed(channelID, userID string) bool {
	allowedChannels := a.config.AllowedChannelIDs
	allowedUsers := a.config.AllowedUserIDs
	return (len(allowedChannels) == 0 && len(allowedUsers) == 0) ||
		slices.Contains(allowedChannels, channelID) ||
		slices.Contains(allowedUsers, userID)
}

// SetMessageHandler sets the handler for incoming messages from the platform
func (a *SlackAdapter) SetMessageHandler(handler adapter.MessageHandler) {
	a.msgHandler = handler
}

// GetPlatformName returns the platform identifier
func (a *SlackAdapter) GetPlatformName() string {
	return "slack"
}

// IsHealthy checks if the adapter is connected and healthy
func (a *SlackAdapter) IsHealthy(ctx context.Context) bool {
	if a.client == nil {
		return false
	}

	// Test authentication
	_, err := a.client.AuthTestContext(ctx)
	return err == nil
}

// Stop gracefully shuts down the adapter
func (a *SlackAdapter) Stop(ctx context.Context) error {
	slog.Info("[Slack] Stopping adapter...")

	// Signal stop to event listener
	close(a.stopChan)

	slog.Info("[Slack] Adapter stopped")
	return nil
}

// Capabilities returns the adapter's capabilities
func (a *SlackAdapter) Capabilities() adapter.AdapterCapabilities {
	return adapter.SlackCapabilities(false)
}

// HydrateThread fetches thread history from Slack API
func (a *SlackAdapter) HydrateThread(ctx context.Context, conversationID string, threadStore *store.ThreadHistoryStore) error {
	channelID, threadTS, err := a.parseConversationID(conversationID)
	if err != nil {
		return fmt.Errorf("invalid conversation ID: %w", err)
	}

	slog.Info(fmt.Sprintf("[Slack] Hydrating thread: channel=%s, thread=%s", channelID, threadTS))

	var messages []slack.Message

	if threadTS != "" {
		msgs, _, _, err := a.client.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
			ChannelID: channelID,
			Timestamp: threadTS,
			Limit:     50,
		})
		if err != nil {
			return fmt.Errorf("failed to fetch thread: %w", err)
		}
		messages = msgs
	} else {
		history, err := a.client.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
			ChannelID: channelID,
			Limit:     50,
		})
		if err != nil {
			return fmt.Errorf("failed to fetch history: %w", err)
		}
		messages = history.Messages
	}

	for _, msg := range messages {
		if msg.Type != "message" || msg.SubType == "bot_message" {
			continue
		}

		threadMsg := &pb.ThreadMessage{
			MessageId: msg.Timestamp,
			User: &pb.User{
				Id:       msg.User,
				Username: msg.Username,
			},
			Content:   msg.Text,
			Timestamp: timestamppb.New(parseSlackTimestamp(msg.Timestamp)),
			WasEdited: msg.Edited != nil,
			PlatformData: map[string]string{
				"team":    msg.Team,
				"subtype": msg.SubType,
			},
		}

		if msg.Edited != nil {
			threadMsg.EditedAt = timestamppb.New(parseSlackTimestamp(msg.Edited.Timestamp))
		}

		threadStore.AddMessage(conversationID, threadMsg)
	}

	slog.Info(fmt.Sprintf("[Slack] Hydrated %d messages for %s", len(messages), conversationID))
	return nil
}

// ============================================================================
// Helper Functions
// ============================================================================

func parseSlackTimestamp(ts string) time.Time {
	parts := strings.Split(ts, ".")
	if len(parts) == 0 {
		return time.Now()
	}
	var seconds int64
	_, _ = fmt.Sscanf(parts[0], "%d", &seconds)
	return time.Unix(seconds, 0)
}

func FormatMessageID(channelID, timestamp string) string {
	return fmt.Sprintf("%s:%s", channelID, timestamp)
}

func ParseMessageID(messageID string) (string, string) {
	parts := strings.Split(messageID, ":")
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func stripMentions(text string) string {
	re := regexp.MustCompile(`<@[A-Z0-9]+>`)
	text = re.ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}
