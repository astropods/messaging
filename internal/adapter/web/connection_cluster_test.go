package web

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

type relayCall struct {
	kind, conversationID string
	data                 any
}

type relayRecorder struct {
	mu    sync.Mutex
	calls []relayCall
}

func (r *relayRecorder) relay(kind, conversationID string, data any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, relayCall{kind, conversationID, data})
}

func (r *relayRecorder) snapshot() []relayCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]relayCall(nil), r.calls...)
}

func relayedManager() (*ConnectionManager, *relayRecorder) {
	cm := NewConnectionManager(time.Minute)
	rec := &relayRecorder{}
	cm.SetRelay(rec.relay)
	return cm, rec
}

func chunk(text string) SSEEvent {
	return SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"` + text + `"}`}
}

func connect(cm *ConnectionManager, conversationID string) *SSEConnection {
	conn := &SSEConnection{ID: "c-" + conversationID, ConversationID: conversationID, EventChan: make(chan SSEEvent, 100), Done: make(chan struct{})}
	cm.Add(conn)
	return conn
}

func drain(conn *SSEConnection) []SSEEvent {
	var out []SSEEvent
	for {
		select {
		case ev := <-conn.EventChan:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestBroadcastRelaysOnlyWhileAnotherSidecarWatches(t *testing.T) {
	cm, rec := relayedManager()

	cm.Broadcast("conv", chunk("before"))
	if n := len(rec.snapshot()); n != 0 {
		t.Fatalf("relayed %d messages with no watcher, want 0", n)
	}

	cm.Watch("conv", "pod-b", false)
	cm.Broadcast("conv", chunk("during"))
	calls := rec.snapshot()
	if len(calls) != 1 || calls[0].kind != busSSE {
		t.Fatalf("relays = %+v, want one %s", calls, busSSE)
	}
	if ev := calls[0].data.(sseMessage).Events[0]; ev.ID != "2" {
		t.Fatalf("relayed event id = %q, want the owner's id 2", ev.ID)
	}

	cm.CloseConversation("conv")
	if calls := rec.snapshot(); calls[len(calls)-1].kind != busClose {
		t.Fatalf("last relay = %s, want %s", calls[len(calls)-1].kind, busClose)
	}
}

func TestWatchReplaysTheLiveTurnOnlyWhenAsked(t *testing.T) {
	cm, rec := relayedManager()
	cm.Broadcast("conv", chunk("one"))
	cm.Broadcast("conv", chunk("two"))

	cm.Watch("conv", "pod-b", false)
	if n := len(rec.snapshot()); n != 0 {
		t.Fatalf("a renewal relayed %d messages, want none", n)
	}

	cm.Watch("conv", "pod-b", true)
	calls := rec.snapshot()
	if len(calls) != 1 || calls[0].kind != busReplay {
		t.Fatalf("relays = %+v, want one %s", calls, busReplay)
	}
	m := calls[0].data.(replayMessage)
	if m.To != "pod-b" || len(m.Events) != 2 || m.Events[0].ID != "1" || m.Events[1].ID != "2" {
		t.Fatalf("replay = %+v, want both buffered events with ids 1 and 2, addressed to pod-b", m)
	}

	cm.Watch("other", "pod-b", true)
	calls = rec.snapshot()
	if m := calls[len(calls)-1].data.(replayMessage); calls[len(calls)-1].kind != busReplay || len(m.Events) != 0 {
		t.Fatalf("a replay for a conversation with no live turn = %+v, want an empty replay", calls[len(calls)-1])
	}
}

func TestMirrorKeepsTheOwnersIDsSoResumeWorks(t *testing.T) {
	owner, _ := relayedManager()
	for _, text := range []string{"a", "b", "c"} {
		owner.Broadcast("conv", chunk(text))
	}
	var events []SSEEvent
	for _, be := range owner.eventBuffers["conv"].events {
		events = append(events, be.event)
	}

	watcher := NewConnectionManager(time.Minute)
	watcher.Mirror("conv", events, true)

	conn := &SSEConnection{ID: "c1", ConversationID: "conv", EventChan: make(chan SSEEvent, 10), Done: make(chan struct{})}
	missed, caughtUp, crossed := watcher.AddWithResume(conn, 1)
	if crossed || caughtUp || len(missed) != 2 || missed[0].ID != "2" || missed[1].ID != "3" {
		t.Fatalf("resume from the owner's id 1: missed=%+v caughtUp=%v crossed=%v, want ids 2 and 3", missed, caughtUp, crossed)
	}
}

func TestMirrorReplayReplacesABufferNumberedByThisSidecar(t *testing.T) {
	watcher := NewConnectionManager(time.Minute)
	for i := range 10 {
		watcher.Broadcast("conv", chunk(strconv.Itoa(i)))
	}

	watcher.Mirror("conv", []SSEEvent{{Event: EventChunk, ID: "4", Data: "{}"}}, true)
	watcher.Mirror("conv", []SSEEvent{{Event: EventChunk, ID: "5", Data: "{}"}}, false)

	if got := watcher.LatestSeq("conv"); got != 5 {
		t.Fatalf("latest seq = %d, want the owner's 5 after a replay replaced the local buffer", got)
	}
}

func TestMirrorDropsDuplicatesAndFansOutNewEvents(t *testing.T) {
	watcher := NewConnectionManager(time.Minute)
	conn := connect(watcher, "conv")

	watcher.Mirror("conv", []SSEEvent{{Event: EventChunk, ID: "1", Data: "{}"}, {Event: EventChunk, ID: "2", Data: "{}"}}, true)
	watcher.Mirror("conv", []SSEEvent{{Event: EventChunk, ID: "2", Data: "{}"}, {Event: EventChunk, ID: "3", Data: "{}"}}, false)

	got := drain(conn)
	if len(got) != 3 || got[0].ID != "1" || got[1].ID != "2" || got[2].ID != "3" {
		t.Fatalf("connection received %+v, want ids 1, 2, 3 once each", got)
	}
}

func TestMirrorReleasesALiveConnectionAtAGap(t *testing.T) {
	watcher := NewConnectionManager(time.Minute)
	conn := connect(watcher, "conv")
	watcher.Mirror("conv", []SSEEvent{{Event: EventChunk, ID: "1", Data: "{}"}}, true)
	drain(conn)

	watcher.Mirror("conv", []SSEEvent{{Event: EventChunk, ID: "4", Data: "{}"}}, false)

	got := drain(conn)
	if len(got) == 0 || got[0].Event != EventFinish || got[0].ID != "3" {
		t.Fatalf("connection received %+v, want a finish with id 3 so the client reconciles", got)
	}
	select {
	case <-conn.Done:
	default:
		t.Fatal("the connection that rendered around the gap must be closed")
	}
	if seq := watcher.LatestSeq("conv"); seq != 4 {
		t.Fatalf("latest seq = %d, want 4: the buffer restarts at the event after the gap", seq)
	}
}

func TestReleaseMirroredFinishesAndClosesTheConnection(t *testing.T) {
	watcher := NewConnectionManager(time.Minute)
	conn := connect(watcher, "conv")
	watcher.Mirror("conv", []SSEEvent{{Event: EventChunk, ID: "5", Data: "{}"}}, true)
	drain(conn)

	if !watcher.ReleaseMirrored("conv") {
		t.Fatal("ReleaseMirrored reported nothing released for a turn still streaming")
	}

	got := drain(conn)
	if len(got) != 1 || got[0].Event != EventFinish || got[0].ID != "5" {
		t.Fatalf("connection received %+v, want one finish with the last mirrored id 5", got)
	}
	select {
	case <-conn.Done:
	default:
		t.Fatal("ReleaseMirrored left the connection open")
	}
}

func TestReleaseMirroredKeepsTheConnectionWhenTheTurnEnded(t *testing.T) {
	watcher := NewConnectionManager(time.Minute)
	conn := connect(watcher, "conv")
	watcher.Mirror("conv", []SSEEvent{{Event: EventChunk, ID: "5", Data: "{}"}, {Event: EventFinish, ID: "6", Data: "{}"}}, true)
	drain(conn)

	if watcher.ReleaseMirrored("conv") {
		t.Fatal("ReleaseMirrored released a turn that already finished")
	}
	if got := drain(conn); len(got) != 0 {
		t.Fatalf("connection received %+v after a finished turn, want nothing", got)
	}
	select {
	case <-conn.Done:
		t.Fatal("ReleaseMirrored closed the stream of a finished turn")
	default:
	}
}

func TestCloseMirroredDoesNotRelay(t *testing.T) {
	cm, rec := relayedManager()
	cm.Watch("conv", "pod-b", false)
	conn := connect(cm, "conv")

	cm.CloseMirrored("conv")

	if n := len(rec.snapshot()); n != 0 {
		t.Fatalf("closing a mirrored conversation relayed %d messages, want 0", n)
	}
	select {
	case <-conn.Done:
	default:
		t.Fatal("CloseMirrored left the connection open")
	}
}
