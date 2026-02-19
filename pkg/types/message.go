package types

import "time"

// Attachment represents a file or media attachment
type Attachment struct {
	Type     string `json:"type"` // file, image, video, audio, link
	URL      string `json:"url"`
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

// ConversationContext stores metadata about a conversation
type ConversationContext struct {
	ConversationID string                 `json:"conversation_id"`
	Platform       string                 `json:"platform"`
	ChannelID      string                 `json:"channel_id"`
	ThreadID       string                 `json:"thread_id,omitempty"`
	UserID         string                 `json:"user_id"`
	CreatedAt      time.Time              `json:"created_at"`
	LastMessageAt  time.Time              `json:"last_message_at"`
	MessageCount   int                    `json:"message_count"`
	Metadata       map[string]interface{} `json:"metadata,omitempty"`
}

// HealthStatus represents the health status of the messaging service
type HealthStatus struct {
	Status   string            `json:"status"`
	Adapters map[string]string `json:"adapters"`
	Uptime   int64             `json:"uptime"`
}
