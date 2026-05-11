package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/authz"
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
	authz        authz.Authorizer // nil = skip authz (dev convenience)
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

// SetAuthorizer wires the authorizer used to gate every incoming slack
// message. nil disables the check (dev mode); production wiring sets a real
// Authorizer in main.
func (a *SlackAdapter) SetAuthorizer(az authz.Authorizer) {
	a.authz = az
}

// errAuthzDenied / errAuthzUnavailable are sentinel errors returned by
// dispatch so call sites can surface a sanitized user-facing reply via
// sendErrorMessage. The raw authz error is never posted to slack — it
// would leak internal wiring details.
var (
	errAuthzDenied      = errors.New("slack: message denied by authz")
	errAuthzUnavailable = errors.New("slack: authz check unavailable")
)

// dispatch authorizes the message's principal and forwards it to msgHandler.
// All slack-side message ingress points (events, slash commands, AI prompts,
// reactions, file uploads, ...) must go through this so authz can never be
// forgotten on a new path. On deny or authz error, returns a sentinel so the
// caller can post a sanitized reply.
//
// teamID is the slack workspace identifier. The server pairs it with
// msg.User.Id to look up the linked WorkOS user (slack_identity_mappings),
// which is the only way per-user grants on slack can match. Pass the
// raw value from the source event (MessageEvent.Team, SlashCommand.TeamID,
// etc.). Empty string is acceptable for legacy callers — the server
// falls back to the unscoped owning-account candidate.
func (a *SlackAdapter) dispatch(ctx context.Context, msg *pb.Message, teamID string) error {
	if a.authz != nil && msg != nil && msg.User != nil {
		allowed, err := a.authz.Allowed(ctx, authz.IdentityTypeSlack, msg.User.Id, authz.AdapterSlack, teamID)
		if err != nil {
			slog.Warn("[Slack] authz check failed",
				"user_id", msg.User.Id, "err", err)
			return errAuthzUnavailable
		}
		if !allowed {
			slog.Info("[Slack] message denied by authz",
				"user_id", msg.User.Id)
			return errAuthzDenied
		}
	}
	if a.msgHandler == nil {
		return nil
	}
	return a.msgHandler(ctx, msg)
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

		a.handleInnerEvent(ctx, eventsAPIEvent.InnerEvent, eventsAPIEvent.TeamID)

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
		// Acknowledge slash commands before any processing (3-second window).
		a.socketClient.Ack(*evt.Request)
		cmd, ok := evt.Data.(slack.SlashCommand)
		if !ok {
			slog.Info("[Slack] Slash command: could not parse payload")
			return
		}
		a.handleSlashCommand(ctx, cmd)

	case socketmode.EventTypeHello:
		// Hello event is just a connection acknowledgment, no action needed

	default:
		// Only log truly unknown event types at debug level
		if evt.Type != "" {
			slog.Info(fmt.Sprintf("[Slack] Unhandled event type: %s", evt.Type))
		}
	}
}

// handleInnerEvent processes the actual event data. teamID comes from the
// outer EventsAPIEvent envelope and is threaded down to every dispatch
// call so the server can resolve slack identities to WorkOS users —
// individual event payloads (e.g. ReactionAddedEvent) don't always carry
// the workspace id, so we use the envelope's value uniformly.
func (a *SlackAdapter) handleInnerEvent(ctx context.Context, innerEvent slackevents.EventsAPIInnerEvent, teamID string) {
	switch ev := innerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		a.handleMessage(ctx, ev, teamID)

	case *slackevents.AppMentionEvent:
		a.handleAppMention(ctx, ev, teamID)

	case *slackevents.ReactionAddedEvent:
		a.handleReactionAdded(ctx, ev, teamID)

	default:
		if innerEvent.Type == "assistant_thread_started" {
			a.handleAssistantThreadStarted(ctx, innerEvent, teamID)
		} else {
      slog.Info(fmt.Sprintf("[Slack] Unhandled inner event type: %s", innerEvent.Type))
		}
	}
}

// handleMessage processes message events
func (a *SlackAdapter) handleMessage(ctx context.Context, ev *slackevents.MessageEvent, teamID string) {
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
		if err := a.dispatch(ctx, msg, teamID); err != nil {
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

		// feedback_buttons: built-in Slack AI thumbs up/down widget — handled locally.
		// All other block actions are agent-sent interactive buttons forwarded to the agent.
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
		} else {
			// Agent-sent interactive button: forward to agent as an incoming message.
			a.routeButtonClickToAgent(ctx, callback, action)
		}
	}
}

// routeButtonClickToAgent forwards a Slack block-action button click to the agent
// by sending it as an incoming message with a JSON-encoded button_click payload.
// The agent identifies button-click messages by the "type":"button_click" field.
func (a *SlackAdapter) routeButtonClickToAgent(ctx context.Context, callback *slack.InteractionCallback, action *slack.BlockAction) {
	channelID := callback.Channel.ID

	// Prefer the thread root over the individual message timestamp so the
	// conversation ID matches the one used when the card was first sent.
	threadTS := callback.Container.ThreadTs
	if threadTS == "" {
		threadTS = callback.Message.ThreadTimestamp
	}
	if threadTS == "" {
		threadTS = callback.Message.Timestamp
	}
	if channelID == "" || threadTS == "" {
		slog.Warn(fmt.Sprintf("[Slack] button click: missing channel or thread TS, dropping (channel=%q)", channelID))
		return
	}

	metrics.SlackEvents.WithLabelValues("button_click").Inc()

	content, err := json.Marshal(map[string]string{
		"type":      "button_click",
		"button_id": action.ActionID,
		"value":     action.Value,
		"action":    action.ActionID,
	})
	if err != nil {
		slog.Error(fmt.Sprintf("[Slack] Failed to marshal button click payload: %v", err))
		return
	}

	msg := &pb.Message{
		Id:             uuid.NewString(),
		Timestamp:      timestamppb.New(time.Now()),
		Platform:       "slack",
		Content:        string(content),
		ConversationId: fmt.Sprintf("%s-%s", channelID, threadTS),
		PlatformContext: &pb.PlatformContext{
			ChannelId: channelID,
			ThreadId:  threadTS,
		},
		User: &pb.User{Id: callback.User.ID},
	}

	slog.Info(fmt.Sprintf("[Slack] Routing button click to agent: action_id=%s, user=%s", action.ActionID, callback.User.ID))
	if a.msgHandler != nil {
		if err := a.dispatch(ctx, msg, callback.Team.ID); err != nil {
			slog.Error(fmt.Sprintf("[Slack] Error routing button click: %v", err))
			if errors.Is(err, errAuthzDenied) || errors.Is(err, errAuthzUnavailable) {
				a.sendErrorMessage(ctx, channelID, threadTS, err)
			}
		}
	}
}

// handleSlashCommand routes a Slack slash command to the agent as an incoming message.
// The command text (without the /command prefix) is used as message content.
func (a *SlackAdapter) handleSlashCommand(ctx context.Context, cmd slack.SlashCommand) {
	slog.Info(fmt.Sprintf("[Slack] Slash command: %s %q from user=%s in channel=%s",
		cmd.Command, cmd.Text, cmd.UserID, cmd.ChannelID))

	if !a.isAllowed(cmd.ChannelID, cmd.UserID) {
		slog.Info(fmt.Sprintf("[Slack] Slash command from disallowed channel=%s or user=%s", cmd.ChannelID, cmd.UserID))
		metrics.MessagesDropped.WithLabelValues("slack", "allowlist").Inc()
		return
	}

	metrics.SlackEvents.WithLabelValues("slash_command").Inc()

	// Use the command arguments as message text; fall back to the bare command
	// name so the agent can still route on it.
	text := strings.TrimSpace(cmd.Text)
	if text == "" {
		text = strings.TrimPrefix(cmd.Command, "/")
	}

	msg := &pb.Message{
		Id:             uuid.NewString(),
		Timestamp:      timestamppb.New(time.Now()),
		Platform:       "slack",
		Content:        text,
		ConversationId: cmd.ChannelID,
		PlatformContext: &pb.PlatformContext{
			ChannelId: cmd.ChannelID,
		},
		User: &pb.User{
			Id:       cmd.UserID,
			Username: cmd.UserName,
		},
	}

	if a.msgHandler != nil {
		if err := a.dispatch(ctx, msg, cmd.TeamID); err != nil {
			slog.Error(fmt.Sprintf("[Slack] Error handling slash command: %v", err))
			if errors.Is(err, errAuthzDenied) || errors.Is(err, errAuthzUnavailable) {
				a.sendErrorMessage(ctx, cmd.ChannelID, "", err)
			}
		}
	}
}

// handleAssistantThreadStarted handles the Slack AI assistant_thread_started event,
// which fires when a user opens a new assistant thread. The event is forwarded to
// the agent as an incoming message so the agent can respond with SuggestedPrompts.
//
// Note: slack-go v0.12.3 does not have a typed struct for this event; the raw
// JSON is extracted from the InnerEvent.Data field via json.RawMessage. Upgrading
// to slack-go ≥0.13 will allow using the typed slackevents.AssistantThreadStartedEvent
// instead and this fallback can be removed.
func (a *SlackAdapter) handleAssistantThreadStarted(ctx context.Context, innerEvent slackevents.EventsAPIInnerEvent, teamID string) {
	type payload struct {
		AssistantThread struct {
			UserID    string `json:"user_id"`
			ChannelID string `json:"channel_id"`
			ThreadTs  string `json:"thread_ts"`
		} `json:"assistant_thread"`
	}

	rawData, ok := innerEvent.Data.(json.RawMessage)
	if !ok {
		slog.Warn("[Slack] assistant_thread_started: event data unavailable; upgrade slack-go to ≥0.13 for typed support")
		return
	}

	var ev payload
	if err := json.Unmarshal(rawData, &ev); err != nil {
		slog.Error(fmt.Sprintf("[Slack] assistant_thread_started: parse error: %v", err))
		return
	}

	channelID := ev.AssistantThread.ChannelID
	threadTS := ev.AssistantThread.ThreadTs
	userID := ev.AssistantThread.UserID
	if channelID == "" || threadTS == "" {
		slog.Warn(fmt.Sprintf("[Slack] assistant_thread_started: missing channel (%q) or thread TS (%q)", channelID, threadTS))
		return
	}

	metrics.SlackEvents.WithLabelValues("thread_started").Inc()

	content, err := json.Marshal(map[string]string{
		"type":    "assistant_thread_started",
		"channel": channelID,
		"thread":  threadTS,
	})
	if err != nil {
		slog.Error(fmt.Sprintf("[Slack] Failed to marshal assistant thread started payload: %v", err))
		return
	}

	msg := &pb.Message{
		Id:             uuid.NewString(),
		Timestamp:      timestamppb.New(time.Now()),
		Platform:       "slack",
		Content:        string(content),
		ConversationId: fmt.Sprintf("%s-%s", channelID, threadTS),
		PlatformContext: &pb.PlatformContext{
			ChannelId: channelID,
			ThreadId:  threadTS,
		},
		User: &pb.User{Id: userID},
	}

	slog.Info(fmt.Sprintf("[Slack] Forwarding assistant_thread_started to agent: channel=%s thread=%s user=%s", channelID, threadTS, userID))
	if a.msgHandler != nil {
		if err := a.dispatch(ctx, msg, teamID); err != nil {
			slog.Error(fmt.Sprintf("[Slack] Error forwarding assistant_thread_started: %v", err))
			if errors.Is(err, errAuthzDenied) || errors.Is(err, errAuthzUnavailable) {
				a.sendErrorMessage(ctx, channelID, threadTS, err)
			}
		}
	}
}

// handleAppMention processes app mention events
func (a *SlackAdapter) handleAppMention(ctx context.Context, ev *slackevents.AppMentionEvent, teamID string) {
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
		if err := a.dispatch(ctx, msg, teamID); err != nil {
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
func (a *SlackAdapter) handleReactionAdded(ctx context.Context, ev *slackevents.ReactionAddedEvent, teamID string) {
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
		if err := a.dispatch(ctx, msg, teamID); err != nil {
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
// Authz sentinels (errAuthzDenied, errAuthzUnavailable) are translated into
// fixed user-facing text so the raw error never reaches the channel.
func (a *SlackAdapter) sendErrorMessage(ctx context.Context, channelID, threadTS string, err error) {
	if errors.Is(err, adapter.ErrNoAgentStream) {
		slog.Info(fmt.Sprintf("[Slack] Suppressed infrastructure error (not posting to channel): %v", err))
		return
	}

	var content string
	switch {
	case errors.Is(err, errAuthzDenied):
		content = ":lock: You're not authorized to use this app. Contact your workspace admin if you think this is a mistake."
	case errors.Is(err, errAuthzUnavailable):
		content = ":hourglass_flowing_sand: Couldn't verify your access right now. Please try again in a moment."
	default:
		content = fmt.Sprintf(":x: Error: %s", err.Error())
	}
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
