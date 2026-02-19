package adapter

// AdapterCapabilities defines what AI features a platform adapter supports
type AdapterCapabilities struct {
	// Streaming support
	SupportsStreaming bool // Platform supports incremental content updates

	// AI-specific features
	SupportsStatusUpdates    bool // Platform has status indicators (Slack: setStatus, Discord: typing)
	SupportsSuggestedPrompts bool // Platform supports suggested quick replies
	SupportsThreads          bool // Platform supports conversation threading
	SupportsTypingIndicator  bool // Platform has typing indicators

	// Platform limits
	MaxUpdateRateHz  float64 // Maximum update frequency (Hz), 0 = no limit
	MaxContentLength int     // Maximum message content length

	// Advanced features
	SupportsReactions bool // Platform supports emoji reactions
	SupportsCards     bool // Platform supports rich cards/embeds
}

// SlackCapabilities returns capabilities for Slack
func SlackCapabilities(aiFeatures bool) AdapterCapabilities {
	return AdapterCapabilities{
		SupportsStreaming:        false, // No content streaming (3s rate limit)
		SupportsStatusUpdates:    aiFeatures,
		SupportsSuggestedPrompts: aiFeatures,
		SupportsThreads:          true,
		SupportsTypingIndicator:  false, // Not available for AI assistants
		MaxUpdateRateHz:          0.33,  // 3 seconds minimum per Slack docs
		MaxContentLength:         4000,
		SupportsReactions:        true,
		SupportsCards:            true, // Block Kit support (future)
	}
}

// WebCapabilities returns capabilities for web browser clients via HTTP + SSE
func WebCapabilities() AdapterCapabilities {
	return AdapterCapabilities{
		SupportsStreaming:        true,  // SSE streaming
		SupportsStatusUpdates:    true,  // Status events via SSE
		SupportsSuggestedPrompts: true,  // Suggested prompts in SSE events
		SupportsThreads:          true,  // Conversation threading
		SupportsTypingIndicator:  false, // No typing indicator
		MaxUpdateRateHz:          0,     // No rate limit
		MaxContentLength:         0,     // No content length limit
		SupportsReactions:        false, // No reactions
		SupportsCards:            true,  // Rich cards/embeds
	}
}
