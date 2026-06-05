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
	client          *slack.Client
	socketClient    *socketmode.Client
	config          adapter.Config
	msgHandler      adapter.MessageHandler
	feedbackHandler adapter.FeedbackHandler
	authz           authz.Authorizer // nil = skip authz (dev convenience)
	rateLimiter     *RateLimiter
	stopChan        chan struct{}
	aiClient        *SlackAIClient

	// contentBuffers accumulates DELTA chunks per conversation so the adapter
	// can send a single complete message to Slack on END.
	contentBuffers map[string]string

	// actionableReactions is the set of emoji names forwarded to the agent.
	// Built from config at initialization; empty map means no reactions are forwarded.
	actionableReactions map[string]bool

	// observeChannels is the set of channel IDs whose top-level messages are
	// forwarded (built from config.ObserveChannelIDs at init).
	observeChannels map[string]bool

	// botUserID is the authed bot user (U…), resolved via auth.test at init when
	// observe channels are configured. Used to drop top-level messages that
	// contain <@botUserID> so they don't double-deliver with app_mention.
	botUserID string

	// msgDedup suppresses duplicate top-level deliveries (Slack retries) in
	// observe channels.
	msgDedup *slackMsgDedup

	// profileCache memoizes users.info results so the Slack profile enrichment
	// path doesn't add one Slack API call per message.
	profileCache *slackUserProfileCache
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
//
// Side effect: on success this overwrites msg.User.Id with the canonical
// trace identity — the resolved WorkOS user_id when the Slack user has
// linked, otherwise the raw Slack ID (e.g. "U07ABCDEF") passed through
// unchanged. Downstream the agent SDK plumbs msg.User.Id into
// langfuse.user.id, so unlinked Slack users land on their own
// per-Slack-ID row in Insights instead of the generic Unattributed
// bucket.
func (a *SlackAdapter) dispatch(ctx context.Context, msg *pb.Message, teamID string) error {
	// Observe channels are passive watch channels — the user didn't address the
	// bot, so per-user authz doesn't apply. Operators opt into this by listing
	// the channel in observe_channel_ids. Identity rewrite is also skipped: the
	// observed user didn't address the bot, so attribution back to them on a
	// trace would misrepresent intent.
	observed := msg != nil && msg.PlatformContext != nil && a.observeChannels[msg.PlatformContext.ChannelId]

	if !observed && a.authz != nil && msg != nil && msg.User != nil {
		profile := a.lookupSlackUserProfile(ctx, teamID, msg.User.Id)
		result, err := a.authz.Authorize(ctx, authz.IdentityTypeSlack, msg.User.Id, authz.AdapterSlack, teamID, profile)
		if err != nil {
			slog.Warn("[Slack] authz check failed",
				"user_id", msg.User.Id, "err", err)
			return errAuthzUnavailable
		}
		if !result.Allowed {
			slog.Warn("[Slack] message denied by authz",
				"user_id", msg.User.Id)
			return errAuthzDenied
		}
		msg.User.Id = canonicalUserID(result, msg.User.Id)
	}
	if a.msgHandler == nil {
		return nil
	}
	return a.msgHandler(ctx, msg)
}

// canonicalUserID converts a Slack identity to the form that should appear
// as Langfuse user_id on the trace.
//
// Linked users get their WorkOS id (full attribution; the Insights People
// table renders them as the matching account member with name + avatar).
// Unlinked users keep the raw Slack id — this is the same format every
// pre-existing trace in Langfuse already carries, so there's a single
// aggregation key per Slack user and no historical-vs-new row duplication.
//
// The workspace team_id never lives in user_id; the astro-server side keeps
// it in slack_identity_mappings (populated by the live-ingest path on
// /authorize and a one-time backfill) so the Insights deep link still works
// without a namespaced wire format.
func canonicalUserID(result authz.Result, slackUserID string) string {
	if result.UserID != "" {
		return result.UserID
	}
	return slackUserID
}

// New creates a new Slack adapter
func New() *SlackAdapter {
	return &SlackAdapter{
		stopChan:       make(chan struct{}),
		contentBuffers: make(map[string]string),
		profileCache:   newSlackUserProfileCache(),
	}
}

// Initialize sets up the Slack adapter with configuration
func (a *SlackAdapter) Initialize(ctx context.Context, config adapter.Config) error {
	a.config = config
	if a.profileCache == nil {
		a.profileCache = newSlackUserProfileCache()
	}

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

	a.aiClient = NewSlackAIClient(config.BotToken, config.DevMode, config.AgentID)

	a.actionableReactions = make(map[string]bool, len(config.ActionableReactions))
	for _, r := range config.ActionableReactions {
		a.actionableReactions[r] = true
	}

	a.observeChannels = make(map[string]bool, len(config.ObserveChannelIDs))
	for _, id := range config.ObserveChannelIDs {
		if id != "" {
			a.observeChannels[id] = true
		}
	}
	if len(a.observeChannels) > 0 {
		a.msgDedup = newSlackMsgDedup(512)
	}
	// Resolve the bot user id once at init so every outbound Message can carry
	// it on PlatformContext (agents may need it to detect "I was @-mentioned"
	// on paths where the adapter strips the mention or doesn't see it).
	if auth, err := a.client.AuthTestContext(ctx); err != nil {
		slog.Warn("[Slack] AuthTest failed during init; bot_user_id will be empty", "err", err)
	} else if auth.UserID != "" {
		a.botUserID = auth.UserID
		slog.Info("[Slack] resolved bot user id from auth.test", "bot_user_id", a.botUserID)
	}

	slog.Info(fmt.Sprintf("[Slack] Adapter initialized (Socket Mode: %v, observe channels: %v, actionable reactions: %v, allowed channels: %v, allowed user IDs: %v)",
		config.SocketMode, config.ObserveChannelIDs, config.ActionableReactions, config.AllowedChannelIDs, config.AllowedUserIDs))
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
		// Acknowledge the event. An Ack failure here is non-fatal: Slack will
		// retry the event, and the duplicate is suppressed downstream by the
		// observe-channel dedup. Log so an outage is still visible.
		if err := a.socketClient.Ack(*evt.Request); err != nil {
			slog.Warn("[Slack] Ack failed for events API", "err", err)
		}

		// Handle the inner event
		eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			slog.Warn("[Slack] Could not type cast event to EventsAPIEvent")
			return
		}

		a.handleInnerEvent(ctx, eventsAPIEvent.InnerEvent, eventsAPIEvent.TeamID)

	case socketmode.EventTypeInteractive:
		// Handle block actions (feedback buttons, etc.) and view submissions
		// (free-form feedback modal). View submissions need a *payload* in the
		// ack (response_action) to surface validation errors back to the modal,
		// so we defer the ack to handleViewSubmission for that case. Everything
		// else gets the immediate empty ack — Slack retries on failure, so a
		// missed Ack is non-fatal.
		callback, ok := evt.Data.(slack.InteractionCallback)
		if !ok {
			slog.Debug("[Slack] Interactive event received (not a callback)")
			if err := a.socketClient.AckCtx(ctx, evt.Request.EnvelopeID, nil); err != nil {
				slog.Warn("[Slack] Ack failed for interactive event", "err", err)
			}
			break
		}
		switch callback.Type {
		case slack.InteractionTypeBlockActions:
			if err := a.socketClient.AckCtx(ctx, evt.Request.EnvelopeID, nil); err != nil {
				slog.Warn("[Slack] Ack failed for block actions", "err", err)
			}
			a.handleBlockActions(ctx, &callback)
		case slack.InteractionTypeViewSubmission:
			// handleViewSubmission acks with response_action when validation fails.
			a.handleViewSubmission(ctx, evt.Request, &callback)
		default:
			if err := a.socketClient.AckCtx(ctx, evt.Request.EnvelopeID, nil); err != nil {
				slog.Warn("[Slack] Ack failed for interactive event", "err", err)
			}
			slog.Debug("[Slack] Interactive event received (not yet handled)", "type", callback.Type)
		}

	case socketmode.EventTypeSlashCommand:
		// Acknowledge slash commands before any processing (3-second window).
		// Slack retries on Ack failure, so logging and continuing is correct.
		if err := a.socketClient.Ack(*evt.Request); err != nil {
			slog.Warn("[Slack] Ack failed for slash command", "err", err)
		}
		cmd, ok := evt.Data.(slack.SlashCommand)
		if !ok {
			slog.Warn("[Slack] Slash command: could not parse payload")
			return
		}
		a.handleSlashCommand(ctx, cmd)

	case socketmode.EventTypeHello:
		// Hello event is just a connection acknowledgment, no action needed

	default:
		// Only log truly unknown event types at debug level
		if evt.Type != "" {
			slog.Debug(fmt.Sprintf("[Slack] Unhandled event type: %s", evt.Type))
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

	case *slackevents.AssistantThreadStartedEvent:
		a.handleAssistantThreadStarted(ctx, ev, teamID)

	default:
		slog.Debug(fmt.Sprintf("[Slack] Unhandled inner event type: %s", innerEvent.Type))
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

	isDM := ev.Channel != "" && ev.Channel[0] == 'D'
	isChannel := ev.Channel != "" && ev.Channel[0] != 'D'
	topLevelInChannel := isChannel && ev.ThreadTimeStamp == ""
	observe := topLevelInChannel && a.observeChannels[ev.Channel]

	// In channels, top-level messages are usually dropped (handled via app_mention).
	// Observe channels forward them, except when the text @-mentions the bot
	// (those still flow through app_mention to avoid double-delivery).
	if topLevelInChannel {
		if !observe {
			slog.Debug(fmt.Sprintf("[Slack] Ignoring top-level message in channel %s (will handle via app_mention)", ev.Channel))
			return
		}
		if a.botUserID != "" && strings.Contains(ev.Text, "<@"+a.botUserID+">") {
			slog.Debug("[Slack] Skipping observed top-level message: contains bot mention (handled via app_mention)",
				"channel_id", ev.Channel, "message_ts", ev.TimeStamp, "user_id", ev.User)
			metrics.MessagesDropped.WithLabelValues("slack", "app_mention_dedup").Inc()
			return
		}
		if a.msgDedup != nil {
			key := ev.Channel + ":" + ev.TimeStamp
			if ok, dupAge := a.msgDedup.shouldDeliver(key); !ok {
				slog.Debug("[Slack] Dedup: skipping duplicate observed message",
					"dedup_key", key, "channel_id", ev.Channel, "message_ts", ev.TimeStamp,
					"user_id", ev.User, "since_first_delivery", dupAge)
				return
			}
		}
		slog.Debug(fmt.Sprintf("[Slack] Processing observed top-level message in channel %s", ev.Channel))
	} else if isChannel {
		slog.Debug(fmt.Sprintf("[Slack] Processing thread reply in channel %s, thread=%s", ev.Channel, ev.ThreadTimeStamp))
	}

	// threadID is the slack thread root for replies / observed top-level messages
	// so the agent can post back into the right thread.
	threadID := ev.ThreadTimeStamp
	if observe {
		threadID = ev.TimeStamp
	}

	// Allowlist: if configured, only allow messages from allowed channels or users
	if !a.isAllowed(ev.Channel, ev.User) {
		slog.Debug(fmt.Sprintf("[Slack] Message from disallowed channel=%s or user=%s", ev.Channel, ev.User))
		metrics.MessagesDropped.WithLabelValues("slack", "allowlist").Inc()
		a.sendNotEnabledMessage(ctx, ev.Channel, threadID)
		return
	}

	eventType := "thread_reply"
	switch {
	case isDM:
		eventType = "dm"
	case observe:
		eventType = "observed_top"
	}
	metrics.SlackEvents.WithLabelValues(eventType).Inc()

	slog.Debug(fmt.Sprintf("[Slack] Message received: channel=%s, user=%s, text=%s", ev.Channel, ev.User, ev.Text))

	// Build conversation ID
	conversationID := ev.Channel
	if threadID != "" {
		conversationID = fmt.Sprintf("%s-%s", ev.Channel, threadID)
	}

	eventKind := pb.PlatformContext_EVENT_KIND_THREAD_REPLY
	switch {
	case observe:
		eventKind = pb.PlatformContext_EVENT_KIND_OBSERVED
	case isDM:
		eventKind = pb.PlatformContext_EVENT_KIND_DM
	}

	// Convert to pb.Message
	msg := &pb.Message{
		Id:             uuid.NewString(),
		Timestamp:      timestamppb.New(parseSlackTimestamp(ev.TimeStamp)),
		Platform:       "slack",
		Content:        renderBlocks(ev.Text, ev.Blocks),
		ConversationId: conversationID,
		PlatformContext: &pb.PlatformContext{
			MessageId:    ev.TimeStamp,
			ChannelId:    ev.Channel,
			ThreadId:     threadID,
			ThreadRootId: ev.ThreadTimeStamp, // empty for top-level (DM and observed)
			EventKind:    eventKind,
			BotUserId:    a.botUserID,
		},
		User: &pb.User{
			Id: ev.User,
		},
	}

	// Call handler if registered
	if a.msgHandler != nil {
		if err := a.dispatch(ctx, msg, teamID); err != nil {
			slog.Error(fmt.Sprintf("[Slack] Error handling message: %v", err))
			a.sendErrorMessage(ctx, ev.Channel, threadID, err)
		}
	}
}

// handleBlockActions processes block action events (button clicks, etc.)
func (a *SlackAdapter) handleBlockActions(ctx context.Context, callback *slack.InteractionCallback) {
	slog.Debug(fmt.Sprintf("[Slack] Block action received: type=%s, actions=%d", callback.Type, len(callback.ActionCallback.BlockActions)))

	for _, action := range callback.ActionCallback.BlockActions {
		slog.Debug(fmt.Sprintf("[Slack] Action: id=%s, value=%s", action.ActionID, action.Value))

		// feedback_buttons: built-in Slack AI thumbs up/down widget — handled locally
		//   AND forwarded to the agent via the FeedbackHandler so developers can
		//   persist the signal (Airtable, eval pipelines, etc.).
		// feedback_comment: our extension — opens a free-form text modal. Modal
		//   submission is handled in handleViewSubmission.
		// All other block actions are agent-sent interactive buttons forwarded
		// via routeButtonClickToAgent.
		switch action.ActionID {
		case feedbackButtonsActionID:
			a.handleFeedbackButton(ctx, callback, action)
		case feedbackCommentActionID:
			a.handleFeedbackCommentOpen(ctx, callback, action)
		default:
			a.routeButtonClickToAgent(ctx, callback, action)
		}
	}
}

// handleFeedbackButton processes a thumbs-up/down click on the Slack AI
// feedback_buttons widget. It (1) updates the visible UI (removes the
// buttons, adds a reaction emoji) so the user sees their click registered,
// and (2) forwards a PlatformFeedback{MessageReaction} to the agent so the
// developer's on_feedback callback fires.
func (a *SlackAdapter) handleFeedbackButton(ctx context.Context, callback *slack.InteractionCallback, action *slack.BlockAction) {
	feedbackType := action.Value // "positive_feedback" or "negative_feedback"
	slog.Debug(fmt.Sprintf("[Slack] Feedback received: %s from user %s on message %s",
		feedbackType, callback.User.ID, callback.Message.Timestamp))

	// Use Slack emoji names (not emoji characters)
	emojiName := "thumbsup"
	reactionType := pb.MessageReaction_THUMBS_UP
	if feedbackType == "negative_feedback" {
		emojiName = "thumbsdown"
		reactionType = pb.MessageReaction_THUMBS_DOWN
	}

	// Remove feedback buttons from the message first
	if len(callback.Message.Blocks.BlockSet) > 0 {
		updatedBlocks := []slack.Block{}
		for _, block := range callback.Message.Blocks.BlockSet {
			// Filter out the context_actions block (Slack AI feedback block type)
			// AND the actions block that hosts our :speech_balloon: comment button.
			bt := block.BlockType()
			if bt == "context_actions" {
				continue
			}
			if bt == slack.MBTAction {
				if ab, ok := block.(*slack.ActionBlock); ok && ab.BlockID == feedbackCommentBlockID {
					continue
				}
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
			slog.Debug("[Slack] Feedback buttons removed from message")
		}
	}

	// Acknowledge the feedback visually by adding a reaction
	if err := a.client.AddReaction(emojiName, slack.ItemRef{
		Channel:   callback.Channel.ID,
		Timestamp: callback.Message.Timestamp,
	}); err != nil {
		slog.Error(fmt.Sprintf("[Slack] Failed to add reaction: %v", err))
	} else {
		slog.Debug(fmt.Sprintf("[Slack] Feedback acknowledged with :%s: reaction", emojiName))
	}

	// Forward to agent so developer-supplied callback fires.
	a.forwardFeedback(ctx, &pb.PlatformFeedback{
		ConversationId: conversationIDFromCallback(callback),
		ResponseId:     callback.Message.Timestamp,
		Timestamp:      timestamppb.Now(),
		User: &pb.User{
			Id:       callback.User.ID,
			Username: callback.User.Name,
		},
		Feedback: &pb.PlatformFeedback_Reaction{
			Reaction: &pb.MessageReaction{
				Type:  reactionType,
				Added: true,
			},
		},
	})
}

// Block / action / callback identifiers for the feedback UI. Centralised so
// the post-render (slack_ai_api.go) and the click/submission handlers
// (adapter.go) cannot drift out of sync — a typo in either side silently
// routes the click to the wrong handler.
const (
	// feedbackButtonsActionID is the native Slack AI thumbs widget's action_id.
	feedbackButtonsActionID = "feedback_buttons"

	// feedbackCommentActionID identifies the 💬 Comment button click that
	// opens the free-form text modal.
	feedbackCommentActionID = "feedback_comment"

	// feedbackCommentBlockID identifies the actions block that hosts the
	// 💬 comment button (so handleFeedbackButton can strip it alongside the
	// thumbs widget when removing feedback UI after a click).
	feedbackCommentBlockID = "astropods_feedback_comment_actions"

	// feedbackCommentCallbackID identifies the modal view submission so
	// handleViewSubmission knows the submission came from our comment dialog.
	feedbackCommentCallbackID = "astropods_feedback_comment_modal"

	// feedbackCommentInputBlockID / feedbackCommentInputActionID locate the
	// text input inside the modal — both needed to read the typed value out
	// of the view_submission state map.
	feedbackCommentInputBlockID  = "astropods_feedback_comment_input"
	feedbackCommentInputActionID = "astropods_feedback_comment_text"
)

// handleFeedbackCommentOpen opens a Slack modal where the user types
// free-form feedback. We stash channel + message ts in private_metadata so
// handleViewSubmission can resolve the conversation when the modal is
// submitted (callbacks don't carry the originating message context).
func (a *SlackAdapter) handleFeedbackCommentOpen(ctx context.Context, callback *slack.InteractionCallback, action *slack.BlockAction) {
	if callback.TriggerID == "" {
		slog.Warn("[Slack] feedback_comment: missing trigger_id, cannot open modal")
		return
	}

	privateMeta, err := json.Marshal(map[string]string{
		"channel_id":      callback.Channel.ID,
		"message_ts":      callback.Message.Timestamp,
		"thread_ts":       callback.Message.ThreadTimestamp,
		"conversation_id": conversationIDFromCallback(callback),
	})
	if err != nil {
		slog.Error("[Slack] feedback_comment: failed to encode private_metadata", "err", err)
		return
	}

	modal := slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      feedbackCommentCallbackID,
		Title:           slack.NewTextBlockObject(slack.PlainTextType, "Share feedback", false, false),
		Submit:          slack.NewTextBlockObject(slack.PlainTextType, "Send", false, false),
		Close:           slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
		PrivateMetadata: string(privateMeta),
		Blocks: slack.Blocks{
			BlockSet: []slack.Block{
				slack.NewInputBlock(
					feedbackCommentInputBlockID,
					slack.NewTextBlockObject(slack.PlainTextType, "What did you think of this reply?", false, false),
					slack.NewTextBlockObject(slack.PlainTextType, "Your feedback goes straight to the team building this agent.", false, false),
					&slack.PlainTextInputBlockElement{
						Type:        slack.METPlainTextInput,
						ActionID:    feedbackCommentInputActionID,
						Multiline:   true,
						Placeholder: slack.NewTextBlockObject(slack.PlainTextType, "Anything you'd like the agent's developer to know…", false, false),
					},
				),
			},
		},
	}

	if _, err := a.client.OpenView(callback.TriggerID, modal); err != nil {
		slog.Error("[Slack] feedback_comment: OpenView failed", "err", err)
	}
}

// handleViewSubmission handles modal submissions. Currently only the
// feedback comment modal is wired; other view_submission callbacks are
// logged and dropped.
//
// This function owns the Slack ack for view_submission events because empty
// submissions are surfaced back to the modal via a response_action payload
// (which must be sent inside the ack, not as a follow-up REST call).
func (a *SlackAdapter) handleViewSubmission(ctx context.Context, req *socketmode.Request, callback *slack.InteractionCallback) {
	view := callback.View
	if view.CallbackID != feedbackCommentCallbackID {
		slog.Debug("[Slack] Unhandled view_submission", "callback_id", view.CallbackID)
		a.ackViewSubmission(ctx, req, nil)
		return
	}

	var meta struct {
		ChannelID      string `json:"channel_id"`
		MessageTS      string `json:"message_ts"`
		ThreadTS       string `json:"thread_ts"`
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal([]byte(view.PrivateMetadata), &meta); err != nil {
		slog.Error("[Slack] feedback_comment: bad private_metadata", "err", err)
		a.ackViewSubmission(ctx, req, nil)
		return
	}

	var text string
	if blockVals, ok := view.State.Values[feedbackCommentInputBlockID]; ok {
		if input, ok := blockVals[feedbackCommentInputActionID]; ok {
			text = input.Value
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		// Show the inline error in the modal so the user knows their click
		// didn't submit anything — silent close would look like a UI bug.
		slog.Debug("[Slack] feedback_comment: empty submission, returning validation error")
		a.ackViewSubmission(ctx, req, slack.NewErrorsViewSubmissionResponse(map[string]string{
			feedbackCommentInputBlockID: "Please type something before sending.",
		}))
		return
	}

	// Valid submission — close the modal with an empty ack.
	a.ackViewSubmission(ctx, req, nil)

	// Acknowledge in the original thread so the user knows their comment landed.
	if meta.ChannelID != "" && meta.MessageTS != "" {
		if err := a.client.AddReaction("speech_balloon", slack.ItemRef{
			Channel:   meta.ChannelID,
			Timestamp: meta.MessageTS,
		}); err != nil {
			slog.Debug("[Slack] feedback_comment: ack reaction failed", "err", err)
		}
	}

	a.forwardFeedback(ctx, &pb.PlatformFeedback{
		ConversationId: meta.ConversationID,
		ResponseId:     meta.MessageTS,
		Timestamp:      timestamppb.Now(),
		User: &pb.User{
			Id:       callback.User.ID,
			Username: callback.User.Name,
		},
		Feedback: &pb.PlatformFeedback_Text{
			Text: &pb.TextFeedback{
				Text:   text,
				Prompt: "What did you think of this reply?",
			},
		},
	})
}

// ackViewSubmission sends the socket-mode ack for a view_submission. payload
// nil closes the modal; pass a *slack.ViewSubmissionResponse (e.g. errors,
// update, push) to keep the modal open with that response_action.
//
// When socketClient is nil (unit-test path) or req is nil, the ack is a no-op
// — the handler still runs its forwarding logic so behaviour can be asserted
// without a live Slack socket.
func (a *SlackAdapter) ackViewSubmission(ctx context.Context, req *socketmode.Request, payload any) {
	if a.socketClient == nil || req == nil {
		return
	}
	if err := a.socketClient.AckCtx(ctx, req.EnvelopeID, payload); err != nil {
		slog.Warn("[Slack] Ack failed for view_submission", "err", err)
	}
}

// conversationIDFromCallback reconstructs the conversation ID used elsewhere
// in this adapter ("<channel>-<thread>"). Block-action callbacks expose
// the originating thread via Container.ThreadTs or the message's own ts.
func conversationIDFromCallback(callback *slack.InteractionCallback) string {
	threadTS := callback.Container.ThreadTs
	if threadTS == "" {
		threadTS = callback.Message.ThreadTimestamp
	}
	if threadTS == "" {
		threadTS = callback.Message.Timestamp
	}
	return fmt.Sprintf("%s-%s", callback.Channel.ID, threadTS)
}

// forwardFeedback routes a PlatformFeedback through the registered handler.
// No-ops cleanly when the handler is unset (single-binary dev runs) or when
// no agent is currently connected.
func (a *SlackAdapter) forwardFeedback(ctx context.Context, fb *pb.PlatformFeedback) {
	if a.feedbackHandler == nil {
		return
	}
	if err := a.feedbackHandler(ctx, fb); err != nil {
		if errors.Is(err, adapter.ErrNoAgentStream) {
			slog.Debug("[Slack] Feedback dropped: no agent connected")
			return
		}
		slog.Error("[Slack] Feedback forward failed", "err", err)
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

	// The button lives on a specific message; if that message is itself a
	// thread reply, ThreadTimestamp gives us the parent thread root.
	threadRootID := callback.Message.ThreadTimestamp

	msg := &pb.Message{
		Id:             uuid.NewString(),
		Timestamp:      timestamppb.New(time.Now()),
		Platform:       "slack",
		Content:        string(content),
		ConversationId: fmt.Sprintf("%s-%s", channelID, threadTS),
		PlatformContext: &pb.PlatformContext{
			MessageId:    callback.Message.Timestamp,
			ChannelId:    channelID,
			ThreadId:     threadTS,
			ThreadRootId: threadRootID,
			EventKind:    pb.PlatformContext_EVENT_KIND_BUTTON_CLICK,
			BotUserId:    a.botUserID,
		},
		User: &pb.User{Id: callback.User.ID},
	}

	slog.Debug(fmt.Sprintf("[Slack] Routing button click to agent: action_id=%s, user=%s", action.ActionID, callback.User.ID))
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
	slog.Debug(fmt.Sprintf("[Slack] Slash command: %s %q from user=%s in channel=%s",
		cmd.Command, cmd.Text, cmd.UserID, cmd.ChannelID))

	if !a.isAllowed(cmd.ChannelID, cmd.UserID) {
		slog.Debug(fmt.Sprintf("[Slack] Slash command from disallowed channel=%s or user=%s", cmd.ChannelID, cmd.UserID))
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
			EventKind: pb.PlatformContext_EVENT_KIND_SLASH_COMMAND,
			BotUserId: a.botUserID,
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
func (a *SlackAdapter) handleAssistantThreadStarted(ctx context.Context, ev *slackevents.AssistantThreadStartedEvent, teamID string) {
	channelID := ev.AssistantThread.ChannelID
	threadTS := ev.AssistantThread.ThreadTimeStamp
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
			MessageId:    threadTS,
			ChannelId:    channelID,
			ThreadId:     threadTS,
			ThreadRootId: threadTS,
			EventKind:    pb.PlatformContext_EVENT_KIND_ASSISTANT_THREAD_STARTED,
			BotUserId:    a.botUserID,
		},
		User: &pb.User{Id: userID},
	}

	slog.Debug(fmt.Sprintf("[Slack] Forwarding assistant_thread_started to agent: channel=%s thread=%s user=%s", channelID, threadTS, userID))
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
	slog.Debug(fmt.Sprintf("[Slack] App mentioned: channel=%s, user=%s, text=%s", ev.Channel, ev.User, ev.Text))

	// Allowlist: if configured, only allow mentions from allowed channels or users
	if !a.isAllowed(ev.Channel, ev.User) {
		slog.Debug(fmt.Sprintf("[Slack] App mention from disallowed channel=%s or user=%s", ev.Channel, ev.User))
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
	// Render any Block Kit content into the merged text, then strip bot
	// mentions from the combined string — rich_text user elements get
	// rendered as <@U…> by renderBlocks, so the same regex handles them.
	text := stripMentions(renderBlocks(ev.Text, ev.Blocks))

	slog.Debug(fmt.Sprintf("[Slack] Setting loading state: channel=%s, threadTS=%s", ev.Channel, threadID))
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
			MessageId:    ev.TimeStamp,
			ChannelId:    ev.Channel,
			ThreadId:     threadID,
			ThreadRootId: ev.ThreadTimeStamp, // empty for top-level mentions
			EventKind:    pb.PlatformContext_EVENT_KIND_APP_MENTION,
			BotUserId:    a.botUserID,
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
	slog.Debug(fmt.Sprintf("[Slack] Reaction added: emoji=%s, user=%s, channel=%s, item_ts=%s",
		ev.Reaction, ev.User, ev.Item.Channel, ev.Item.Timestamp))

	if !a.actionableReactions[ev.Reaction] {
		slog.Debug(fmt.Sprintf("[Slack] Ignoring non-actionable reaction :%s:", ev.Reaction))
		return
	}

	metrics.SlackEvents.WithLabelValues("reaction").Inc()

	originalText, parentThreadTs, ok := a.fetchReactionMessage(ctx, ev.Item.Channel, ev.Item.Timestamp)
	if !ok || originalText == "" {
		slog.Debug("[Slack] Could not fetch original message for reaction, skipping")
		return
	}

	// Reply target: if the reacted message is already in a thread, post into
	// that thread; otherwise the agent's reply opens a new thread under it.
	threadID := parentThreadTs
	if threadID == "" {
		threadID = ev.Item.Timestamp
	}
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
			MessageId:    ev.Item.Timestamp,
			ChannelId:    ev.Item.Channel,
			ThreadId:     threadID,
			ThreadRootId: parentThreadTs, // empty when reaction is on a top-level message
			EventKind:    pb.PlatformContext_EVENT_KIND_REACTION,
			BotUserId:    a.botUserID,
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

// fetchReactionMessage loads the reacted message text and (when present) its
// parent thread timestamp. The parent thread ts is what lets handleReactionAdded
// populate PlatformContext.ThreadRootId so the agent can distinguish a reaction
// on a top-level message from a reaction on a reply in an existing thread.
func (a *SlackAdapter) fetchReactionMessage(ctx context.Context, channelID, timestamp string) (text string, parentThreadTs string, ok bool) {
	msgs, _, _, err := a.client.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: timestamp,
		Limit:     1,
		Inclusive: true,
	})
	if err != nil {
		slog.Error(fmt.Sprintf("[Slack] Failed to fetch message %s in %s: %v", timestamp, channelID, err))
		return "", "", false
	}
	for _, m := range msgs {
		if m.Timestamp == timestamp {
			// ThreadTimestamp is set on thread replies; on a thread root it
			// equals the message's own ts, which is not "in an existing
			// thread" — so suppress that case.
			if m.ThreadTimestamp != "" && m.ThreadTimestamp != m.Timestamp {
				parentThreadTs = m.ThreadTimestamp
			}
			return renderBlocks(m.Text, m.Blocks), parentThreadTs, true
		}
	}
	return "", "", false
}

// sendErrorMessage posts user-facing errors to Slack. Infrastructure errors
// (e.g. agent not connected) are kept in logs only to avoid channel spam.
// Authz sentinels (errAuthzDenied, errAuthzUnavailable) are translated into
// fixed user-facing text so the raw error never reaches the channel.
func (a *SlackAdapter) sendErrorMessage(ctx context.Context, channelID, threadTS string, err error) {
	if errors.Is(err, adapter.ErrNoAgentStream) {
		slog.Debug(fmt.Sprintf("[Slack] Suppressed infrastructure error (not posting to channel): %v", err))
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

// SetFeedbackHandler sets the handler for incoming feedback events (thumbs
// up/down, free-form comment, etc.). When nil the adapter still renders the
// platform UI but does not forward feedback to the agent.
func (a *SlackAdapter) SetFeedbackHandler(handler adapter.FeedbackHandler) {
	a.feedbackHandler = handler
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

	slog.Debug(fmt.Sprintf("[Slack] Hydrating thread: channel=%s, thread=%s", channelID, threadTS))

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
			Content:   renderBlocks(msg.Text, msg.Blocks),
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

	slog.Debug(fmt.Sprintf("[Slack] Hydrated %d messages for %s", len(messages), conversationID))
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
