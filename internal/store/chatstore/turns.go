package chatstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TurnOwnerTTL is how long an owner row stays live without a heartbeat.
const TurnOwnerTTL = 30 * time.Second

// slackEventRetention outlasts Slack's retry window, which ends within minutes.
const slackEventRetention = time.Hour

var errNeedsPostgres = errors.New("chatstore: needs the postgres chat store")

// Shared reports whether the store is Postgres, which several sidecars can share.
func (s *Store) Shared() bool { return s.db.dialect == dialectPostgres }

func (s *Store) liveOwnerExclusion(ownRowsStale bool) string {
	if !s.Shared() {
		return ""
	}
	q := `
			AND NOT EXISTS (SELECT 1 FROM turn_owners o WHERE o.conversation_id = c.conversation_id AND o.heartbeat_at > ?`
	if ownRowsStale {
		q += ` AND o.pod_id <> ?`
	}
	return q + `)`
}

func (s *Store) reapArgs(ownRowsStale bool) []any {
	args := []any{time.Now().Add(-s.reapGrace).UnixMilli()}
	if s.Shared() {
		args = append(args, liveOwnerCutoff())
		if ownRowsStale {
			args = append(args, s.podID)
		}
	}
	return args
}

func liveOwnerCutoff() int64 { return time.Now().Add(-TurnOwnerTTL).UnixMilli() }

// SyncTurnOwners records podID as the owner of active and releases every other row podID holds.
// A live row held by another pod is left alone.
func (s *Store) SyncTurnOwners(ctx context.Context, podID string, active []string) error {
	if !s.Shared() {
		return errNeedsPostgres
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("chatstore sync turn owners begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if active == nil {
		active = []string{}
	}
	now := time.Now().UnixMilli()
	if len(active) > 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO turn_owners (conversation_id, pod_id, heartbeat_at)
			SELECT id, ?, ? FROM unnest(?::text[]) AS id
			ON CONFLICT (conversation_id) DO UPDATE SET pod_id = excluded.pod_id, heartbeat_at = excluded.heartbeat_at
			WHERE turn_owners.pod_id = excluded.pod_id OR turn_owners.heartbeat_at <= ?`,
			podID, now, active, liveOwnerCutoff(),
		); err != nil {
			return fmt.Errorf("chatstore sync turn owners upsert: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM turn_owners WHERE pod_id = ? AND NOT (conversation_id = ANY(?::text[]))`,
		podID, active,
	); err != nil {
		return fmt.Errorf("chatstore sync turn owners release: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("chatstore sync turn owners commit: %w", err)
	}
	return nil
}

// ClaimTurnOwner makes podID the conversation's owner unless another pod holds a live claim.
func (s *Store) ClaimTurnOwner(ctx context.Context, conversationID, podID string) (bool, error) {
	if !s.Shared() {
		return true, nil
	}
	var owner string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO turn_owners (conversation_id, pod_id, heartbeat_at) VALUES (?, ?, ?)
		ON CONFLICT (conversation_id) DO UPDATE SET pod_id = excluded.pod_id, heartbeat_at = excluded.heartbeat_at
		WHERE turn_owners.pod_id = excluded.pod_id OR turn_owners.heartbeat_at <= ?
		RETURNING pod_id`,
		conversationID, podID, time.Now().UnixMilli(), liveOwnerCutoff(),
	).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("chatstore claim turn owner: %w", err)
	}
	return true, nil
}

// TurnOwner returns the pod running the conversation's turn, or "" when no live owner holds it.
func (s *Store) TurnOwner(ctx context.Context, conversationID string) (string, error) {
	if !s.Shared() {
		return "", nil
	}
	var pod string
	err := s.db.QueryRowContext(ctx,
		`SELECT pod_id FROM turn_owners WHERE conversation_id = ? AND heartbeat_at > ?`,
		conversationID, liveOwnerCutoff(),
	).Scan(&pod)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("chatstore turn owner: %w", err)
	}
	return pod, nil
}

// LiveTurnOwners maps each conversation with a live owner to that owner's pod.
func (s *Store) LiveTurnOwners(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	if !s.Shared() {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT conversation_id, pod_id FROM turn_owners WHERE heartbeat_at > ?`, liveOwnerCutoff(),
	)
	if err != nil {
		return nil, fmt.Errorf("chatstore live turn owners: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var conv, pod string
		if err := rows.Scan(&conv, &pod); err != nil {
			return nil, fmt.Errorf("chatstore live turn owners scan: %w", err)
		}
		out[conv] = pod
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chatstore live turn owners rows: %w", err)
	}
	return out, nil
}

// ClaimSlackEvent records eventID and reports whether this call was first to see it.
func (s *Store) ClaimSlackEvent(ctx context.Context, eventID string) (bool, error) {
	if !s.Shared() {
		return false, errNeedsPostgres
	}
	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO slack_events (event_id, seen_at) VALUES (?, ?) ON CONFLICT (event_id) DO NOTHING`,
		eventID, now.UnixMilli(),
	)
	if err != nil {
		return false, fmt.Errorf("chatstore claim slack event: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("chatstore claim slack event rows: %w", err)
	}
	return n == 1, nil
}

// PruneSlackEvents deletes claims older than the retention window.
func (s *Store) PruneSlackEvents(ctx context.Context) error {
	if !s.Shared() {
		return errNeedsPostgres
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM slack_events WHERE seen_at < ?`, time.Now().Add(-slackEventRetention).UnixMilli(),
	); err != nil {
		return fmt.Errorf("chatstore prune slack events: %w", err)
	}
	return nil
}
