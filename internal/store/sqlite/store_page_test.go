package sqlite

// Covers PageMessages, which windows a thread in SQL (WHERE seq < ? ... LIMIT ?)
// rather than materializing the whole thread. Preserves the prior pagination
// contract: seq is contiguous from 1, so the page's oldest seq alone determines
// hasMore. Includes the before_seq-beyond-range case that previously risked an
// out-of-range slice.
//
//	go test ./internal/store/sqlite -run TestPageMessages -v

import (
	"context"
	"strconv"
	"testing"
)

func seedThread(t *testing.T, st *Store, convID string, n int) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.EnsureForSend(ctx, convID, "owner", "t"); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := st.AppendMessage(ctx, convID, "owner", "user", strconv.Itoa(i+1)); err != nil {
			t.Fatalf("seed message %d: %v", i, err)
		}
	}
}

func TestPageMessages(t *testing.T) {
	tests := []struct {
		name        string
		total       int
		limit       int
		beforeSeq   int
		wantLen     int
		wantHasMore bool
		wantOldest  int
	}{
		{
			name: "before_seq far beyond range does not over-read",
			total: 3, limit: 100, beforeSeq: 10,
			wantLen: 3, wantHasMore: false, wantOldest: 1,
		},
		{
			name: "before_seq beyond range with small limit returns clamped tail",
			total: 5, limit: 2, beforeSeq: 100,
			wantLen: 2, wantHasMore: true, wantOldest: 4,
		},
		{
			name: "before_seq within range returns older page",
			total: 5, limit: 2, beforeSeq: 4,
			wantLen: 2, wantHasMore: true, wantOldest: 2,
		},
		{
			name: "before_seq at head yields empty page",
			total: 3, limit: 10, beforeSeq: 1,
			wantLen: 0, wantHasMore: false, wantOldest: 0,
		},
		{
			name: "no before_seq, thread fits in limit",
			total: 3, limit: 10,
			wantLen: 3, wantHasMore: false, wantOldest: 1,
		},
		{
			name: "no before_seq, thread exceeds limit returns tail",
			total: 5, limit: 2,
			wantLen: 2, wantHasMore: true, wantOldest: 4,
		},
		{
			name: "empty thread",
			total: 0, limit: 10,
			wantLen: 0, wantHasMore: false, wantOldest: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			seedThread(t, st, "c", tc.total)

			msgs, hasMore, oldest, _, err := st.PageMessages(context.Background(), "c", tc.limit, tc.beforeSeq)
			if err != nil {
				t.Fatalf("PageMessages: %v", err)
			}
			if len(msgs) != tc.wantLen {
				t.Errorf("len = %d, want %d", len(msgs), tc.wantLen)
			}
			if hasMore != tc.wantHasMore {
				t.Errorf("hasMore = %v, want %v", hasMore, tc.wantHasMore)
			}
			if oldest != tc.wantOldest {
				t.Errorf("oldestSeq = %d, want %d", oldest, tc.wantOldest)
			}
			// The returned page must be ascending and contiguous from oldestSeq.
			for i, m := range msgs {
				if m.Seq != oldest+i {
					t.Fatalf("page not ascending/contiguous: msgs[%d].Seq=%d want %d", i, m.Seq, oldest+i)
				}
			}
		})
	}
}

// lastRole reflects the newest message in the whole thread, independent of which
// page is returned — this is what drives the assistant_streaming heuristic.
func TestPageMessagesLastRole(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.EnsureForSend(ctx, "c", "owner", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.AppendMessage(ctx, "c", "owner", "user", "q"); err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := st.UpsertAssistantProgress(ctx, "c", "reply"); err != nil {
		t.Fatalf("assistant: %v", err)
	}

	// Page an OLDER window (before the assistant row); lastRole must still be the
	// newest message's role, not the page's last row.
	_, _, _, lastRole, err := st.PageMessages(ctx, "c", 1, 2)
	if err != nil {
		t.Fatalf("PageMessages: %v", err)
	}
	if lastRole != "assistant" {
		t.Fatalf("lastRole = %q, want assistant (newest row), independent of the paged window", lastRole)
	}
}
