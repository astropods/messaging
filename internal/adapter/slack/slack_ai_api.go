package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const (
	slackAPIBaseURL = "https://slack.com/api"
)

// SlackAIClient handles calls to Slack AI APIs that aren't in the slack-go library yet
type SlackAIClient struct {
	botToken   string
	httpClient *http.Client
	baseURL    string // defaults to slackAPIBaseURL
}

// NewSlackAIClient creates a new Slack AI API client
func NewSlackAIClient(botToken string) *SlackAIClient {
	return &SlackAIClient{
		botToken:   botToken,
		httpClient: &http.Client{},
		baseURL:    slackAPIBaseURL,
	}
}

// SetThreadStatus sets the status for an assistant thread
// https://api.slack.com/methods/assistant.threads.setStatus
func (c *SlackAIClient) SetThreadStatus(ctx context.Context, channelID, threadTS, status, emoji string) error {
	reqBody := map[string]interface{}{
		"channel_id": channelID,
		"thread_ts":  threadTS,
		"status":     status,
	}

	if emoji != "" {
		reqBody["status_emoji"] = emoji
	}

	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}

	if err := c.postJSON(ctx, "assistant.threads.setStatus", reqBody, &result); err != nil {
		return err
	}

	if !result.OK {
		return fmt.Errorf("slack API error: %s", result.Error)
	}

	return nil
}

// SuggestedPrompt represents a suggested prompt for the user
type SuggestedPrompt struct {
	Title   string `json:"title"`
	Message string `json:"message"`
}

// SetSuggestedPrompts sets suggested prompts for an assistant thread
// https://api.slack.com/methods/assistant.threads.setSuggestedPrompts
func (c *SlackAIClient) SetSuggestedPrompts(ctx context.Context, channelID, threadTS string, prompts []SuggestedPrompt) error {
	reqBody := map[string]interface{}{
		"channel_id": channelID,
		"thread_ts":  threadTS,
		"prompts":    prompts,
	}

	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}

	if err := c.postJSON(ctx, "assistant.threads.setSuggestedPrompts", reqBody, &result); err != nil {
		return err
	}

	if !result.OK {
		return fmt.Errorf("slack API error: %s", result.Error)
	}

	return nil
}

// SetTitle sets the title for an assistant thread
// https://api.slack.com/methods/assistant.threads.setTitle
func (c *SlackAIClient) SetTitle(ctx context.Context, channelID, threadTS, title string) error {
	reqBody := map[string]interface{}{
		"channel_id": channelID,
		"thread_ts":  threadTS,
		"title":      title,
	}

	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}

	if err := c.postJSON(ctx, "assistant.threads.setTitle", reqBody, &result); err != nil {
		return err
	}

	if !result.OK {
		return fmt.Errorf("slack API error: %s", result.Error)
	}

	return nil
}

// PostMessageWithFeedback posts a message with feedback buttons
// https://api.slack.com/methods/chat.postMessage
func (c *SlackAIClient) PostMessageWithFeedback(ctx context.Context, channelID, content, threadID string) (string, error) {
	// Build blocks with feedback buttons
	blocks := []map[string]interface{}{
		{
			"type": "section",
			"text": map[string]interface{}{
				"type": "mrkdwn",
				"text": content,
			},
		},
		{
			"type": "context_actions",
			"elements": []map[string]interface{}{
				{
					"type":      "feedback_buttons",
					"action_id": "feedback_buttons",
					"positive_button": map[string]interface{}{
						"text": map[string]interface{}{
							"type": "plain_text",
							"text": "👍",
						},
						"value": "positive_feedback",
					},
					"negative_button": map[string]interface{}{
						"text": map[string]interface{}{
							"type": "plain_text",
							"text": "👎",
						},
						"value": "negative_feedback",
					},
				},
			},
		},
	}

	payload := map[string]interface{}{
		"channel": channelID,
		"text":    content,
		"blocks":  blocks,
	}

	if threadID != "" {
		payload["thread_ts"] = threadID
	}

	fmt.Printf("[SlackAI] Posting message with feedback buttons to channel %s\n", channelID)

	var result struct {
		OK        bool   `json:"ok"`
		Error     string `json:"error,omitempty"`
		Timestamp string `json:"ts,omitempty"`
	}

	if err := c.postJSON(ctx, "chat.postMessage", payload, &result); err != nil {
		fmt.Printf("[SlackAI] ERROR posting message: %v\n", err)
		return "", err
	}

	if !result.OK {
		fmt.Printf("[SlackAI] Slack API returned error: %s\n", result.Error)
		return "", fmt.Errorf("slack API error: %s", result.Error)
	}

	fmt.Printf("[SlackAI] ✓ Message posted successfully: timestamp=%s\n", result.Timestamp)
	return result.Timestamp, nil
}

// postJSON makes a POST request to a Slack API endpoint with JSON body
func (c *SlackAIClient) postJSON(ctx context.Context, method string, body interface{}, result interface{}) error {
	url := fmt.Sprintf("%s/%s", c.baseURL, method)

	// Marshal request body
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.botToken))

	// Execute request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	// Check HTTP status
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP error %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse response
	if err := json.Unmarshal(respBody, result); err != nil {
		return fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return nil
}
