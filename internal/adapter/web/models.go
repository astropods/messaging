package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// ChatModels is the decoded CHAT_MODELS.
type ChatModels struct {
	Default ModelChoice   `json:"default"`
	Options []ModelChoice `json:"options"`
}

// ModelChoice is a model id under a models entry of astropods.yml.
type ModelChoice struct {
	Entry string `json:"entry"`
	Model string `json:"model"`
}

func ParseChatModels(raw string) (*ChatModels, error) {
	var cm ChatModels
	if err := json.Unmarshal([]byte(raw), &cm); err != nil {
		return nil, fmt.Errorf("decode CHAT_MODELS: %w", err)
	}
	if len(cm.Options) == 0 {
		return nil, errors.New("CHAT_MODELS has no options")
	}
	for _, o := range cm.Options {
		if o.Entry == "" || o.Model == "" {
			return nil, errors.New("CHAT_MODELS has an option without an entry or a model")
		}
	}
	if !slices.Contains(cm.Options, cm.Default) {
		return nil, errors.New("CHAT_MODELS default is not one of its options")
	}
	return &cm, nil
}

// WithChatModels lets chat users switch between these models.
func WithChatModels(cm *ChatModels) WebAdapterOption {
	return func(a *WebAdapter) {
		a.chatModels = cm
	}
}

// turnModel's ok is false when the deployment does not offer the requested model.
func (h *Handlers) turnModel(requested *ModelChoice) (model *pb.ModelChoice, ok bool) {
	if h.chatModels == nil {
		return nil, requested == nil
	}
	choice := h.chatModels.Default
	if requested != nil {
		if !slices.Contains(h.chatModels.Options, *requested) {
			return nil, false
		}
		choice = *requested
	}
	return &pb.ModelChoice{Entry: choice.Entry, Model: choice.Model}, true
}

func writeInvalidModel(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error":             "invalid_model",
		"error_description": "this deployment does not offer that model",
	})
}
