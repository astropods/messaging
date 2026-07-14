package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// This file implements store.InteractionStore against the SQLite chat database.
// Interactions live in their own table but draw their seq from the shared
// allocator (nextSeqTx), so they interleave with messages by order. The whole
// InteractionStore interface is satisfied here so the web adapter can be wired to
// the durable store in place of the in-memory one.

var _ store.InteractionStore = (*Store)(nil)

const interactionColumns = `id, conversation_id, user_id, seq, render_json, status, response_json, created_at, responded_at`

// AppendInteraction is intentionally uncapped (unlike messages): interactions are
// gated by agent turns, which are themselves bounded, so no per-conversation cap
// is enforced here.
func (s *Store) AppendInteraction(ctx context.Context, conversationID, userID string, r *pb.Renderable) (store.Interaction, error) {
	renderJSON, err := protojson.Marshal(r)
	if err != nil {
		return store.Interaction{}, fmt.Errorf("chatstore marshal renderable: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Interaction{}, fmt.Errorf("chatstore append interaction begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	// Idempotent by id: a re-emitted Renderable returns the existing record rather
	// than resetting an answered one or duplicating it in the shared seq order.
	if existing, found, err := scanInteractionRow(tx.QueryRowContext(ctx,
		`SELECT `+interactionColumns+` FROM interactions WHERE conversation_id = ? AND id = ?`,
		conversationID, r.GetId())); err != nil {
		return store.Interaction{}, err
	} else if found {
		return existing, nil
	}

	seq, err := nextSeqTx(ctx, tx, conversationID)
	if err != nil {
		return store.Interaction{}, err
	}

	created := time.Now()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO interactions (id, conversation_id, user_id, seq, render_json, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.GetId(), conversationID, userID, seq, string(renderJSON), string(store.InteractionPending), created.UnixMilli(),
	); err != nil {
		return store.Interaction{}, fmt.Errorf("chatstore insert interaction: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.Interaction{}, fmt.Errorf("chatstore append interaction commit: %w", err)
	}

	return store.Interaction{
		ID:             r.GetId(),
		ConversationID: conversationID,
		UserID:         userID,
		Seq:            seq,
		Renderable:     r,
		Status:         store.InteractionPending,
		CreatedAt:      created,
	}, nil
}

func (s *Store) GetInteraction(ctx context.Context, conversationID, interactionID string) (store.Interaction, bool, error) {
	return scanInteractionRow(s.db.QueryRowContext(ctx,
		`SELECT `+interactionColumns+` FROM interactions WHERE conversation_id = ? AND id = ?`,
		conversationID, interactionID))
}

func (s *Store) RecordInteractionResponse(ctx context.Context, conversationID, interactionID string, resp *pb.RenderableResponse) (store.Interaction, bool, error) {
	respJSON, err := protojson.Marshal(resp)
	if err != nil {
		return store.Interaction{}, false, fmt.Errorf("chatstore marshal response: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Interaction{}, false, fmt.Errorf("chatstore record response begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	it, found, err := scanInteractionRow(tx.QueryRowContext(ctx,
		`SELECT `+interactionColumns+` FROM interactions WHERE conversation_id = ? AND id = ?`,
		conversationID, interactionID))
	if err != nil {
		return store.Interaction{}, false, err
	}
	if !found {
		return store.Interaction{}, false, store.ErrInteractionNotFound
	}
	// Already answered: return the stored outcome so the caller replays it rather
	// than delivering a second response to the agent. The read-then-update runs in
	// one tx over the single connection, so a concurrent responder can't slip
	// between the pending check and the update.
	if it.Status != store.InteractionPending {
		return it, false, nil
	}

	status := store.StatusForAction(resp.GetAction())
	respondedAt := time.Now()
	if _, err := tx.ExecContext(ctx, `
		UPDATE interactions SET status = ?, response_json = ?, responded_at = ?
		WHERE conversation_id = ? AND id = ? AND status = ?`,
		string(status), string(respJSON), respondedAt.UnixMilli(),
		conversationID, interactionID, string(store.InteractionPending),
	); err != nil {
		return store.Interaction{}, false, fmt.Errorf("chatstore update interaction: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.Interaction{}, false, fmt.Errorf("chatstore record response commit: %w", err)
	}

	it.Status = status
	it.Response = resp
	it.RespondedAt = respondedAt
	return it, true, nil
}

func (s *Store) ListInteractions(ctx context.Context, conversationID string) ([]store.Interaction, error) {
	return s.queryInteractions(ctx,
		`SELECT `+interactionColumns+` FROM interactions WHERE conversation_id = ? ORDER BY seq ASC`,
		conversationID)
}

func (s *Store) PendingInteractions(ctx context.Context, conversationID string) ([]store.Interaction, error) {
	return s.queryInteractions(ctx,
		`SELECT `+interactionColumns+` FROM interactions WHERE conversation_id = ? AND status = 'pending' ORDER BY seq ASC`,
		conversationID)
}

func (s *Store) queryInteractions(ctx context.Context, query string, args ...any) ([]store.Interaction, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("chatstore list interactions: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	out := make([]store.Interaction, 0, 8)
	for rows.Next() {
		it, err := scanInteraction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chatstore list interactions rows: %w", err)
	}
	return out, nil
}

// scanInteractionRow scans a single-row query, reporting found=false for no rows.
func scanInteractionRow(row *sql.Row) (store.Interaction, bool, error) {
	it, err := scanInteraction(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Interaction{}, false, nil
	}
	if err != nil {
		return store.Interaction{}, false, err
	}
	return it, true, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanInteraction(row rowScanner) (store.Interaction, error) {
	var (
		it           store.Interaction
		seq          int64
		renderJSON   string
		status       string
		responseJSON sql.NullString
		createdMs    int64
		respondedMs  sql.NullInt64
	)
	if err := row.Scan(&it.ID, &it.ConversationID, &it.UserID, &seq, &renderJSON, &status, &responseJSON, &createdMs, &respondedMs); err != nil {
		return store.Interaction{}, err
	}

	r := &pb.Renderable{}
	if err := protojson.Unmarshal([]byte(renderJSON), r); err != nil {
		return store.Interaction{}, fmt.Errorf("chatstore unmarshal renderable: %w", err)
	}
	it.Renderable = r
	it.Seq = int(seq)
	it.Status = store.InteractionStatus(status)
	it.CreatedAt = time.UnixMilli(createdMs)
	if responseJSON.Valid && responseJSON.String != "" {
		resp := &pb.RenderableResponse{}
		if err := protojson.Unmarshal([]byte(responseJSON.String), resp); err != nil {
			return store.Interaction{}, fmt.Errorf("chatstore unmarshal response: %w", err)
		}
		it.Response = resp
	}
	if respondedMs.Valid {
		it.RespondedAt = time.UnixMilli(respondedMs.Int64)
	}
	return it, nil
}
