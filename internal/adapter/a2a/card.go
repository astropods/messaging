package a2a

import (
	"fmt"

	"github.com/astropods/messaging/internal/store"
)

// cardVersion is the agent version advertised on the card. Astropods has no
// per-agent semantic version to report, and A2A requires the field.
const cardVersion = "1.0.0"

// buildCard renders the agent card. Skills come from the tools the agent
// declared over its gRPC config stream, so a peer sees what the agent can
// actually do rather than a deploy-time guess. An agent that declares no tools
// still gets a card, just with no skills.
func buildCard(name, description, publicURL string, configStore *store.AgentConfigStore) Card {
	card := Card{
		ProtocolVersion:    protocolVersion,
		Name:               name,
		Description:        description,
		URL:                publicURL,
		PreferredTransport: "JSONRPC",
		Version:            cardVersion,
		Capabilities: Capabilities{
			Streaming:              false,
			PushNotifications:      false,
			StateTransitionHistory: false,
		},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             []Skill{},
	}
	if card.Description == "" {
		card.Description = fmt.Sprintf("The %s agent, running on Astropods.", name)
	}
	if configStore == nil {
		return card
	}
	cfg := configStore.Get()
	if cfg == nil {
		return card
	}
	for _, tool := range cfg.Tools {
		if tool == nil || tool.Name == "" {
			continue
		}
		skillName := tool.Title
		if skillName == "" {
			skillName = tool.Name
		}
		card.Skills = append(card.Skills, Skill{
			ID:          tool.Name,
			Name:        skillName,
			Description: tool.Description,
			Tags:        []string{"tool"},
		})
	}
	return card
}
