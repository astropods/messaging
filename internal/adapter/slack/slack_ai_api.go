package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
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

// slackMaxBlocksPerMessage is Slack's hard cap on the number of blocks a single
// chat.postMessage may carry. Replies that exceed it are fanned out across
// multiple messages in the same thread.
const slackMaxBlocksPerMessage = 50

// PostMessageWithFeedback posts an agent reply, with feedback widgets attached.
// https://api.slack.com/methods/chat.postMessage
//
// Long replies are split into section blocks (Slack caps section text at 3000
// chars) which can easily exceed the 50-block-per-message limit. Rather than
// truncate — which silently dropped the middle of big answers — the blocks are
// fanned out across multiple messages in the same thread.
// The footer and feedback widgets ride on the final message so the reply ends
// with a single set of controls. Returns the timestamp of the first message.
func (c *SlackAIClient) PostMessageWithFeedback(ctx context.Context, channelID, content, threadID string) (string, error) {
	// Convert standard Markdown to Slack mrkdwn before building blocks.
	mrkdwn := markdownToMrkdwn(content)

	// Slack section blocks have a 3000 char text limit. Split long content
	// into multiple sections so messages with tables or lists aren't rejected.
	var sections []map[string]interface{}
	for _, chunk := range splitIntoChunks(mrkdwn, 3000) {
		sections = append(sections, map[string]interface{}{
			"type": "section",
			"text": map[string]interface{}{
				"type": "mrkdwn",
				"text": chunk,
			},
		})
	}

	// Footer + feedback widgets must stay together on the final message.
	trailing := c.feedbackTrailingBlocks()

	messages := batchBlocks(sections, trailing, slackMaxBlocksPerMessage)

	var firstTS string
	for i, blocks := range messages {
		// The first message carries the full reply text as the notification
		// fallback; continuations are marked so previews read sensibly.
		text := mrkdwn
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

		slog.Debug("[SlackAI] Posting message", "channel", channelID, "part", i+1, "parts", len(messages))

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

	slog.Debug("[SlackAI] Message posted successfully", "timestamp", firstTS, "parts", len(messages))
	return firstTS, nil
}

// feedbackTrailingBlocks builds the optional footer plus the two feedback
// affordances that close out a reply:
//   1. Native Slack AI thumbs widget (context_actions/feedback_buttons) —
//      one-click 👍/👎 that the platform renders with built-in styling.
//   2. A 💬 button in an actions block — opens a modal where the user can
//      leave free-form text. Kept separate because feedback_buttons only
//      accepts positive_button + negative_button and rejects a third option.
// Both flow through handleBlockActions and end up calling forwardFeedback, so
// the agent developer sees a single on_feedback callback regardless of path.
// These blocks stay together on the last message of a fanned-out reply.
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

// batchBlocks splits content blocks into groups no larger than maxBlocks,
// keeping the trailing blocks (footer + feedback widgets) together on the final
// group — appended to the last content group when they fit, otherwise sent as
// their own final group. Always returns at least one group; when there is no
// content the trailing blocks are the only group.
func batchBlocks(content, trailing []map[string]interface{}, maxBlocks int) [][]map[string]interface{} {
	var groups [][]map[string]interface{}
	for i := 0; i < len(content); i += maxBlocks {
		end := i + maxBlocks
		if end > len(content) {
			end = len(content)
		}
		group := make([]map[string]interface{}, end-i)
		copy(group, content[i:end])
		groups = append(groups, group)
	}

	if len(groups) == 0 {
		return [][]map[string]interface{}{trailing}
	}

	last := groups[len(groups)-1]
	if len(last)+len(trailing) <= maxBlocks {
		groups[len(groups)-1] = append(last, trailing...)
	} else {
		groups = append(groups, trailing)
	}
	return groups
}

var (
	reMarkdownLink = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reBoldDouble   = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reHeading      = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	reTableRow     = regexp.MustCompile(`(?m)^\|(.+)\|$`)
	reTableSep     = regexp.MustCompile(`(?m)^\|[-| :]+\|$`)
)

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

// markdownToMrkdwn converts standard Markdown to Slack mrkdwn.
// Handles links, bold, headings, and tables (rendered as code blocks).
func markdownToMrkdwn(md string) string {
	md = convertTables(md)
	md = convertLinks(md)
	md = convertBold(md)
	md = convertHeadings(md)
	return md
}

// convertLinks converts [text](url) → <url|text>
func convertLinks(text string) string {
	return reMarkdownLink.ReplaceAllString(text, "<$2|$1>")
}

// convertBold converts **bold** → *bold*
func convertBold(text string) string {
	return reBoldDouble.ReplaceAllString(text, "*$1*")
}

// convertHeadings converts ## Heading → *Heading*
func convertHeadings(text string) string {
	return reHeading.ReplaceAllStringFunc(text, func(m string) string {
		sub := reHeading.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		return "*" + strings.TrimSpace(sub[1]) + "*"
	})
}

// convertTables finds Markdown table blocks and wraps them in code fences
// so Slack renders them as pre-formatted text with alignment preserved.
func convertTables(text string) string {
	lines := strings.Split(text, "\n")
	var result []string
	inTable := false

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		isTableLine := reTableRow.MatchString(line) || reTableSep.MatchString(line)

		if isTableLine && !inTable {
			inTable = true
			result = append(result, "```")
		}

		if !isTableLine && inTable {
			inTable = false
			result = append(result, "```")
		}

		if inTable {
			// Strip leading/trailing pipe and convert inner pipes to
			// padded separators for cleaner display
			trimmed := strings.TrimSpace(line)
			trimmed = strings.TrimPrefix(trimmed, "|")
			trimmed = strings.TrimSuffix(trimmed, "|")
			if !reTableSep.MatchString(line) {
				cells := strings.Split(trimmed, "|")
				for j := range cells {
					cells[j] = strings.TrimSpace(cells[j])
				}
				result = append(result, strings.Join(cells, "  |  "))
			}
		} else {
			result = append(result, line)
		}
	}

	if inTable {
		result = append(result, "```")
	}

	return strings.Join(result, "\n")
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
