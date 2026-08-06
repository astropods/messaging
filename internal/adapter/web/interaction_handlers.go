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
	// CANCEL is always allowed (client always offers dismiss); other actions must be offered by the Renderable.
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
		// New turn, not an in-turn answer: queue the prose and cancel; the END handler injects it. No watchdog re-arm — a reap in that window would drop the queued prose.
		if h.turns != nil {
			h.turns.setPendingRespond(conversationID, session.UserID, input.Text)
		}
		h.emitRenderableResponse(r.Context(), conversationID, session.UserID, &pb.RenderableResponse{
			Id:     interactionID,
			Action: pb.RenderableAction_RENDERABLE_ACTION_CANCEL,
		})
	case pb.RenderableAction_RENDERABLE_ACTION_SUBMIT, pb.RenderableAction_RENDERABLE_ACTION_DECLINE:
		// Agent resumes this turn: re-arm the watchdog, reset the buffer so the continuation isn't glued to the flushed preamble, and persist the note as the new-bubble boundary.
		if h.turns != nil {
			h.turns.resume(conversationID)
			h.turns.startFreshBuffer(conversationID)
		}
		h.persistNote(ctx, conversationID, noteContent(action, it.Renderable, resp))
		h.emitRenderableResponse(r.Context(), conversationID, session.UserID, resp)
	default: // CANCEL — abort the turn; re-arm the watchdog to reap a hung post-cancel agent.
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

// injectRespond starts the follow-up turn for a "write your own reply": records the prose as a note, then forwards it as a fresh turn. Called from the END handler after the cancelled turn finalizes, so the two never overlap.
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
		// END withheld finish to keep the SSE open for this reply, so a failed forward must surface an error (as HandleSendMessage does) rather than hang the stream.
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

// intentToolPermission marks a tool-approval ask, which reads as Approved/Denied rather than Submitted/Declined.
const intentToolPermission = "tool_permission"

// persistNote records a resolved interaction as a ghost note and broadcasts it; the row and SSE event share an id so a reload reconciles. No-op for empty content.
func (h *Handlers) persistNote(ctx context.Context, conversationID, content string) {
	if strings.TrimSpace(content) == "" {
		return
	}
	messageID := uuid.NewString()
	if h.chatStore != nil {
		msg, err := h.chatStore.AppendMessage(ctx, conversationID, "", "note", content, "")
		if err != nil {
			// Don't broadcast a note that didn't persist: a reload wouldn't show it and the continuation would clobber the preamble row. The cap is expected — log at Debug, not ERROR.
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

// noteContent is the ghost-note text for a resolved interaction: approve/deny, a submitted-form summary, or the prose. "" for actions with no record (cancel).
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

// summarizeSubmission renders a submitted form's scalar fields as a one-line record ("Date: … · Attendees: 12"), preferring schema titles. "" when nothing scalar to show.
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

// schemaTitles maps each top-level property to its schema title (empty when the schema has none).
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

// scalarString renders a JSON scalar as text (bools as yes/no); non-scalars return "".
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

// humanizeKey turns a property key into a label ("meetingDate" → "Meeting date"); used only when the schema has no title.
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

// emitRenderableResponse forwards a response to the agent (best effort: dropped if no stream; no reconnect redelivery yet).
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

// noExternalRefLoader refuses external $refs: agent-authored schemas are untrusted and the default loader would read file:// refs off the sidecar FS.
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
