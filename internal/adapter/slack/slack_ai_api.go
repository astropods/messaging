package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	slackAPIBaseURL = "https://slack.com/api"
)

// SlackAIClient handles calls to Slack AI APIs that aren't in the slack-go library yet
type SlackAIClient struct {
	botToken   string
	devMode    bool
	agentID    string
	httpClient *http.Client
	baseURL    string // defaults to slackAPIBaseURL
}

// NewSlackAIClient creates a new Slack AI API client. agentID is the value of
// ASTRO_AGENT_ID at startup (may be empty) and is rendered in the message
// footer so users can identify which agent replied.
func NewSlackAIClient(botToken string, devMode bool, agentID string) *SlackAIClient {
	return &SlackAIClient{
		botToken:   botToken,
		devMode:    devMode,
		agentID:    agentID,
		httpClient: &http.Client{Timeout: 30 * time.Second},
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

// maxMarkdownBlockChars caps the text in a single Slack markdown block. Slack
// rejects a markdown block over 12000 chars (msg_too_long), and a whole
// message's blocks have their own size limit (msg_blocks_too_long), so we stay
// well under both and fan out across messages, leaving headroom on the last
// message for the footer + feedback widgets.
const maxMarkdownBlockChars = 10000

// PostMessageWithFeedback posts an agent reply as native Slack markdown
// block(s), with feedback widgets attached to the final message.
// https://api.slack.com/methods/chat.postMessage
//
// The reply text is handed to Slack as Markdown and Slack does the rendering
// (headings, bold, links, lists, code blocks, tables). Long replies are split
// into ≤maxMarkdownBlockChars chunks on line boundaries — never inside a fenced
// code block — and each chunk is posted as its own message in the thread, so we
// stay under Slack's per-block and per-message size limits. The footer and
// feedback widgets ride on the last message. Returns the first message's ts.
func (c *SlackAIClient) PostMessageWithFeedback(ctx context.Context, channelID, content, threadID string) (string, error) {
	chunks := chunkMarkdown(content, maxMarkdownBlockChars)
	trailing := c.feedbackTrailingBlocks()

	var firstTS string
	for i, chunk := range chunks {
		blocks := []map[string]interface{}{markdownBlock(chunk)}
		if i == len(chunks)-1 {
			blocks = append(blocks, trailing...)
		}

		// The first message carries the full reply text as the notification
		// fallback; continuations are marked so previews read sensibly.
		text := content
		if i > 0 {
			text = "(continued)"
		}

		payload := map[string]interface{}{
			"channel": channelID,
			"text":    text,
			"blocks":  blocks,
		}
		if threadID != "" {
			payload["thread_ts"] = threadID
		}

		slog.Debug("[SlackAI] Posting message", "channel", channelID, "part", i+1, "parts", len(chunks))

		var result struct {
			OK        bool   `json:"ok"`
			Error     string `json:"error,omitempty"`
			Timestamp string `json:"ts,omitempty"`
		}

		if err := c.postJSON(ctx, "chat.postMessage", payload, &result); err != nil {
			slog.Error("[SlackAI] Error posting message", "err", err, "part", i+1)
			return firstTS, err
		}
		if !result.OK {
			slog.Error("[SlackAI] Slack API returned error", "error", result.Error, "part", i+1)
			return firstTS, fmt.Errorf("slack API error: %s", result.Error)
		}

		if i == 0 {
			firstTS = result.Timestamp
		}
	}

	slog.Debug("[SlackAI] Message posted successfully", "timestamp", firstTS, "parts", len(chunks))
	return firstTS, nil
}

// markdownBlock builds a Slack markdown block. Slack renders the Markdown
// natively, so no client-side conversion to mrkdwn is needed.
func markdownBlock(text string) map[string]interface{} {
	return map[string]interface{}{
		"type": "markdown",
		"text": text,
	}
}

// feedbackTrailingBlocks builds the optional footer plus the two feedback
// affordances that close out a reply:
//  1. Native Slack AI thumbs widget (context_actions/feedback_buttons) —
//     one-click 👍/👎 that the platform renders with built-in styling.
//  2. A 💬 button in an actions block — opens a modal where the user can
//     leave free-form text. Kept separate because feedback_buttons only
//     accepts positive_button + negative_button and rejects a third option.
//
// Both flow through handleBlockActions and end up calling forwardFeedback, so
// the agent developer sees a single on_feedback callback regardless of path.
// These blocks ride on the last message of a fanned-out reply.
func (c *SlackAIClient) feedbackTrailingBlocks() []map[string]interface{} {
	blocks := make([]map[string]interface{}, 0, 3)

	if footer := buildFooterText(c.devMode, c.agentID); footer != "" {
		blocks = append(blocks, map[string]interface{}{
			"type": "context",
			"elements": []map[string]interface{}{
				{
					"type": "mrkdwn",
					"text": footer,
				},
			},
		})
	}

	blocks = append(blocks, map[string]interface{}{
		"type": "context_actions",
		"elements": []map[string]interface{}{
			{
				"type":      "feedback_buttons",
				"action_id": feedbackButtonsActionID,
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
	})

	blocks = append(blocks, map[string]interface{}{
		"type":     "actions",
		"block_id": feedbackCommentBlockID,
		"elements": []map[string]interface{}{
			{
				"type":      "button",
				"action_id": feedbackCommentActionID,
				"text": map[string]interface{}{
					"type":  "plain_text",
					"text":  "💬 Comment",
					"emoji": true,
				},
				"value": "open_comment_modal",
			},
		},
	})

	return blocks
}

// buildFooterText returns the context-block footer text for a Slack message,
// or "" if no footer should be rendered. In dev mode the message is flagged
// explicitly; outside dev mode the footer only appears when agentID is
// set so agents identify themselves to the user.
func buildFooterText(devMode bool, agentID string) string {
	if devMode {
		footer := ":test_tube: Sent from dev environment"
		if agentID != "" {
			footer += fmt.Sprintf(" — Agent ID: %s", agentID)
		}
		return footer
	}
	if agentID != "" {
		return fmt.Sprintf("Agent ID: %s", agentID)
	}
	return ""
}

// chunkMarkdown splits a Markdown reply into pieces of at most maxChars,
// breaking only on line boundaries that fall outside a fenced code block so a
// ```code``` block is never cut in two (which would drop its closing fence and
// reinterpret the rest as Markdown). A single fenced block longer than maxChars
// is hard-split as a last resort (rare; breaks the fence but avoids a
// msg_too_long rejection).
func chunkMarkdown(s string, maxChars int) []string {
	if len(s) <= maxChars {
		return []string{s}
	}

	var (
		out     []string
		current string
	)
	for _, atom := range markdownAtoms(s) {
		// An atom (a whole code block) larger than the budget can't be packed —
		// flush, then hard-split it on its own.
		if len(atom) > maxChars {
			if current != "" {
				out = append(out, current)
				current = ""
			}
			out = append(out, splitIntoChunks(atom, maxChars)...)
			continue
		}
		if current != "" && len(current)+1+len(atom) > maxChars {
			out = append(out, current)
			current = ""
		}
		if current != "" {
			current += "\n" + atom
		} else {
			current = atom
		}
	}
	if current != "" {
		out = append(out, current)
	}
	return out
}

// markdownAtoms splits text into indivisible chunking units: each fenced code
// block is one atom (kept whole, fences included), and every other line is its
// own atom. Joining the atoms back with "\n" reproduces the input.
func markdownAtoms(s string) []string {
	var (
		atoms   []string
		fence   []string
		inFence bool
	)
	for _, line := range strings.Split(s, "\n") {
		isFence := strings.HasPrefix(strings.TrimSpace(line), "```")
		if inFence {
			fence = append(fence, line)
			if isFence {
				atoms = append(atoms, strings.Join(fence, "\n"))
				fence = nil
				inFence = false
			}
			continue
		}
		if isFence {
			inFence = true
			fence = []string{line}
			continue
		}
		atoms = append(atoms, line)
	}
	if len(fence) > 0 { // unterminated fence — emit what we accumulated
		atoms = append(atoms, strings.Join(fence, "\n"))
	}
	return atoms
}

// splitIntoChunks breaks text into pieces of at most maxLen characters,
// splitting on newline boundaries so markdown formatting isn't broken mid-line.
func splitIntoChunks(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}

		// Find the last newline within the limit
		cut := strings.LastIndex(text[:maxLen], "\n")
		if cut < 1 {
			// No usable newline — hard-split at maxLen
			cut = maxLen
		}

		chunk := text[:cut]
		text = text[cut:]

		// Trim the newline between chunks
		if len(text) > 0 && text[0] == '\n' {
			text = text[1:]
		}

		if chunk != "" {
			chunks = append(chunks, chunk)
		}
	}

	return chunks
}

// PostBlocks posts a message containing only Slack Block Kit blocks (no feedback widget).
// blocksJSON must be a JSON array of block objects, e.g. `[{"type":"section",...}]`.
// This is used for agent-generated rich cards (CardAttachment).
func (c *SlackAIClient) PostBlocks(ctx context.Context, channelID, threadTS, blocksJSON string) error {
	// Validate that the input is a JSON array before sending to Slack.
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(blocksJSON), &raw); err != nil {
		return fmt.Errorf("platform_card_json is not a valid JSON array: %w", err)
	}

	payload := map[string]interface{}{
		"channel": channelID,
		"blocks":  raw,
	}
	if threadTS != "" {
		payload["thread_ts"] = threadTS
	}

	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	if err := c.postJSON(ctx, "chat.postMessage", payload, &result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("slack API error: %s", result.Error)
	}
	return nil
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
	resp, err := c.httpClient.Do(req) //nolint:gosec // URL is constructed from trusted config (baseURL defaults to slack API)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

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
