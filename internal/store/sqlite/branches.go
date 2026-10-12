package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// In messages.parent_id, "" means the previous message by seq and parentRoot means none.
const parentRoot = "root"

// Keeps a history replay well under the 4 MiB gRPC message limit.
const maxHistoryBytes = 512 << 10

// ErrInvalidEdit is returned when edit_of names no user message in the conversation.
var ErrInvalidEdit = errors.New("message cannot be edited")

// ErrStaleBranch is returned when the anchor is not the active branch's newest message.
var ErrStaleBranch = errors.New("the active branch changed")

type TurnInput struct {
	Content         string
	AttachmentsJSON string
	EditOf          string
	// Anchor is the newest message the client shows; "" skips the check.
	Anchor string
}

type UserTurn struct {
	Message
	// History is set when the agent's memory may not end where this message starts.
	History *History
}

// History is the active branch before a user turn, oldest first.
type History struct {
	Messages []Message
	Complete bool
}

type treeNode struct {
	id     string
	parent string // resolved parent; "" for a root
	seq    int
	role   string
}

// nodes are in seq order, and a child always has a higher seq than its parent.
type messageTree struct {
	nodes    []treeNode
	index    map[string]int
	children map[string][]string // keyed by parent; "" holds the roots
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func loadTree(ctx context.Context, q queryer, conversationID string) (*messageTree, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, parent_id, seq, role FROM messages WHERE conversation_id = ? ORDER BY seq ASC`,
		conversationID,
	)
	if err != nil {
		return nil, fmt.Errorf("chatstore load tree: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	t := &messageTree{index: map[string]int{}, children: map[string][]string{}}
	prev := ""
	for rows.Next() {
		var (
			n      treeNode
			stored string
		)
		if err := rows.Scan(&n.id, &stored, &n.seq, &n.role); err != nil {
			return nil, fmt.Errorf("chatstore load tree scan: %w", err)
		}
		switch stored {
		case "":
			n.parent = prev
		case parentRoot:
			n.parent = ""
		default:
			n.parent = stored
		}
		t.index[n.id] = len(t.nodes)
		t.nodes = append(t.nodes, n)
		t.children[n.parent] = append(t.children[n.parent], n.id)
		prev = n.id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chatstore load tree rows: %w", err)
	}
	return t, nil
}

func (t *messageTree) node(id string) (treeNode, bool) {
	i, ok := t.index[id]
	if !ok {
		return treeNode{}, false
	}
	return t.nodes[i], true
}

func (t *messageTree) newest() string {
	if len(t.nodes) == 0 {
		return ""
	}
	return t.nodes[len(t.nodes)-1].id
}

// resolve decodes a stored head: "" or a missing id is the newest message, parentRoot is none.
func (t *messageTree) resolve(stored string) string {
	if stored == parentRoot {
		return ""
	}
	if _, ok := t.index[stored]; ok {
		return stored
	}
	return t.newest()
}

func (t *messageTree) pathTo(id string) []treeNode {
	var path []treeNode
	for n, ok := t.node(id); ok && len(path) < len(t.nodes); n, ok = t.node(n.parent) {
		path = append(path, n)
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

func (t *messageTree) branches(id string) []string {
	n, ok := t.node(id)
	if !ok {
		return nil
	}
	siblings := t.children[n.parent]
	if len(siblings) < 2 {
		return nil
	}
	return siblings
}

func (t *messageTree) newestUnder(id string) string {
	start, ok := t.index[id]
	if !ok {
		return ""
	}
	in := map[string]bool{id: true}
	newest := id
	for _, n := range t.nodes[start+1:] {
		if in[n.parent] {
			in[n.id] = true
			newest = n.id
		}
	}
	return newest
}

// storedParent encodes parent for messages.parent_id, given the tree before the insert.
func (t *messageTree) storedParent(parent string) string {
	switch {
	case parent == t.newest():
		return ""
	case parent == "":
		return parentRoot
	default:
		return parent
	}
}

// head counts only while the newest message is headNewest, since an older image
// appends without resetting it. agentHead is where the agent's memory ends.
type heads struct {
	head, headNewest, agentHead string
}

func conversationHeads(ctx context.Context, q queryer, conversationID string) (heads, error) {
	var h heads
	err := q.QueryRowContext(ctx,
		`SELECT head_id, head_newest_id, agent_head_id FROM conversations WHERE conversation_id = ?`,
		conversationID,
	).Scan(&h.head, &h.headNewest, &h.agentHead)
	if errors.Is(err, sql.ErrNoRows) {
		return heads{}, nil
	}
	if err != nil {
		return heads{}, fmt.Errorf("chatstore read heads: %w", err)
	}
	return h, nil
}

func (t *messageTree) activeHead(h heads) string {
	if h.head == "" || h.headNewest != t.newest() {
		return t.newest()
	}
	return t.resolve(h.head)
}

func loadMessages(ctx context.Context, q queryer, conversationID string, path []treeNode) ([]Message, error) {
	if len(path) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(path)+1)
	args = append(args, conversationID)
	for _, n := range path {
		args = append(args, n.id)
	}
	rows, err := q.QueryContext(ctx, `
		SELECT id, role, content, seq, attachments, author
		FROM messages
		WHERE conversation_id = ? AND id IN (?`+strings.Repeat(",?", len(path)-1)+`)
		ORDER BY seq ASC`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("chatstore load messages: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	out := make([]Message, 0, len(path))
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &m.Seq, &m.Attachments, &m.Author); err != nil {
			return nil, fmt.Errorf("chatstore load messages scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chatstore load messages rows: %w", err)
	}
	return out, nil
}

// PageMessages pages the active branch, oldest first. lastRole may come from another branch.
func (s *Store) PageMessages(ctx context.Context, conversationID string, limit, beforeSeq int) (msgs []Message, hasMore bool, oldestSeq int, lastRole string, err error) {
	if limit <= 0 {
		limit = 1
	}
	t, err := loadTree(ctx, s.db, conversationID)
	if err != nil {
		return nil, false, 0, "", err
	}
	h, err := conversationHeads(ctx, s.db, conversationID)
	if err != nil {
		return nil, false, 0, "", err
	}
	path := t.pathTo(t.activeHead(h))

	end := len(path)
	if beforeSeq > 0 {
		end = sort.Search(len(path), func(i int) bool { return path[i].seq >= beforeSeq })
	}
	start := max(0, end-limit)
	page := path[start:end]
	hasMore = start > 0
	if len(page) > 0 {
		oldestSeq = page[0].seq
	}

	msgs, err = loadMessages(ctx, s.db, conversationID, page)
	if err != nil {
		return nil, false, 0, "", err
	}
	for i := range msgs {
		msgs[i].Branches = t.branches(msgs[i].ID)
	}

	// A saved copy ends on a user turn with no reply coming; "" keeps it from reading as in flight.
	err = s.db.QueryRowContext(ctx,
		`SELECT CASE WHEN origin = 'save' THEN '' ELSE role END
		 FROM messages WHERE conversation_id = ? ORDER BY seq DESC LIMIT 1`,
		conversationID,
	).Scan(&lastRole)
	if errors.Is(err, sql.ErrNoRows) {
		return msgs, hasMore, oldestSeq, "", nil
	}
	if err != nil {
		return nil, false, 0, "", fmt.Errorf("chatstore page last role: %w", err)
	}
	return msgs, hasMore, oldestSeq, lastRole, nil
}

// AppendUserTurn always sets History on an edit: the agent may hold the edited turn unanswered.
func (s *Store) AppendUserTurn(ctx context.Context, conversationID, userID string, in TurnInput) (UserTurn, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UserTurn{}, fmt.Errorf("chatstore append user turn begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	t, err := loadTree(ctx, tx, conversationID)
	if err != nil {
		return UserTurn{}, err
	}
	h, err := conversationHeads(ctx, tx, conversationID)
	if err != nil {
		return UserTurn{}, err
	}

	parent := t.activeHead(h)
	if in.Anchor != "" && in.Anchor != parent {
		return UserTurn{}, ErrStaleBranch
	}
	if in.EditOf != "" {
		edited, ok := t.node(in.EditOf)
		if !ok || edited.role != "user" {
			return UserTurn{}, ErrInvalidEdit
		}
		parent = edited.parent
	}

	// Pin before the insert, or an unset head would resolve to this turn and read as held.
	agentHead := h.agentHead
	agentAt := t.resolve(agentHead)
	if agentHead == "" {
		agentHead = agentAt
		if agentHead == "" {
			agentHead = parentRoot
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE conversations SET agent_head_id = ? WHERE conversation_id = ?`, agentHead, conversationID,
		); err != nil {
			return UserTurn{}, fmt.Errorf("chatstore pin agent head: %w", err)
		}
	}

	msg, err := insertMessageTx(ctx, tx, conversationID, userID, "user", in.Content, in.AttachmentsJSON, t.storedParent(parent))
	if err != nil {
		return UserTurn{}, err
	}
	turn := UserTurn{Message: msg}
	if in.EditOf != "" || parent != agentAt {
		turn.History, err = historyTx(ctx, tx, conversationID, t.pathTo(parent))
		if err != nil {
			return UserTurn{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return UserTurn{}, fmt.Errorf("chatstore append user turn commit: %w", err)
	}
	return turn, nil
}

func historyTx(ctx context.Context, q queryer, conversationID string, path []treeNode) (*History, error) {
	msgs, err := loadMessages(ctx, q, conversationID, path)
	if err != nil {
		return nil, err
	}
	h := &History{Complete: true}
	budget := maxHistoryBytes
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == "user" && m.Content == "" {
			m.Content = attachmentsLine(m.Attachments)
		}
		if (m.Role != "user" && m.Role != "assistant") || m.Content == "" {
			continue
		}
		if len(m.Content) > budget {
			h.Complete = false
			break
		}
		budget -= len(m.Content)
		h.Messages = append(h.Messages, m)
	}
	for i, j := 0, len(h.Messages)-1; i < j; i, j = i+1, j-1 {
		h.Messages[i], h.Messages[j] = h.Messages[j], h.Messages[i]
	}
	return h, nil
}

func attachmentsLine(attachmentsJSON string) string {
	var files []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(attachmentsJSON), &files); err != nil || len(files) == 0 {
		return ""
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Name)
	}
	return "[Attached: " + strings.Join(names, ", ") + "]"
}

// SwitchBranch makes the newest message under messageID the head; false means not found.
func (s *Store) SwitchBranch(ctx context.Context, conversationID, userID, messageID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("chatstore switch branch begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM conversations WHERE conversation_id = ? AND user_id = ? AND deleted_at IS NULL`,
		conversationID, userID,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("chatstore switch branch owner check: %w", err)
	}

	t, err := loadTree(ctx, tx, conversationID)
	if err != nil {
		return false, err
	}
	leaf := t.newestUnder(messageID)
	if leaf == "" {
		return false, nil
	}
	head := leaf
	if leaf == t.newest() {
		head = ""
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE conversations SET head_id = ?, head_newest_id = ? WHERE conversation_id = ?`,
		head, t.newest(), conversationID,
	); err != nil {
		return false, fmt.Errorf("chatstore switch branch: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("chatstore switch branch commit: %w", err)
	}
	return true, nil
}
