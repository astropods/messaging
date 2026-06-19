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

	// Break the reply into small section blocks so Slack renders each one in
	// full instead of folding long text behind a "See more". splitForSectionBlocks
	// targets ~250 chars on line/sentence boundaries; the inner splitIntoChunks
	// is a safety net for any chunk still over Slack's hard 3000-char limit.
	var sections []map[string]interface{}
	for _, chunk := range splitForSectionBlocks(mrkdwn, sectionBlockTargetChars) {
		for _, sec := range splitIntoChunks(chunk, 3000) {
			sections = append(sections, map[string]interface{}{
				"type": "section",
				"text": map[string]interface{}{
					"type": "mrkdwn",
					"text": sec,
				},
			})
		}
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

// sectionBlockTargetChars is the size we aim for per section block. Slack
// folds section text longer than a few hundred characters behind a "See more",
// so keeping each block small renders the whole reply inline. Matches the
// threshold the Yoda agent uses for the same reason.
const sectionBlockTargetChars = 250

// reSentenceBoundary matches sentence-ending punctuation followed by
// whitespace, used to break an over-long line without splitting mid-word.
var reSentenceBoundary = regexp.MustCompile(`[.!?]\s+`)

// splitSentences breaks a line at sentence boundaries, keeping the terminal
// punctuation with its sentence and dropping the whitespace between sentences.
// Go's regexp has no lookbehind, so this slices around the matched boundaries
// rather than using regexp.Split (which would discard the punctuation).
func splitSentences(line string) []string {
	locs := reSentenceBoundary.FindAllStringIndex(line, -1)
	if len(locs) == 0 {
		return []string{line}
	}
	var out []string
	prev := 0
	for _, loc := range locs {
		out = append(out, line[prev:loc[0]+1]) // include the punctuation char
		prev = loc[1]                           // resume after the whitespace
	}
	if prev < len(line) {
		out = append(out, line[prev:])
	}
	return out
}

// wordWrap breaks s into pieces no longer than maxChars, splitting on spaces so
// words stay intact. A single word longer than maxChars (e.g. a long URL) is
// hard-split as a last resort. Returns the trimmed input unchanged when it
// already fits.
func wordWrap(s string, maxChars int) []string {
	s = strings.TrimSpace(s)
	if len(s) <= maxChars {
		return []string{s}
	}

	var out []string
	current := ""
	for _, word := range strings.Fields(s) {
		// A single oversized word can't fit any line — hard-split it.
		for len(word) > maxChars {
			if current != "" {
				out = append(out, current)
				current = ""
			}
			out = append(out, word[:maxChars])
			word = word[maxChars:]
		}
		switch {
		case current == "":
			current = word
		case len(current)+1+len(word) > maxChars:
			out = append(out, current)
			current = word
		default:
			current += " " + word
		}
	}
	if current != "" {
		out = append(out, current)
	}
	return out
}

// splitForSectionBlocks splits mrkdwn text into chunks of roughly targetChars,
// small enough that Slack renders each section block in full rather than
// collapsing it behind a "See more" fold. It breaks on line boundaries, and on
// sentence boundaries within a line that is itself longer than the target.
// Two structures are kept intact so formatting survives: fenced code blocks
// (```…```, which convertTables also emits for tables) and runs of consecutive
// blockquote lines (so Slack draws one continuous bar). Chunks may still exceed
// targetChars when a single sentence or code block is longer; the caller caps
// them at Slack's hard 3000-char section limit.
func splitForSectionBlocks(text string, targetChars int) []string {
	var out []string
	current := ""

	flush := func() {
		if strings.TrimSpace(current) != "" {
			out = append(out, strings.TrimSpace(current))
		}
		current = ""
	}
	appendLine := func(line string) {
		if current != "" {
			current += "\n" + line
		} else {
			current = line
		}
	}

	inFence := false
	for _, line := range strings.Split(text, "\n") {
		// Keep fenced code blocks whole — never split between the ``` pair.
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if !inFence {
				flush() // start the fence in its own chunk
			}
			appendLine(line)
			inFence = !inFence
			if !inFence {
				flush() // closing fence — emit the whole block
			}
			continue
		}
		if inFence {
			appendLine(line)
			continue
		}

		// Glue adjacent blockquote lines so Slack renders one bar, not several.
		lastLine := current
		if idx := strings.LastIndex(current, "\n"); idx != -1 {
			lastLine = current[idx+1:]
		}
		if strings.HasPrefix(strings.TrimLeft(lastLine, " \t"), ">") &&
			strings.HasPrefix(strings.TrimLeft(line, " \t"), ">") {
			appendLine(line)
			continue
		}

		if len(line) > targetChars {
			// Over-long line: split on sentence boundaries, then word-wrap any
			// sentence that is itself longer than the target (a single long
			// clause with no .!? break would otherwise stay one oversized block
			// and Slack would fold it behind a "See more").
			for _, sent := range splitSentences(line) {
				for _, piece := range wordWrap(sent, targetChars) {
					if current != "" && len(current)+len(piece)+1 > targetChars {
						flush()
						current = piece
					} else if current != "" {
						current += " " + piece
					} else {
						current = piece
					}
				}
			}
			continue
		}

		if current != "" && len(current)+len(line)+1 > targetChars {
			flush()
		}
		appendLine(line)
	}
	flush()

	return out
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
