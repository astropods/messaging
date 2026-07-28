package web

import (
	"encoding/json"
	"fmt"
	"log/slog"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// SSE Event types matching playground pattern
const (
	EventConnected      = "connected"
	EventChunk          = "chunk"
	EventStatus         = "status"
	EventStepStart      = "step-start"
	EventStepEnd        = "step-end"
	EventReasoningStart = "reasoning-start"
	EventReasoningDelta = "reasoning-delta"
	EventReasoningEnd   = "reasoning-end"
	EventFinish         = "finish"
	EventError          = "error"
	EventHeartbeat      = "heartbeat"
	EventPrompts        = "prompts"
	EventTranscript     = "transcript"
	EventInteraction    = "interaction"
)

// SSEEvent represents a Server-Sent Event
type SSEEvent struct {
	Event string // Event type
	ID    string // Optional event ID
	Data  string // JSON data
	Retry int    // Optional retry interval in ms
}

// Format returns the SSE wire format
func (e SSEEvent) Format() string {
	result := ""
	if e.ID != "" {
		result += fmt.Sprintf("id: %s\n", e.ID)
	}
	if e.Event != "" {
		result += fmt.Sprintf("event: %s\n", e.Event)
	}
	if e.Retry > 0 {
		result += fmt.Sprintf("retry: %d\n", e.Retry)
	}
	result += fmt.Sprintf("data: %s\n\n", e.Data)
	return result
}

// ConnectedEventData represents the data for a connected event
type ConnectedEventData struct {
	Type           string `json:"type"`
	ConversationID string `json:"conversation_id"`
	ConnectionID   string `json:"connection_id"`
}

// ChunkEventData represents the data for a content chunk event
type ChunkEventData struct {
	Type              string `json:"type"`
	Content           string `json:"content"`
	ChunkType         string `json:"chunk_type"` // start, delta, end, replace
	ResponseID        string `json:"response_id,omitempty"`
	PlatformMessageID string `json:"platform_message_id,omitempty"`
	// Attachments carries agent-produced files, populated only on the END chunk.
	Attachments []chatAttachment `json:"attachments,omitempty"`
}

// StatusEventData represents the data for a status update event
type StatusEventData struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Emoji   string `json:"emoji,omitempty"`
}

// StepEventData represents the data for step start/end events
type StepEventData struct {
	Type    string `json:"type"`
	StepID  string `json:"step_id"`
	Name    string `json:"name,omitempty"`
	Details string `json:"details,omitempty"`
}

// ErrorEventData represents the data for an error event
type ErrorEventData struct {
	Type      string `json:"type"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Details   string `json:"details,omitempty"`
	Retryable bool   `json:"retryable"`
}

// FinishEventData represents the data for a finish event
type FinishEventData struct {
	Type       string `json:"type"`
	ResponseID string `json:"response_id,omitempty"`
}

// PromptsEventData represents the data for suggested prompts
type PromptsEventData struct {
	Type    string       `json:"type"`
	Prompts []PromptData `json:"prompts"`
}

// PromptData represents a single suggested prompt
type PromptData struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Message     string `json:"message"`
	Description string `json:"description,omitempty"`
}

// NewConnectedEvent creates a connected SSE event
func NewConnectedEvent(conversationID, connectionID string) SSEEvent {
	data := ConnectedEventData{
		Type:           "connected",
		ConversationID: conversationID,
		ConnectionID:   connectionID,
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		slog.Error(fmt.Sprintf("[Web] Error marshaling connected event: %v", err))
	}
	return SSEEvent{
		Event: EventConnected,
		Data:  string(jsonData),
	}
}

// NewChunkEvent creates a content chunk SSE event from a protobuf ContentChunk.
// attachments carries agent-produced files (resolved to the canonical shape by
// the caller) and is expected only on the END chunk.
func NewChunkEvent(chunk *pb.ContentChunk, responseID string, attachments []chatAttachment) SSEEvent {
	chunkType := "delta"
	switch chunk.Type {
	case pb.ContentChunk_START:
		chunkType = "start"
	case pb.ContentChunk_DELTA:
		chunkType = "delta"
	case pb.ContentChunk_END:
		chunkType = "end"
	case pb.ContentChunk_REPLACE:
		chunkType = "replace"
	}

	data := ChunkEventData{
		Type:              "chunk",
		Content:           chunk.Content,
		ChunkType:         chunkType,
		ResponseID:        responseID,
		PlatformMessageID: chunk.PlatformMessageId,
		Attachments:       attachments,
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		slog.Error(fmt.Sprintf("[Web] Error marshaling chunk event: %v", err))
	}
	return SSEEvent{
		Event: EventChunk,
		Data:  string(jsonData),
	}
}

// NewStatusEvent creates a status update SSE event from a protobuf StatusUpdate
func NewStatusEvent(status *pb.StatusUpdate) SSEEvent {
	statusStr := status.Status.String()
	data := StatusEventData{
		Type:    "status",
		Status:  statusStr,
		Message: status.CustomMessage,
		Emoji:   status.Emoji,
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		slog.Error(fmt.Sprintf("[Web] Error marshaling status event: %v", err))
	}
	return SSEEvent{
		Event: EventStatus,
		Data:  string(jsonData),
	}
}

// NewStepStartEvent creates a step start SSE event
func NewStepStartEvent(stepID, name, details string) SSEEvent {
	data := StepEventData{
		Type:    "step-start",
		StepID:  stepID,
		Name:    name,
		Details: details,
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		slog.Error(fmt.Sprintf("[Web] Error marshaling step-start event: %v", err))
	}
	return SSEEvent{
		Event: EventStepStart,
		Data:  string(jsonData),
	}
}

// NewStepEndEvent creates a step end SSE event
func NewStepEndEvent(stepID string) SSEEvent {
	data := StepEventData{
		Type:   "step-end",
		StepID: stepID,
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		slog.Error(fmt.Sprintf("[Web] Error marshaling step-end event: %v", err))
	}
	return SSEEvent{
		Event: EventStepEnd,
		Data:  string(jsonData),
	}
}

// NewErrorEvent creates an error SSE event from a protobuf ErrorResponse
func NewErrorEvent(pbErr *pb.ErrorResponse) SSEEvent {
	data := ErrorEventData{
		Type:      "error",
		Code:      pbErr.Code.String(),
		Message:   pbErr.Message,
		Details:   pbErr.Details,
		Retryable: pbErr.Retryable,
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		slog.Error(fmt.Sprintf("[Web] Error marshaling error event: %v", err))
	}
	return SSEEvent{
		Event: EventError,
		Data:  string(jsonData),
	}
}

// NewErrorEventFromMessage creates an error SSE event from a message string
func NewErrorEventFromMessage(code, message string, retryable bool) SSEEvent {
	data := ErrorEventData{
		Type:      "error",
		Code:      code,
		Message:   message,
		Retryable: retryable,
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		slog.Error(fmt.Sprintf("[Web] Error marshaling error event: %v", err))
	}
	return SSEEvent{
		Event: EventError,
		Data:  string(jsonData),
	}
}

// NewFinishEvent creates a finish SSE event
func NewFinishEvent(responseID string) SSEEvent {
	data := FinishEventData{
		Type:       "finish",
		ResponseID: responseID,
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		slog.Error(fmt.Sprintf("[Web] Error marshaling finish event: %v", err))
	}
	return SSEEvent{
		Event: EventFinish,
		Data:  string(jsonData),
	}
}

// TranscriptEventData represents the data for a transcript event
type TranscriptEventData struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	MessageID string `json:"message_id,omitempty"`
	Language  string `json:"language,omitempty"`
}

// NewTranscriptEvent creates a transcript SSE event from a protobuf Transcript
func NewTranscriptEvent(transcript *pb.Transcript) SSEEvent {
	data := TranscriptEventData{
		Type:      "transcript",
		Text:      transcript.Text,
		MessageID: transcript.MessageId,
		Language:  transcript.Language,
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		slog.Error(fmt.Sprintf("[Web] Error marshaling transcript event: %v", err))
	}
	return SSEEvent{
		Event: EventTranscript,
		Data:  string(jsonData),
	}
}

// InteractionEventData is the SSE payload for a blocking interaction: schema and
// value re-embedded as JSON objects, actions/kind lowercased for the client.
type InteractionEventData struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Message    string          `json:"message"`
	DataSchema json.RawMessage `json:"dataSchema"`
	Value      json.RawMessage `json:"value,omitempty"`
	Actions    []string        `json:"actions"`
	Intent     string          `json:"intent,omitempty"`
}

// NewInteractionEvent builds an interaction SSE event, erroring on invalid
// data_schema_json or value_json so a malformed Renderable is never emitted.
func NewInteractionEvent(r *pb.Renderable) (SSEEvent, error) {
	data, err := interactionEventData(r)
	if err != nil {
		return SSEEvent{}, err
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		return SSEEvent{}, fmt.Errorf("renderable %q: marshal interaction event: %w", r.GetId(), err)
	}
	return SSEEvent{Event: EventInteraction, Data: string(jsonData)}, nil
}

// interactionEventData builds the client interaction shape from a Renderable
// (schema/value as JSON objects, actions lowercased); errors on invalid JSON or a
// schema that won't compile. Shared by the SSE event and the conversation-fetch
// pending queue, so both validate identically.
func interactionEventData(r *pb.Renderable) (InteractionEventData, error) {
	schema := json.RawMessage(r.GetDataSchemaJson())
	if !json.Valid(schema) {
		return InteractionEventData{}, fmt.Errorf("renderable %q: invalid data_schema_json", r.GetId())
	}
	// Reject a schema that won't compile (e.g. a hostile external $ref) before the client sees it.
	if _, err := compileSchema(r.GetDataSchemaJson()); err != nil {
		return InteractionEventData{}, fmt.Errorf("renderable %q: %w", r.GetId(), err)
	}

	var value json.RawMessage
	if v := r.GetValueJson(); v != "" {
		if !json.Valid([]byte(v)) {
			return InteractionEventData{}, fmt.Errorf("renderable %q: invalid value_json", r.GetId())
		}
		value = json.RawMessage(v)
	}

	actions := make([]string, 0, len(r.GetAllowedActions()))
	for _, a := range r.GetAllowedActions() {
		if name := renderableActionWireName(a); name != "" {
			actions = append(actions, name)
		}
	}

	return InteractionEventData{
		Type:       "interaction",
		ID:         r.GetId(),
		Kind:       renderKindWireName(r.GetKind()),
		Message:    r.GetMessage(),
		DataSchema: schema,
		Value:      value,
		Actions:    actions,
		Intent:     r.GetIntent(),
	}, nil
}

// NewPromptsEvent creates a suggested prompts SSE event from a protobuf SuggestedPrompts
func NewPromptsEvent(prompts *pb.SuggestedPrompts) SSEEvent {
	promptsData := make([]PromptData, 0, len(prompts.Prompts))
	for _, p := range prompts.Prompts {
		promptsData = append(promptsData, PromptData{
			ID:          p.Id,
			Title:       p.Title,
			Message:     p.Message,
			Description: p.Description,
		})
	}
	data := PromptsEventData{
		Type:    "prompts",
		Prompts: promptsData,
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		slog.Error(fmt.Sprintf("[Web] Error marshaling prompts event: %v", err))
	}
	return SSEEvent{
		Event: EventPrompts,
		Data:  string(jsonData),
	}
}
