package web

// Covers paginateChatMessages, especially the before_seq clamp: before_seq is
// caller-controlled and may exceed the thread length (crafted query or a stale
// client paging past the head). Without clamping end to the message count the
// slice expression panics (a 500 recovered by net/http).
//
//	go test ./internal/adapter/web -run TestPaginateChatMessages -v

import (
	"strconv"
	"testing"

	"github.com/astropods/messaging/internal/store/sqlite"
)

func seqMessages(n int) []sqlite.Message {
	out := make([]sqlite.Message, n)
	for i := 0; i < n; i++ {
		out[i] = sqlite.Message{ID: strconv.Itoa(i + 1), Role: "user", Content: "c", Seq: i + 1}
	}
	return out
}

func TestPaginateChatMessages(t *testing.T) {
	tests := []struct {
		name        string
		all         []sqlite.Message
		limit       int
		beforeSeq   int
		wantLen     int
		wantHasMore bool
		wantOldest  int
	}{
		{
			name:      "before_seq far beyond range does not panic",
			all:       seqMessages(3),
			limit:     100,
			beforeSeq: 10,
			// end clamps to 3, start clamps to 1 -> whole thread.
			wantLen: 3, wantHasMore: false, wantOldest: 1,
		},
		{
			name:      "before_seq beyond range with small limit returns clamped tail",
			all:       seqMessages(5),
			limit:     2,
			beforeSeq: 100,
			// end clamps to 5, start = 4 -> seq 4,5.
			wantLen: 2, wantHasMore: true, wantOldest: 4,
		},
		{
			name:      "before_seq within range returns older page",
			all:       seqMessages(5),
			limit:     2,
			beforeSeq: 4,
			// end = 3, start = 2 -> seq 2,3.
			wantLen: 2, wantHasMore: true, wantOldest: 2,
		},
		{
			name:      "before_seq at head yields empty page",
			all:       seqMessages(3),
			limit:     10,
			beforeSeq: 1,
			wantLen:   0, wantHasMore: false, wantOldest: 0,
		},
		{
			name:  "no before_seq, thread fits in limit",
			all:   seqMessages(3),
			limit: 10,
			// beforeSeq 0
			wantLen: 3, wantHasMore: false, wantOldest: 1,
		},
		{
			name:  "no before_seq, thread exceeds limit returns tail",
			all:   seqMessages(5),
			limit: 2,
			// beforeSeq 0 -> newest 2.
			wantLen: 2, wantHasMore: true, wantOldest: 4,
		},
		{
			name:    "empty thread",
			all:     nil,
			limit:   10,
			wantLen: 0, wantHasMore: false, wantOldest: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msgs, hasMore, oldest := paginateChatMessages(tc.all, tc.limit, tc.beforeSeq)
			if len(msgs) != tc.wantLen {
				t.Errorf("len = %d, want %d", len(msgs), tc.wantLen)
			}
			if hasMore != tc.wantHasMore {
				t.Errorf("hasMore = %v, want %v", hasMore, tc.wantHasMore)
			}
			if oldest != tc.wantOldest {
				t.Errorf("oldestSeq = %d, want %d", oldest, tc.wantOldest)
			}
		})
	}
}
