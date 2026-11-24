package web

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/cluster"
	"github.com/astropods/messaging/internal/store"
	"github.com/astropods/messaging/internal/store/chatstore"
	"github.com/astropods/messaging/internal/testpg"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

const sidecarUser = "user-1"

// sidecar is one messaging sidecar on the shared test database, with a recording agent.
type sidecar struct {
	pod string
	a   *WebAdapter
	st  *chatstore.Store
	srv *httptest.Server

	mu       sync.Mutex
	sent     []*pb.Message
	feedback []*pb.PlatformFeedback
}

func (s *sidecar) feedbackOfKind(match func(*pb.PlatformFeedback) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, fb := range s.feedback {
		if match(fb) {
			n++
		}
	}
	return n
}

func isStop(fb *pb.PlatformFeedback) bool {
	return fb.GetStreamControl().GetAction() == pb.StreamControl_STOP
}

func isRenderableResponse(fb *pb.PlatformFeedback) bool { return fb.GetRenderableResponse() != nil }

func newSidecar(t *testing.T, url, pod string) *sidecar {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	st, err := chatstore.OpenPostgres(ctx, url)
	if err != nil {
		t.Fatalf("%s: open store: %v", pod, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.SetPodID(pod)

	s := &sidecar{pod: pod, st: st}
	s.a = New(WithSessionManager(NewHeaderSessionManager("X-User-ID", "", "")), WithInteractionStore(st))
	if err := s.a.Initialize(ctx, adapter.Config{}); err != nil {
		t.Fatalf("%s: initialize: %v", pod, err)
	}
	s.a.SetChatStore(st)
	bus := cluster.New(url, pod)
	busDone := make(chan struct{})
	go func() {
		bus.Run(ctx)
		close(busDone)
	}()
	t.Cleanup(func() { cancel(); <-busDone })
	s.a.SetCluster(bus)
	s.a.SetMessageHandler(func(_ context.Context, msg *pb.Message) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.sent = append(s.sent, msg)
		return nil
	})
	s.a.SetFeedbackHandler(func(_ context.Context, fb *pb.PlatformFeedback) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.feedback = append(s.feedback, fb)
		return nil
	})
	s.a.connManager.Start(ctx)
	go s.a.runCluster(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /c/{id}/stream", s.a.handlers.HandleStream)
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// newSidecarPair starts two sidecars and waits until each hears the other on the bus.
func newSidecarPair(t *testing.T) (*sidecar, *sidecar) {
	t.Helper()
	s := newSidecars(t, "pod-a", "pod-b")
	return s[0], s[1]
}

// newSidecars starts one sidecar per pod and waits until each hears every other on the bus.
func newSidecars(t *testing.T, pods ...string) []*sidecar {
	t.Helper()
	url := testpg.URL(t)
	all := make([]*sidecar, 0, len(pods))
	for _, pod := range pods {
		all = append(all, newSidecar(t, url, pod))
	}
	for _, from := range all {
		for _, to := range all {
			if from != to {
				waitHeard(t, from, to)
			}
		}
	}
	return all
}

func waitHeard(t *testing.T, from, to *sidecar) {
	t.Helper()
	kind := "test.ping." + from.pod
	var heard sync.Once
	got := make(chan struct{})
	to.a.cluster.Handle(kind, func(cluster.Envelope) { heard.Do(func() { close(got) }) })
	deadline := time.After(10 * time.Second)
	for {
		from.a.cluster.Publish(kind, "", nil)
		select {
		case <-got:
			return
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			t.Fatalf("%s never heard %s on the bus", to.pod, from.pod)
		}
	}
}

func (s *sidecar) send(t *testing.T, conversationID, content string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.a.handlers.HandleSendMessage(w, sendMessageRequest(sidecarUser, conversationID, content))
	return w
}

func (s *sidecar) agentSays(t *testing.T, conversationID string, kind pb.ContentChunk_ChunkType, text string) {
	t.Helper()
	if err := s.a.HandleAgentResponse(context.Background(), &pb.AgentResponse{
		ConversationId: conversationID,
		ResponseId:     "r-" + conversationID,
		Payload:        &pb.AgentResponse_Content{Content: &pb.ContentChunk{Type: kind, Content: text}},
	}); err != nil {
		t.Fatalf("%s: agent response: %v", s.pod, err)
	}
}

func (s *sidecar) answer(t *testing.T, conversationID, interactionID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/chat/conversations/"+conversationID+"/interactions/"+interactionID, strings.NewReader(body))
	req.Header.Set("X-User-ID", sidecarUser)
	req.SetPathValue("id", conversationID)
	req.SetPathValue("interactionId", interactionID)
	w := httptest.NewRecorder()
	s.a.handlers.HandleInteractionResponse(w, req)
	return w
}

func seedConversation(t *testing.T, s *sidecar, conversationID string) {
	t.Helper()
	if _, err := s.st.EnsureForSend(t.Context(), conversationID, sidecarUser, "t"); err != nil {
		t.Fatalf("seed %s: %v", conversationID, err)
	}
}

// sseStream reads one SSE connection on a sidecar's HTTP server into a channel of raw events.
type sseStream struct {
	events chan string
	cancel context.CancelFunc
}

func openStream(t *testing.T, s *sidecar, conversationID, lastEventID string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.srv.URL+"/c/"+conversationID+"/stream", nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	req.Header.Set("X-User-ID", sidecarUser)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // the reader goroutine closes it
	if err != nil {
		cancel()
		t.Fatalf("open stream on %s: %v", s.pod, err)
	}
	st := &sseStream{events: make(chan string, 256), cancel: cancel}
	go func() {
		defer close(st.events)
		defer resp.Body.Close() //nolint:errcheck
		sc := bufio.NewScanner(resp.Body)
		var ev strings.Builder
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if ev.Len() > 0 {
					st.events <- ev.String()
					ev.Reset()
				}
				continue
			}
			ev.WriteString(line + "\n")
		}
	}()
	t.Cleanup(cancel)
	return st
}

// waitEvent returns the first event containing want, failing after the timeout or when the stream ends.
func (st *sseStream) waitEvent(t *testing.T, want string) string {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-st.events:
			if !ok {
				t.Fatalf("stream ended before an event containing %q", want)
			}
			if strings.Contains(ev, want) {
				return ev
			}
		case <-timeout:
			t.Fatalf("no event containing %q", want)
		}
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestStreamOnOneSidecarFollowsATurnTheOtherStartsLater(t *testing.T) {
	a, b := newSidecarPair(t)
	seedConversation(t, a, "conv")

	stream := openStream(t, b, "conv", "")
	stream.waitEvent(t, "event: connected")
	waitUntil(t, "pod-b to register the stream", func() bool { return b.a.connManager.GetConnectionCount("conv") > 0 })

	if w := a.send(t, "conv", "hi"); w.Code != http.StatusOK {
		t.Fatalf("send on pod-a = %d %s", w.Code, w.Body.String())
	}
	a.agentSays(t, "conv", pb.ContentChunk_START, "")
	a.agentSays(t, "conv", pb.ContentChunk_DELTA, "hello from pod-a")

	stream.waitEvent(t, "hello from pod-a")
	a.agentSays(t, "conv", pb.ContentChunk_END, "")
	stream.waitEvent(t, "event: "+EventFinish)
}

func TestStreamResumesOnTheOtherSidecarFromTheOwnersEventID(t *testing.T) {
	a, b := newSidecarPair(t)
	seedConversation(t, a, "conv")
	if w := a.send(t, "conv", "hi"); w.Code != http.StatusOK {
		t.Fatalf("send on pod-a = %d %s", w.Code, w.Body.String())
	}
	a.agentSays(t, "conv", pb.ContentChunk_START, "")
	a.agentSays(t, "conv", pb.ContentChunk_DELTA, "seen-before-disconnect ")
	cursor := a.a.connManager.LatestSeq("conv")
	a.agentSays(t, "conv", pb.ContentChunk_DELTA, "missed-while-away")
	waitUntil(t, "pod-a's owner row", func() bool {
		owner, _ := b.st.TurnOwner(t.Context(), "conv")
		return owner == "pod-a"
	})

	stream := openStream(t, b, "conv", strconv.FormatUint(cursor, 10))
	ev := stream.waitEvent(t, "missed-while-away")
	if !strings.Contains(ev, "id: "+strconv.FormatUint(cursor+1, 10)) {
		t.Fatalf("replayed event %q, want the owner's id %d", ev, cursor+1)
	}

	a.agentSays(t, "conv", pb.ContentChunk_DELTA, "live-after-resume")
	stream.waitEvent(t, "live-after-resume")
	a.agentSays(t, "conv", pb.ContentChunk_END, "")
	stream.waitEvent(t, "event: "+EventFinish)
}

func TestSendOnTheOtherSidecarIsRefusedWhileATurnRuns(t *testing.T) {
	a, b := newSidecarPair(t)
	seedConversation(t, a, "conv")
	if w := a.send(t, "conv", "first"); w.Code != http.StatusOK {
		t.Fatalf("send on pod-a = %d %s", w.Code, w.Body.String())
	}
	a.agentSays(t, "conv", pb.ContentChunk_START, "")

	w := b.send(t, "conv", "second")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "turn_in_progress") {
		t.Fatalf("send on pod-b during pod-a's turn = %d %s, want 409 turn_in_progress", w.Code, w.Body.String())
	}
	b.mu.Lock()
	forwarded := len(b.sent)
	b.mu.Unlock()
	if forwarded != 0 {
		t.Fatal("pod-b forwarded the refused send to its agent")
	}
}

func TestConversationReadOnTheOtherSidecarShowsTheRemoteTurnStreaming(t *testing.T) {
	a, b := newSidecarPair(t)
	seedConversation(t, a, "conv")
	if w := a.send(t, "conv", "hi"); w.Code != http.StatusOK {
		t.Fatalf("send on pod-a = %d %s", w.Code, w.Body.String())
	}
	a.agentSays(t, "conv", pb.ContentChunk_START, "")
	a.agentSays(t, "conv", pb.ContentChunk_DELTA, "partial")

	for _, tc := range []struct {
		name string
		call func(http.ResponseWriter, *http.Request)
		path string
	}{
		{"list", b.a.handlers.HandleListChatConversations, "/api/chat/conversations"},
		{"get", b.a.handlers.HandleGetChatConversation, "/api/chat/conversations/conv"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("X-User-ID", sidecarUser)
		req.SetPathValue("id", "conv")
		w := httptest.NewRecorder()
		tc.call(w, req)
		if !strings.Contains(w.Body.String(), `"assistant_streaming":true`) {
			t.Errorf("%s on pod-b = %s, want assistant_streaming true for pod-a's live turn", tc.name, w.Body.String())
		}
	}
}

func TestStopOnTheOtherSidecarStopsTheOwnersTurn(t *testing.T) {
	a, b := newSidecarPair(t)
	seedConversation(t, a, "conv")
	if w := a.send(t, "conv", "hi"); w.Code != http.StatusOK {
		t.Fatalf("send on pod-a = %d %s", w.Code, w.Body.String())
	}
	a.agentSays(t, "conv", pb.ContentChunk_START, "")
	a.agentSays(t, "conv", pb.ContentChunk_DELTA, "partial reply")
	waitUntil(t, "pod-a's owner row", func() bool {
		owner, _ := b.st.TurnOwner(t.Context(), "conv")
		return owner == "pod-a"
	})

	start := time.Now()
	w := httptest.NewRecorder()
	b.a.handlers.HandleCancel(w, cancelRequest(sidecarUser, "conv"))
	if w.Code != http.StatusNoContent {
		t.Fatalf("cancel on pod-b = %d %s", w.Code, w.Body.String())
	}
	if elapsed := time.Since(start); elapsed >= stopAckWait {
		t.Fatalf("cancel took %s, want the owner's acknowledgement before the %s fallback", elapsed, stopAckWait)
	}

	if n := a.feedbackOfKind(isStop); n != 1 {
		t.Fatalf("pod-a's agent got %d stop signals, want 1", n)
	}
	if n := b.feedbackOfKind(isStop); n != 0 {
		t.Fatalf("pod-b's agent got %d stop signals, want 0: the owner acknowledged", n)
	}
	msgs, err := a.st.ListMessages(t.Context(), "conv")
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if last := msgs[len(msgs)-1]; last.Role != "assistant" || last.Content != "partial reply" {
		t.Fatalf("last message = %+v, want the stopped partial reply", last)
	}
	waitUntil(t, "pod-a to release the stopped turn", func() bool {
		owner, _ := b.st.TurnOwner(t.Context(), "conv")
		return owner == ""
	})
}

func TestInteractionAnswerOnTheOtherSidecarReachesTheOwnersAgent(t *testing.T) {
	a, b := newSidecarPair(t)
	seedConversation(t, a, "conv")
	if w := a.send(t, "conv", "hi"); w.Code != http.StatusOK {
		t.Fatalf("send on pod-a = %d %s", w.Code, w.Body.String())
	}
	a.agentSays(t, "conv", pb.ContentChunk_START, "")
	if err := a.a.HandleAgentResponse(context.Background(), &pb.AgentResponse{
		ConversationId: "conv",
		Payload: &pb.AgentResponse_Renderable{Renderable: testRenderable("i1",
			pb.RenderableAction_RENDERABLE_ACTION_DECLINE, pb.RenderableAction_RENDERABLE_ACTION_CANCEL)},
	}); err != nil {
		t.Fatalf("renderable: %v", err)
	}
	waitUntil(t, "pod-a's owner row", func() bool {
		owner, _ := b.st.TurnOwner(t.Context(), "conv")
		return owner == "pod-a"
	})

	if w := b.answer(t, "conv", "i1", `{"action":"decline"}`); w.Code != http.StatusOK {
		t.Fatalf("answer on pod-b = %d %s", w.Code, w.Body.String())
	}

	if n := a.feedbackOfKind(isRenderableResponse); n != 1 {
		t.Fatalf("pod-a's agent got %d answers when pod-b's answer returned, want 1: pod-b waits for the owner's result", n)
	}
	if n := b.feedbackOfKind(isRenderableResponse); n != 0 {
		t.Fatalf("pod-b's agent got %d answers, want 0: the turn runs on pod-a", n)
	}

	if w := b.answer(t, "conv", "i1", `{"action":"decline"}`); w.Code != http.StatusOK {
		t.Fatalf("retried answer on pod-b = %d %s", w.Code, w.Body.String())
	}
	if n := a.feedbackOfKind(isRenderableResponse); n != 1 {
		t.Fatalf("pod-a's agent got %d answers after a retry, want 1: an answered interaction is never forwarded again", n)
	}
}

func TestInteractionAnsweredOnTheOwnerIsNotRedeliveredByARetryOnTheOtherSidecar(t *testing.T) {
	a, b := newSidecarPair(t)
	seedConversation(t, a, "conv")
	if w := a.send(t, "conv", "hi"); w.Code != http.StatusOK {
		t.Fatalf("send on pod-a = %d %s", w.Code, w.Body.String())
	}
	a.agentSays(t, "conv", pb.ContentChunk_START, "")
	if err := a.a.HandleAgentResponse(context.Background(), &pb.AgentResponse{
		ConversationId: "conv",
		Payload: &pb.AgentResponse_Renderable{Renderable: testRenderable("i1",
			pb.RenderableAction_RENDERABLE_ACTION_DECLINE, pb.RenderableAction_RENDERABLE_ACTION_CANCEL)},
	}); err != nil {
		t.Fatalf("renderable: %v", err)
	}
	waitUntil(t, "pod-a's owner row", func() bool {
		owner, _ := b.st.TurnOwner(t.Context(), "conv")
		return owner == "pod-a"
	})

	if w := a.answer(t, "conv", "i1", `{"action":"decline"}`); w.Code != http.StatusOK {
		t.Fatalf("answer on pod-a = %d %s", w.Code, w.Body.String())
	}
	if w := b.answer(t, "conv", "i1", `{"action":"decline"}`); w.Code != http.StatusOK {
		t.Fatalf("re-POST on pod-b = %d %s", w.Code, w.Body.String())
	}

	time.Sleep(answerAckWait)
	if n := a.feedbackOfKind(isRenderableResponse); n != 1 {
		t.Fatalf("pod-a's agent got %d answers, want 1: a re-POST on pod-b must not deliver the answer again", n)
	}
}

func TestInteractionAnswerIsRefusedWhenTheOwnerNeverConfirms(t *testing.T) {
	b := newSidecar(t, testpg.URL(t), "pod-b")
	seedConversation(t, b, "conv")
	if _, err := b.st.AppendInteraction(t.Context(), "conv", sidecarUser, testRenderable("i1",
		pb.RenderableAction_RENDERABLE_ACTION_DECLINE, pb.RenderableAction_RENDERABLE_ACTION_CANCEL)); err != nil {
		t.Fatalf("append interaction: %v", err)
	}
	if ok, err := b.st.ClaimTurnOwner(t.Context(), "conv", "pod-gone"); err != nil || !ok {
		t.Fatalf("claim for pod-gone = %v, %v", ok, err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		w := b.answer(t, "conv", "i1", `{"action":"decline"}`)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("attempt %d with a silent owner = %d %s, want 503 so the client retries", attempt, w.Code, w.Body.String())
		}
	}
	it, _, err := b.st.GetInteraction(t.Context(), "conv", "i1")
	if err != nil {
		t.Fatalf("get interaction: %v", err)
	}
	if it.Status != store.InteractionPending {
		t.Fatalf("interaction status = %s, want pending: only the owner records a forwarded answer", it.Status)
	}
	if n := b.feedbackOfKind(isRenderableResponse); n != 0 {
		t.Fatalf("pod-b's agent got %d answers, want 0: the turn runs on pod-gone", n)
	}
}

func TestStreamOnTheOtherSidecarFinishesWhenTheOwnerIsGone(t *testing.T) {
	b := newSidecar(t, testpg.URL(t), "pod-b")
	seedConversation(t, b, "conv")
	if ok, err := b.st.ClaimTurnOwner(t.Context(), "conv", "pod-gone"); err != nil || !ok {
		t.Fatalf("claim for pod-gone = %v, %v", ok, err)
	}

	stream := openStream(t, b, "conv", "")
	stream.waitEvent(t, "event: connected")
	waitUntil(t, "pod-b to watch pod-gone's turn", func() bool { return b.a.handlers.clusterState.isWatching("conv") })

	b.a.renewWatches(t.Context())
	if n := b.a.connManager.GetConnectionCount("conv"); n != 1 {
		t.Fatalf("connections after a renewal with a live owner = %d, want 1", n)
	}

	if err := b.st.SyncTurnOwners(t.Context(), "pod-gone", nil); err != nil {
		t.Fatalf("drop pod-gone's owner row: %v", err)
	}
	b.a.renewWatches(t.Context())

	stream.waitEvent(t, "event: finish")
	if b.a.handlers.clusterState.isWatching("conv") {
		t.Fatal("pod-b still watches a turn with no live owner")
	}
	if n := b.a.connManager.GetConnectionCount("conv"); n != 0 {
		t.Fatalf("connections after the owner is gone = %d, want 0", n)
	}
}

func TestStreamFollowsTheNextTurnWhenAnotherSidecarRunsIt(t *testing.T) {
	s := newSidecars(t, "pod-a", "pod-b", "pod-c")
	a, b, c := s[0], s[1], s[2]
	seedConversation(t, a, "conv")

	stream := openStream(t, c, "conv", "")
	stream.waitEvent(t, "event: connected")
	waitUntil(t, "pod-c to register the stream", func() bool { return c.a.connManager.GetConnectionCount("conv") > 0 })

	if w := a.send(t, "conv", "first"); w.Code != http.StatusOK {
		t.Fatalf("send on pod-a = %d %s", w.Code, w.Body.String())
	}
	a.agentSays(t, "conv", pb.ContentChunk_START, "")
	a.agentSays(t, "conv", pb.ContentChunk_DELTA, "turn one from pod-a")
	stream.waitEvent(t, "turn one from pod-a")
	a.agentSays(t, "conv", pb.ContentChunk_END, "")
	stream.waitEvent(t, "event: "+EventFinish)
	waitUntil(t, "pod-a to release its turn", func() bool {
		owner, _ := b.st.TurnOwner(t.Context(), "conv")
		return owner == ""
	})

	if w := b.send(t, "conv", "second"); w.Code != http.StatusOK {
		t.Fatalf("send on pod-b = %d %s", w.Code, w.Body.String())
	}
	waitUntil(t, "pod-c to watch pod-b's turn", func() bool { return c.a.handlers.clusterState.watchedOwner("conv") == "pod-b" })
	b.agentSays(t, "conv", pb.ContentChunk_START, "")
	b.agentSays(t, "conv", pb.ContentChunk_DELTA, "turn two from pod-b")
	stream.waitEvent(t, "turn two from pod-b")
}

func TestStreamOnTheOtherSidecarStaysOpenAfterTheOwnersTurnEnds(t *testing.T) {
	a, b := newSidecarPair(t)
	seedConversation(t, a, "conv")

	stream := openStream(t, b, "conv", "")
	stream.waitEvent(t, "event: connected")
	waitUntil(t, "pod-b to register the stream", func() bool { return b.a.connManager.GetConnectionCount("conv") > 0 })

	if w := a.send(t, "conv", "hi"); w.Code != http.StatusOK {
		t.Fatalf("send on pod-a = %d %s", w.Code, w.Body.String())
	}
	a.agentSays(t, "conv", pb.ContentChunk_START, "")
	a.agentSays(t, "conv", pb.ContentChunk_DELTA, "hello")
	a.agentSays(t, "conv", pb.ContentChunk_END, "")
	stream.waitEvent(t, "event: "+EventFinish)
	waitUntil(t, "pod-a to release its turn", func() bool {
		owner, _ := b.st.TurnOwner(t.Context(), "conv")
		return owner == ""
	})

	b.a.renewWatches(t.Context())

	if b.a.handlers.clusterState.isWatching("conv") {
		t.Fatal("pod-b still watches pod-a after its turn ended")
	}
	if n := b.a.connManager.GetConnectionCount("conv"); n != 1 {
		t.Fatalf("connections after the turn ended = %d, want 1: a finished turn keeps the stream for the next one", n)
	}
}
