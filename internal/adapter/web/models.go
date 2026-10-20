package web

import (
	"net/http"
	"slices"
)

func (h *Handlers) agentModels() []string {
	if h.agentConfigStore == nil {
		return nil
	}
	return h.agentConfigStore.Get().GetModels()
}

// offersModel is true for an empty model, which leaves the choice to the agent.
func (h *Handlers) offersModel(model string) bool {
	return model == "" || slices.Contains(h.agentModels(), model)
}

func writeInvalidModel(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error":             "invalid_model",
		"error_description": "this agent does not offer that model",
	})
}
