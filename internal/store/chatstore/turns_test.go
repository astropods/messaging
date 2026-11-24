package chatstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func sidecarStores(t *testing.T) (*Store, *Store) {
	t.Helper()
	a, b := newSidecarPair(t)
	a.SetPodID("pod-a")
	b.SetPodID("pod-b")
	return a, b
}

func ageOwner(t *testing.T, st *Store, conversationID string, by time.Duration) {
	t.Helper()
	if _, err := st.db.ExecContext(t.Context(),
		`UPDATE turn_owners SET heartbeat_at = ? WHERE conversation_id = ?`,
		time.Now().Add(-by).UnixMilli(), conversationID,
	); err != nil {
		t.Fatalf("age owner of %s: %v", conversationID, err)
	}
}

func mustOwner(t *testing.T, st *Store, conversationID, want string) {
	t.Helper()
	got, err := st.TurnOwner(t.Context(), conversationID)
	if err != nil {
		t.Fatalf("turn owner of %s: %v", conversationID, err)
	}
	if got != want {
		t.Fatalf("owner of %s = %q, want %q", conversationID, got, want)
	}
}

func TestClaimTurnOwnerRefusesALiveClaimFromAnotherPod(t *testing.T) {
	a, b := sidecarStores(t)
	ctx := t.Context()

	if ok, err := a.ClaimTurnOwner(ctx, "conv", "pod-a"); err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v, want it granted", ok, err)
	}
	if ok, err := b.ClaimTurnOwner(ctx, "conv", "pod-b"); err != nil || ok {
		t.Fatalf("claim over a live owner: ok=%v err=%v, want it refused", ok, err)
	}
	if ok, err := a.ClaimTurnOwner(ctx, "conv", "pod-a"); err != nil || !ok {
		t.Fatalf("owner reclaiming: ok=%v err=%v, want it granted", ok, err)
	}
	mustOwner(t, b, "conv", "pod-a")
}

func TestClaimTurnOwnerTakesAStaleClaim(t *testing.T) {
	a, b := sidecarStores(t)
	ctx := t.Context()
	if _, err := a.ClaimTurnOwner(ctx, "conv", "pod-a"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	ageOwner(t, a, "conv", TurnOwnerTTL+time.Second)

	mustOwner(t, b, "conv", "")
	if ok, err := b.ClaimTurnOwner(ctx, "conv", "pod-b"); err != nil || !ok {
		t.Fatalf("claim over a stale owner: ok=%v err=%v, want it granted", ok, err)
	}
	mustOwner(t, a, "conv", "pod-b")
}

func TestSyncTurnOwnersHeartbeatsActiveAndReleasesTheRest(t *testing.T) {
	a, b := sidecarStores(t)
	ctx := t.Context()

	if err := a.SyncTurnOwners(ctx, "pod-a", []string{"c1", "c2"}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := b.SyncTurnOwners(ctx, "pod-b", []string{"c3"}); err != nil {
		t.Fatalf("sync b: %v", err)
	}
	ageOwner(t, a, "c1", TurnOwnerTTL-5*time.Second)
	if err := a.SyncTurnOwners(ctx, "pod-a", []string{"c1"}); err != nil {
		t.Fatalf("resync: %v", err)
	}

	owners, err := b.LiveTurnOwners(ctx)
	if err != nil {
		t.Fatalf("live owners: %v", err)
	}
	want := map[string]string{"c1": "pod-a", "c3": "pod-b"}
	if len(owners) != len(want) {
		t.Fatalf("live owners = %v, want %v: c2 must be released and c3 left to pod-b", owners, want)
	}
	for id, pod := range want {
		if owners[id] != pod {
			t.Fatalf("live owners = %v, want %v", owners, want)
		}
	}
	var heartbeat int64
	if err := a.db.QueryRowContext(ctx, `SELECT heartbeat_at FROM turn_owners WHERE conversation_id = 'c1'`).Scan(&heartbeat); err != nil {
		t.Fatalf("read heartbeat: %v", err)
	}
	if time.Since(time.UnixMilli(heartbeat)) > 5*time.Second {
		t.Fatal("sync did not refresh the heartbeat of an active turn")
	}
}

func TestSyncTurnOwnersLeavesAnotherPodsLiveTurn(t *testing.T) {
	a, b := sidecarStores(t)
	ctx := t.Context()
	if _, err := a.ClaimTurnOwner(ctx, "conv", "pod-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := b.SyncTurnOwners(ctx, "pod-b", []string{"conv"}); err != nil {
		t.Fatalf("sync b: %v", err)
	}
	mustOwner(t, b, "conv", "pod-a")

	if err := b.SyncTurnOwners(ctx, "pod-b", nil); err != nil {
		t.Fatalf("sync b with nothing active: %v", err)
	}
	mustOwner(t, b, "conv", "pod-a")
}

func seedUserTurn(t *testing.T, st *Store, conversationID string) {
	t.Helper()
	ctx := t.Context()
	if _, err := st.EnsureForSend(ctx, conversationID, "owner", "t"); err != nil {
		t.Fatalf("ensure %s: %v", conversationID, err)
	}
	if _, err := st.AppendMessage(ctx, conversationID, "owner", "user", "hi", ""); err != nil {
		t.Fatalf("append %s: %v", conversationID, err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE conversations SET updated_at = ? WHERE conversation_id = ?`,
		time.Now().Add(-sharedReapGrace-time.Minute).UnixMilli(), conversationID); err != nil {
		t.Fatalf("age %s: %v", conversationID, err)
	}
}

func reapedIDs(t *testing.T, st *Store, ids ...string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, id := range ids {
		msgs, err := st.ListMessages(t.Context(), id)
		if err != nil {
			t.Fatalf("list %s: %v", id, err)
		}
		out[id] = len(msgs) == 2
	}
	return out
}

func TestStartupReapTreatsItsOwnPodsRowsAsAPreviousRun(t *testing.T) {
	a, b := sidecarStores(t)
	ctx := t.Context()
	seedUserTurn(t, a, "own")
	seedUserTurn(t, a, "other")
	if err := a.SyncTurnOwners(ctx, "pod-a", []string{"own"}); err != nil {
		t.Fatalf("sync a: %v", err)
	}
	if err := b.SyncTurnOwners(ctx, "pod-b", []string{"other"}); err != nil {
		t.Fatalf("sync b: %v", err)
	}

	if _, err := a.ReapDanglingUserTurns(ctx); err != nil {
		t.Fatalf("startup reap: %v", err)
	}
	got := reapedIDs(t, a, "own", "other")
	if !got["own"] {
		t.Error("a restarted pod must reap the turn its previous run owned")
	}
	if got["other"] {
		t.Error("the startup reap must leave another pod's live turn alone")
	}
}

func TestOrphanReapSkipsEveryLiveOwner(t *testing.T) {
	a, b := sidecarStores(t)
	ctx := t.Context()
	seedUserTurn(t, a, "own")
	seedUserTurn(t, a, "other")
	seedUserTurn(t, a, "orphan")
	if err := a.SyncTurnOwners(ctx, "pod-a", []string{"own"}); err != nil {
		t.Fatalf("sync a: %v", err)
	}
	if err := b.SyncTurnOwners(ctx, "pod-b", []string{"other", "orphan"}); err != nil {
		t.Fatalf("sync b: %v", err)
	}
	ageOwner(t, a, "orphan", TurnOwnerTTL+time.Second)

	n, err := a.ReapOrphanedTurns(ctx)
	if err != nil {
		t.Fatalf("orphan reap: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped %d turns, want 1", n)
	}
	got := reapedIDs(t, a, "own", "other", "orphan")
	if got["own"] || got["other"] || !got["orphan"] {
		t.Fatalf("reaped = %v, want only the orphan whose owner stopped heartbeating", got)
	}
}

func TestClaimSlackEventIsOncePerEventAcrossSidecars(t *testing.T) {
	a, b := sidecarStores(t)
	ctx := t.Context()

	if first, err := a.ClaimSlackEvent(ctx, "Ev1"); err != nil || !first {
		t.Fatalf("first claim: first=%v err=%v", first, err)
	}
	if first, err := b.ClaimSlackEvent(ctx, "Ev1"); err != nil || first {
		t.Fatalf("retry on the other sidecar: first=%v err=%v, want it dropped", first, err)
	}
	if first, err := b.ClaimSlackEvent(ctx, "Ev2"); err != nil || !first {
		t.Fatalf("a different event: first=%v err=%v", first, err)
	}
}

func TestPruneSlackEventsDropsOnlyExpiredClaims(t *testing.T) {
	a, _ := sidecarStores(t)
	ctx := t.Context()
	for _, id := range []string{"old", "new"} {
		if _, err := a.ClaimSlackEvent(ctx, id); err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE slack_events SET seen_at = ? WHERE event_id = 'old'`,
		time.Now().Add(-slackEventRetention-time.Minute).UnixMilli()); err != nil {
		t.Fatalf("age claim: %v", err)
	}
	if err := a.PruneSlackEvents(ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if first, _ := a.ClaimSlackEvent(ctx, "old"); !first {
		t.Error("an expired claim must be pruned")
	}
	if first, _ := a.ClaimSlackEvent(ctx, "new"); first {
		t.Error("a recent claim must survive the prune")
	}
}

func finishTurn(t *testing.T, st *Store, conversationID, content string) UserTurn {
	t.Helper()
	turn, err := st.AppendUserTurn(t.Context(), conversationID, "owner", TurnInput{Content: content})
	if err != nil {
		t.Fatalf("append user turn %q: %v", content, err)
	}
	if _, err := st.FinishAssistantReply(t.Context(), conversationID, "reply to "+content, ""); err != nil {
		t.Fatalf("finish reply to %q: %v", content, err)
	}
	return turn
}

func TestSharedStoreSendsHistoryOnlyAfterAPodSwitch(t *testing.T) {
	a, b := sidecarStores(t)
	ctx := t.Context()
	if _, err := a.EnsureForSend(ctx, "conv", "owner", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if turn := finishTurn(t, a, "conv", "q1"); turn.History != nil {
		t.Fatalf("first turn carried history %+v, want none", turn.History)
	}
	if turn := finishTurn(t, a, "conv", "q2"); turn.History != nil {
		t.Fatalf("turn on the pod whose agent answered last carried history %+v, want none", turn.History)
	}
	turn := finishTurn(t, b, "conv", "q3")
	if turn.History == nil || len(turn.History.Messages) != 4 {
		t.Fatalf("turn after a pod switch: history %+v, want the 4 earlier messages", turn.History)
	}
	if turn := finishTurn(t, b, "conv", "q4"); turn.History != nil {
		t.Fatalf("second turn on the new pod carried history %+v, want none", turn.History)
	}
}

func TestSQLiteStoreIsNotShared(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	if st.Shared() {
		t.Fatal("SQLite reported shared")
	}
	if ok, err := st.ClaimTurnOwner(ctx, "conv", "pod"); err != nil || !ok {
		t.Fatalf("ClaimTurnOwner on SQLite: ok=%v err=%v, want granted", ok, err)
	}
	if owner, err := st.TurnOwner(ctx, "conv"); err != nil || owner != "" {
		t.Fatalf("TurnOwner on SQLite: %q err=%v, want none", owner, err)
	}
	if _, err := st.ClaimSlackEvent(ctx, "Ev1"); err == nil {
		t.Fatal("ClaimSlackEvent on SQLite succeeded, want an error")
	}
}
