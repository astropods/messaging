package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/store"
	"github.com/astropods/messaging/internal/store/sqlite"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type interactionResponseInput struct {
	Action  string          `json:"action"`
	Content json.RawMessage `json:"content,omitempty"` // SUBMIT payload
	Text    string          `json:"text,omitempty"`    // RESPOND payload
}

type interactionResponseAck struct {
	Status string `json:"status"`
	Action string `json:"action"`
}

// HandleInteractionResponse handles POST
// /api/chat/conversations/{id}/interactions/{interactionId}: the user's answer to
// a blocking interaction.
func (h *Handlers) HandleInteractionResponse(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	session := h.authenticate(w, r)
	if session == nil {
		return
	}

	conversationID := r.PathValue("id")
	interactionID := r.PathValue("interactionId")
	if conversationID == "" || interactionID == "" {
		http.Error(w, "Missing conversation or interaction ID", http.StatusBadRequest)
		return
	}
	if h.interactions == nil {
		http.Error(w, "interactions unavailable", http.StatusServiceUnavailable)
		return
	}

	// Cap the body: a crafted schema plus a large instance can validate superlinearly.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var input interactionResponseInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	it, found, err := h.interactions.GetInteraction(ctx, conversationID, interactionID)
	if err != nil {
		slog.Error("[Web] load interaction failed", "conversation", conversationID, "interaction", interactionID, "err", err)
		http.Error(w, "failed to load interaction", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "interaction not found", http.StatusNotFound)
		return
	}

	if !ownsConversation(it.UserID, session.UserID) {
		slog.Warn("[Web] interaction response from unauthorized user", "conversation", conversationID, "interaction", interactionID)
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Idempotent replay of an already-answered interaction (e.g. re-POST on reload).
	if it.Status != store.InteractionPending {
		writeJSON(w, http.StatusOK, interactionResponseAck{
			Status: string(it.Status),
			Action: renderableActionWireName(it.Response.GetAction()),
		})
		return
	}

	action, ok := parseResponseAction(input.Action)
	if !ok {
		http.Error(w, "invalid action", http.StatusBadRequest)
		return
	}
	// CANCEL is always permitted (the client always renders a dismiss); any other
	// action must be one the Renderable offered.
	if action != pb.RenderableAction_RENDERABLE_ACTION_CANCEL && !renderableOffersAction(it.Renderable, action) {
		http.Error(w, "action not allowed for this interaction", http.StatusBadRequest)
		return
	}

	resp := &pb.RenderableResponse{Id: interactionID, Action: action}
	switch action {
	case pb.RenderableAction_RENDERABLE_ACTION_SUBMIT:
		if len(input.Content) == 0 {
			http.Error(w, "content is required for submit", http.StatusBadRequest)
			return
		}
		sch, err := compileSchema(it.Renderable.GetDataSchemaJson())
		if err != nil {
			slog.Error("[Web] interaction schema compile failed", "conversation", conversationID, "interaction", interactionID, "err", err)
			http.Error(w, "interaction schema invalid", http.StatusInternalServerError)
			return
		}
		if err := validateInstance(sch, input.Content); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error":  "content does not match schema",
				"detail": err.Error(),
			})
			return
		}
		resp.ContentJson = string(input.Content)
	case pb.RenderableAction_RENDERABLE_ACTION_RESPOND:
		if strings.TrimSpace(input.Text) == "" {
			http.Error(w, "text is required for respond", http.StatusBadRequest)
			return
		}
		resp.Text = input.Text
	}

	recorded, wasRecorded, err := h.interactions.RecordInteractionResponse(ctx, conversationID, interactionID, resp)
	if err != nil {
		if errors.Is(err, store.ErrInteractionNotFound) {
			http.Error(w, "interaction not found", http.StatusNotFound)
			return
		}
		slog.Error("[Web] record interaction response failed", "conversation", conversationID, "interaction", interactionID, "err", err)
		http.Error(w, "failed to record response", http.StatusInternalServerError)
		return
	}
	// Lost the race to a concurrent responder: replay, don't re-deliver.
	if !wasRecorded {
		writeJSON(w, http.StatusOK, interactionResponseAck{
			Status: string(recorded.Status),
			Action: renderableActionWireName(recorded.Response.GetAction()),
		})
		return
	}

	switch action {
	case pb.RenderableAction_RENDERABLE_ACTION_RESPOND:
		// "Write your own reply" is a new message, not an in-turn answer: stash the
		// prose and cancel the agent's current turn. When that turn finalizes, the
		// agent-response END handler calls injectRespond to persist the note and
		// start a fresh turn with the prose — so the reply is a new turn, and the
		// two can't overlap. Deliberately do NOT re-arm the idle watchdog here: the
		// turn is being cancelled and its queued prose is delivered only on the
		// terminal chunk, so re-arming would let the reaper fire in that window and
		// drop the prose. The agent's own activity re-arms the watchdog if the cancel
		// produces output, and the follow-up turn arms its own on startTurn.
		if h.turns != nil {
			h.turns.setPendingRespond(conversationID, session.UserID, input.Text)
		}
		h.emitRenderableResponse(r.Context(), conversationID, session.UserID, &pb.RenderableResponse{
			Id:     interactionID,
			Action: pb.RenderableAction_RENDERABLE_ACTION_CANCEL,
		})
	case pb.RenderableAction_RENDERABLE_ACTION_SUBMIT, pb.RenderableAction_RENDERABLE_ACTION_DECLINE:
		// The agent resumes this same turn: re-arm the idle watchdog (suspended while
		// awaiting) so a hung post-answer agent is still reaped. The continuation is
		// a new message, not a continuation of the reply that preceded the
		// interaction, so reset the turn's text buffer first: that reply was already
		// flushed as its own row (enterAwaiting), and without this the continuation
		// would persist as "preamble + continuation" in one row. Then persist a ghost
		// note as the boundary — it records what the user answered and, as a
		// non-assistant row, makes the continuation a new bubble (the store appends a
		// fresh assistant row after a non-assistant tail, and the client opens a new
		// bubble on the note tail too).
		if h.turns != nil {
			h.turns.resume(conversationID)
			h.turns.startFreshBuffer(conversationID)
		}
		h.persistNote(ctx, conversationID, noteContent(action, it.Renderable, resp))
		h.emitRenderableResponse(r.Context(), conversationID, session.UserID, resp)
	default: // CANCEL — the turn aborts, no continuation and no record to keep.
		// Re-arm the idle watchdog so a hung post-cancel agent is reaped; nothing is
		// queued, so there is nothing for the reaper to drop here.
		if h.turns != nil {
			h.turns.resume(conversationID)
		}
		h.emitRenderableResponse(r.Context(), conversationID, session.UserID, resp)
	}
	writeJSON(w, http.StatusOK, interactionResponseAck{
		Status: string(recorded.Status),
		Action: renderableActionWireName(action),
	})
}

// injectRespond starts the follow-up turn for a "write your own reply": it
// records the user's prose as a ghost note (the boundary that splits the
// follow-up reply into its own bubble; server-injected, so not added
// optimistically) and forwards the prose to the agent as a new turn.
// autoResume-capable agents resume the suspended tool from the prose. Called from
// the agent-response END handler once the cancelled turn has reached idle, so the
// new turn can never overlap the old one.
func (h *Handlers) injectRespond(ctx context.Context, conversationID string, p *pendingRespond) {
	h.persistNote(ctx, conversationID, p.text)
	if h.msgHandler == nil {
		slog.Warn("[Web] no message handler; respond not delivered", "conversation", conversationID)
		return
	}
	if h.turns != nil {
		h.turns.startTurn(conversationID)
	}
	messageID := uuid.NewString()
	msg := &pb.Message{
		Id:        messageID,
		Timestamp: timestamppb.Now(),
		Platform:  "web",
		PlatformContext: &pb.PlatformContext{
			MessageId: messageID,
			ChannelId: conversationID,
			EventKind: pb.PlatformContext_EVENT_KIND_DM,
		},
		User:           &pb.User{Id: p.userID},
		Content:        p.text,
		ConversationId: conversationID,
	}
	if err := h.msgHandler(ctx, msg); err != nil {
		// The follow-up turn never reached the agent. The END handler withheld the
		// finish event so the SSE could stay open for this reply, so surface an error
		// to resolve the live stream (mirroring HandleSendMessage's forward-failure
		// path) rather than leaving the client's spinner hung until a reload.
		if h.turns != nil {
			h.turns.clear(conversationID)
		}
		if errors.Is(err, adapter.ErrNoAgentStream) {
			slog.Warn("[Web] no agent connected; respond not delivered", "conversation", conversationID)
			h.sendErrorEvent(conversationID, "AGENT_UNAVAILABLE", "The agent is not available right now. You can try sending again.")
		} else {
			slog.Error("[Web] forward respond message failed", "conversation", conversationID, "err", err)
			h.sendErrorEvent(conversationID, "INTERNAL_ERROR", "Failed to process message")
		}
	}
}

// intentToolPermission is the Renderable intent for a tool-approval ask (approve
// or deny a tool call), which reads as "Approved"/"Denied" rather than the
// form-oriented "Submitted"/"Declined".
const intentToolPermission = "tool_permission"

// persistNote records a ghost note for a resolved interaction and surfaces it to
// connected clients over SSE. The persisted row and the SSE event share an id so a
// reload reconciles to the same message. As a non-assistant, non-user row it both
// records what the user answered and acts as the boundary that splits the agent's
// continuation into its own bubble. No-op for empty content.
func (h *Handlers) persistNote(ctx context.Context, conversationID, content string) {
	if strings.TrimSpace(content) == "" {
		return
	}
	messageID := uuid.NewString()
	if h.chatStore != nil {
		msg, err := h.chatStore.AppendMessage(ctx, conversationID, "", "note", content, "")
		if err != nil {
			// A note that didn't persist must not be broadcast: a reload wouldn't
			// show it, and the missing row leaves the trailing store row the flushed
			// assistant preamble, so the continuation would update that row in place
			// rather than start a new bubble. The message cap is terminal
			// per-conversation state, not a real failure — log it at Debug like the
			// rest of the adapter rather than spamming ERROR.
			if errors.Is(err, sqlite.ErrMessageLimitReached) {
				slog.Debug("[Web] chat at message limit; note not persisted", "conversation", conversationID)
			} else {
				slog.Error("[Web] persist note failed", "conversation", conversationID, "err", err)
			}
			return
		}
		messageID = msg.ID
	}
	if h.connManager != nil {
		h.connManager.Broadcast(conversationID, NewNoteEvent(messageID, content, time.Now().UTC().Format(time.RFC3339)))
	}
}

// noteContent is the ghost-note text recorded when an interaction resolves: a
// compact record of what the user answered. A tool-permission ask reads as an
// approve/deny; a submitted form as its field values ("Answered · Date: … · …");
// a "write your own reply" as the prose itself. Returns "" for actions that leave
// no record (cancel), so the caller skips the note.
func noteContent(action pb.RenderableAction, r *pb.Renderable, resp *pb.RenderableResponse) string {
	isPermission := r.GetIntent() == intentToolPermission
	switch action {
	case pb.RenderableAction_RENDERABLE_ACTION_SUBMIT:
		if isPermission {
			return "Approved"
		}
		if answers := summarizeSubmission(resp.GetContentJson(), r.GetDataSchemaJson()); answers != "" {
			return "Answered · " + answers
		}
		return "Submitted"
	case pb.RenderableAction_RENDERABLE_ACTION_DECLINE:
		if isPermission {
			return "Denied"
		}
		return "Declined"
	case pb.RenderableAction_RENDERABLE_ACTION_RESPOND:
		return strings.TrimSpace(resp.GetText())
	default:
		return ""
	}
}

// summarizeSubmission renders a submitted form's values as a compact one-line
// record ("Date: 2027-06-23 · Attendees: 12"), preferring each field's schema
// title over the raw property key and formatting scalar values. Non-scalar fields
// (nested objects, arrays) are omitted. Returns "" when the content isn't a JSON
// object or has no scalar fields, so the caller falls back to a generic label.
func summarizeSubmission(contentJSON, schemaJSON string) string {
	var values map[string]json.RawMessage
	if json.Unmarshal([]byte(contentJSON), &values) != nil || len(values) == 0 {
		return ""
	}
	titles := schemaTitles(schemaJSON)
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := scalarString(values[k])
		if v == "" {
			continue
		}
		label := titles[k]
		if label == "" {
			label = humanizeKey(k)
		}
		parts = append(parts, label+": "+v)
	}
	return strings.Join(parts, " · ")
}

// schemaTitles maps each top-level property to its schema title, for humanizing
// field labels in a submission summary. Empty for a schema without titles.
func schemaTitles(schemaJSON string) map[string]string {
	var schema struct {
		Properties map[string]struct {
			Title string `json:"title"`
		} `json:"properties"`
	}
	titles := make(map[string]string)
	if json.Unmarshal([]byte(schemaJSON), &schema) != nil {
		return titles
	}
	for k, p := range schema.Properties {
		if p.Title != "" {
			titles[k] = p.Title
		}
	}
	return titles
}

// scalarString renders a JSON scalar (string, number, bool) as plain text for a
// submission summary. Non-scalars (object, array, null) return "" so the summary
// stays a one-line record rather than dumping nested JSON.
func scalarString(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "yes"
		}
		return "no"
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	default:
		return ""
	}
}

// humanizeKey turns a property key into a readable label: snake_case / kebab-case
// separators become spaces, camelCase boundaries are split, and the first letter
// is capitalized ("meetingDate" → "Meeting date", "attendee_count" → "Attendee
// count"). Used only when the schema provides no title for the field.
func humanizeKey(k string) string {
	var b strings.Builder
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c == '_' || c == '-':
			b.WriteByte(' ')
		case c >= 'A' && c <= 'Z' && i > 0 && k[i-1] >= 'a' && k[i-1] <= 'z':
			b.WriteByte(' ')
			b.WriteByte(c - 'A' + 'a')
		default:
			b.WriteByte(c)
		}
	}
	s := strings.TrimSpace(b.String())
	if s != "" && s[0] >= 'a' && s[0] <= 'z' {
		s = string(s[0]-'a'+'A') + s[1:]
	}
	return s
}

// emitRenderableResponse forwards a response to the local agent over the feedback
// channel. Delivery is best effort: with no agent stream the send is dropped, and
// there is no reconnect redelivery yet, so the agent's render() stays unresolved.
func (h *Handlers) emitRenderableResponse(ctx context.Context, conversationID, userID string, resp *pb.RenderableResponse) {
	if h.feedbackHandler == nil {
		slog.Debug("[Web] no feedback handler; dropping renderable response", "conversation", conversationID)
		return
	}
	fb := &pb.PlatformFeedback{
		ConversationId: conversationID,
		Timestamp:      timestamppb.Now(),
		Feedback:       &pb.PlatformFeedback_RenderableResponse{RenderableResponse: resp},
	}
	if userID != "" {
		fb.User = &pb.User{Id: userID}
	}
	if err := h.feedbackHandler(ctx, fb); err != nil {
		slog.Debug("[Web] renderable response not delivered to agent", "conversation", conversationID, "err", err)
	}
}

func parseResponseAction(s string) (pb.RenderableAction, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "submit":
		return pb.RenderableAction_RENDERABLE_ACTION_SUBMIT, true
	case "decline":
		return pb.RenderableAction_RENDERABLE_ACTION_DECLINE, true
	case "cancel":
		return pb.RenderableAction_RENDERABLE_ACTION_CANCEL, true
	case "respond":
		return pb.RenderableAction_RENDERABLE_ACTION_RESPOND, true
	default:
		// UNSUPPORTED is system-emitted only and cannot be sent by a client.
		return pb.RenderableAction_RENDERABLE_ACTION_UNSPECIFIED, false
	}
}

func renderableOffersAction(r *pb.Renderable, action pb.RenderableAction) bool {
	for _, a := range r.GetAllowedActions() {
		if a == action {
			return true
		}
	}
	return false
}

// noExternalRefLoader refuses every external $ref: interaction schemas are
// agent-authored (untrusted), and the library's default FileLoader would resolve
// a file:// $ref against the sidecar filesystem at compile time.
type noExternalRefLoader struct{}

func (noExternalRefLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external $ref not allowed: %s", url)
}

func compileSchema(schemaJSON string) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(schemaJSON))
	if err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(noExternalRefLoader{})
	const url = "mem://interaction/schema.json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, fmt.Errorf("add schema: %w", err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}
	return sch, nil
}

func validateInstance(sch *jsonschema.Schema, content json.RawMessage) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("parse content: %w", err)
	}
	return sch.Validate(inst)
}
