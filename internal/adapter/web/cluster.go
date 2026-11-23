package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/astropods/messaging/internal/cluster"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
)

// ownerHeartbeat keeps owner rows well inside chatstore.TurnOwnerTTL.
const ownerHeartbeat = 10 * time.Second

// replayWait bounds how long a subscribe on a non-owner waits for the owner's buffered events.
const replayWait = 2 * time.Second

// orphanReapInterval closes turns whose sidecar died, which no startup reap covers on a shared store.
const orphanReapInterval = time.Minute

// stopAckWait bounds how long a forwarded stop waits before stopping here instead.
const stopAckWait = 2 * time.Second

// Bus is the part of cluster.Bus the web adapter uses.
type Bus interface {
	Pod() string
	Publish(kind, conversationID string, data any)
	Handle(kind string, fn func(cluster.Envelope))
}

type stopMessage struct {
	To   string   `json:"to"`
	ID   string   `json:"id"`
	User *pb.User `json:"user"`
}

type stoppedMessage struct {
	To string `json:"to"`
	ID string `json:"id"`
}

type answerMessage struct {
	To            string                 `json:"to"`
	UserID        string                 `json:"user_id"`
	InteractionID string                 `json:"interaction_id"`
	Action        pb.RenderableAction    `json:"action"`
	Text          string                 `json:"text"`
	Response      *pb.RenderableResponse `json:"-"`
	ResponseJSON  json.RawMessage        `json:"response"`
}

func (m answerMessage) MarshalJSON() ([]byte, error) {
	type wire answerMessage
	w := wire(m)
	if m.Response != nil {
		raw, err := protojson.Marshal(m.Response)
		if err != nil {
			return nil, err
		}
		w.ResponseJSON = raw
	}
	return json.Marshal(w)
}

// watchMessage with Replay asks the owner named by To for the live turn's buffered events.
// Without Replay it renews an existing watch with whichever sidecar runs the turn.
type watchMessage struct {
	To     string `json:"to,omitempty"`
	Replay bool   `json:"replay,omitempty"`
}

type turnStartedMessage struct{}

// clusterState is what a non-owner tracks about conversations it watches on another sidecar.
type clusterState struct {
	mu       sync.Mutex
	watching map[string]time.Time
	replayed map[string][]chan struct{}
	acks     map[string]chan struct{}
	owned    map[string]bool
}

func newClusterState() *clusterState {
	return &clusterState{
		watching: map[string]time.Time{},
		replayed: map[string][]chan struct{}{},
		acks:     map[string]chan struct{}{},
		owned:    map[string]bool{},
	}
}

func (c *clusterState) awaitAck(id string) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan struct{})
	c.acks[id] = ch
	return ch
}

func (c *clusterState) signalAck(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch, ok := c.acks[id]; ok {
		close(ch)
		delete(c.acks, id)
	}
}

func (c *clusterState) dropAck(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.acks, id)
}

// startedTurns records the owned set and returns the conversations that were not owned before.
func (c *clusterState) startedTurns(owned []string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(map[string]bool, len(owned))
	var started []string
	for _, id := range owned {
		next[id] = true
		if !c.owned[id] {
			started = append(started, id)
		}
	}
	c.owned = next
	return started
}

func (c *clusterState) markWatching(conversationID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.watching[conversationID] = time.Now().Add(watchTTL)
}

func (c *clusterState) isWatching(conversationID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.watching[conversationID]
	if ok && time.Now().After(exp) {
		delete(c.watching, conversationID)
		return false
	}
	return ok
}

func (c *clusterState) awaitReplay(conversationID string) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan struct{})
	c.replayed[conversationID] = append(c.replayed[conversationID], ch)
	return ch
}

func (c *clusterState) signalReplay(conversationID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range c.replayed[conversationID] {
		close(ch)
	}
	delete(c.replayed, conversationID)
}

// SetCluster connects this adapter to the other sidecars on the shared chat store.
// Call after Initialize and SetChatStore, before Start.
func (a *WebAdapter) SetCluster(bus Bus) {
	if bus == nil || a.handlers == nil || a.chatStore == nil || !a.chatStore.Shared() {
		return
	}
	a.cluster = bus
	a.handlers.cluster = bus
	a.handlers.clusterState = newClusterState()
	a.turnsChanged = a.turns.watchChanges()
	a.connManager.SetRelay(bus.Publish)

	h := a.handlers
	bus.Handle(busSSE, func(env cluster.Envelope) {
		if !h.clusterState.isWatching(env.Conversation) {
			return
		}
		var m sseMessage
		if err := json.Unmarshal(env.Data, &m); err != nil {
			slog.Warn("web cluster: bad sse message dropped", "error", err)
			return
		}
		a.connManager.Mirror(env.Conversation, m.Events, false)
	})
	bus.Handle(busClose, func(env cluster.Envelope) {
		if h.clusterState.isWatching(env.Conversation) {
			a.connManager.CloseMirrored(env.Conversation)
		}
	})
	bus.Handle(busWatch, func(env cluster.Envelope) {
		var m watchMessage
		if err := json.Unmarshal(env.Data, &m); err != nil {
			return
		}
		switch {
		case m.Replay && m.To == bus.Pod():
			a.connManager.Watch(env.Conversation, env.From, true)
		case !m.Replay && a.turns.isStreaming(env.Conversation):
			a.connManager.Watch(env.Conversation, env.From, false)
		}
	})
	bus.Handle(busTurnStarted, func(env cluster.Envelope) {
		if a.connManager.GetConnectionCount(env.Conversation) > 0 {
			go h.watchRemote(context.Background(), env.Conversation, env.From)
		}
	})
	bus.Handle(busStopped, func(env cluster.Envelope) {
		var m stoppedMessage
		if err := json.Unmarshal(env.Data, &m); err == nil && m.To == bus.Pod() {
			h.clusterState.signalAck(m.ID)
		}
	})
	bus.Handle(busReplay, func(env cluster.Envelope) {
		var m replayMessage
		if err := json.Unmarshal(env.Data, &m); err != nil {
			slog.Warn("web cluster: bad replay message dropped", "error", err)
			return
		}
		if m.To != bus.Pod() {
			return
		}
		a.connManager.Mirror(env.Conversation, m.Events, true)
		h.clusterState.signalReplay(env.Conversation)
	})
	bus.Handle(busStop, func(env cluster.Envelope) {
		var m stopMessage
		if err := json.Unmarshal(env.Data, &m); err != nil || m.To != bus.Pod() {
			return
		}
		go func() {
			h.stopTurn(context.Background(), env.Conversation, m.User)
			bus.Publish(busStopped, env.Conversation, stoppedMessage{To: env.From, ID: m.ID})
		}()
	})
	bus.Handle(busAnswer, func(env cluster.Envelope) {
		var m answerMessage
		if err := json.Unmarshal(env.Data, &m); err != nil || m.To != bus.Pod() {
			return
		}
		resp := &pb.RenderableResponse{}
		if err := protojson.Unmarshal(m.ResponseJSON, resp); err != nil {
			slog.Warn("web cluster: bad interaction answer dropped", "conversation_id", env.Conversation, "error", err)
			return
		}
		go h.forwardedAnswer(env.Conversation, m, resp)
	})
}

func (h *Handlers) forwardedAnswer(conversationID string, m answerMessage, resp *pb.RenderableResponse) {
	ctx := context.Background()
	it, found, err := h.interactions.GetInteraction(ctx, conversationID, m.InteractionID)
	if err != nil || !found {
		slog.Warn("web cluster: answered interaction not found", "conversation_id", conversationID, "interaction_id", m.InteractionID, "error", err)
		return
	}
	h.applyAnswer(ctx, conversationID, m.UserID, m.Action, it.Renderable, resp, m.Text)
}

// runCluster keeps this sidecar's owner rows and watches current until ctx ends.
func (a *WebAdapter) runCluster(ctx context.Context) {
	if a.cluster == nil {
		return
	}
	owners := time.NewTicker(ownerHeartbeat)
	defer owners.Stop()
	reaps := time.NewTicker(orphanReapInterval)
	defer reaps.Stop()
	watches := time.NewTicker(watchRenewInterval)
	defer watches.Stop()
	a.syncOwners(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.turnsChanged:
			a.syncOwners(ctx)
		case <-owners.C:
			a.syncOwners(ctx)
		case <-watches.C:
			a.renewWatches(ctx)
		case <-reaps.C:
			if n, err := a.chatStore.ReapOrphanedTurns(ctx); err != nil {
				slog.Warn("web cluster: reap orphaned turns failed", "error", err)
			} else if n > 0 {
				slog.Info("web cluster: finalized orphaned turns", "count", n)
			}
		}
	}
}

func (a *WebAdapter) syncOwners(ctx context.Context) {
	owned := a.turns.ownedConversations()
	if err := a.chatStore.SyncTurnOwners(ctx, a.cluster.Pod(), owned); err != nil {
		slog.Warn("web cluster: sync turn owners failed", "error", err)
	}
	for _, id := range a.handlers.clusterState.startedTurns(owned) {
		a.cluster.Publish(busTurnStarted, id, turnStartedMessage{})
	}
}

// renewWatches keeps current watches alive and starts one for any connected
// conversation whose turn another sidecar runs, in case its turn start was missed.
func (a *WebAdapter) renewWatches(ctx context.Context) {
	h := a.handlers
	var unwatched []string
	for _, id := range a.connManager.ConnectedConversations() {
		if h.clusterState.isWatching(id) {
			a.cluster.Publish(busWatch, id, watchMessage{})
			h.clusterState.markWatching(id)
		} else {
			unwatched = append(unwatched, id)
		}
	}
	if len(unwatched) == 0 {
		return
	}
	remote := h.remoteTurns(ctx)
	for _, id := range unwatched {
		if owner, ok := remote[id]; ok {
			go h.watchRemote(context.Background(), id, owner)
		}
	}
}

// remoteOwner names the other sidecar running the conversation's turn, or "" when this one
// runs it or none does. A lookup failure reads as "", so the request is handled here.
func (h *Handlers) remoteOwner(ctx context.Context, conversationID string) string {
	if h.cluster == nil {
		return ""
	}
	if h.turns != nil && h.turns.isStreaming(conversationID) {
		return ""
	}
	pod, err := h.chatStore.TurnOwner(ctx, conversationID)
	if err != nil {
		slog.Warn("web cluster: turn owner lookup failed", "conversation_id", conversationID, "error", err)
		return ""
	}
	if pod == h.cluster.Pod() {
		return ""
	}
	return pod
}

// streaming reports whether any sidecar is running the conversation's turn.
func (h *Handlers) streaming(ctx context.Context, conversationID string) bool {
	if h.turns != nil && h.turns.isStreaming(conversationID) {
		return true
	}
	return h.remoteOwner(ctx, conversationID) != ""
}

// remoteTurns maps conversations whose turn another sidecar runs to that sidecar.
func (h *Handlers) remoteTurns(ctx context.Context) map[string]string {
	if h.cluster == nil {
		return nil
	}
	owners, err := h.chatStore.LiveTurnOwners(ctx)
	if err != nil {
		slog.Warn("web cluster: live turn owners lookup failed", "error", err)
		return nil
	}
	for id, pod := range owners {
		if pod == h.cluster.Pod() {
			delete(owners, id)
		}
	}
	return owners
}

// watchRemote asks the owner to relay the conversation here and waits briefly for its buffered events.
func (h *Handlers) watchRemote(ctx context.Context, conversationID, owner string) {
	if h.clusterState.isWatching(conversationID) {
		return
	}
	done := h.clusterState.awaitReplay(conversationID)
	h.clusterState.markWatching(conversationID)
	h.cluster.Publish(busWatch, conversationID, watchMessage{To: owner, Replay: true})
	t := time.NewTimer(replayWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		slog.Warn("web cluster: no replay from turn owner", "conversation_id", conversationID)
	case <-ctx.Done():
	}
}

// claimClusterTurn takes the turn across sidecars, writing the refusal when another sidecar holds it.
func (h *Handlers) claimClusterTurn(ctx context.Context, w http.ResponseWriter, conversationID string) bool {
	if h.cluster == nil {
		return true
	}
	ok, err := h.chatStore.ClaimTurnOwner(ctx, conversationID, h.cluster.Pod())
	if err != nil {
		slog.Error("web cluster: claim turn owner failed", "conversation_id", conversationID, "error", err)
		http.Error(w, "chat temporarily unavailable", http.StatusServiceUnavailable)
		return false
	}
	if !ok {
		writeTurnInProgress(w)
		return false
	}
	return true
}

// forwardStop asks the owner to stop the turn, and stops it here when the owner doesn't confirm in time.
func (h *Handlers) forwardStop(ctx context.Context, conversationID, owner string, user *pb.User) {
	id := uuid.NewString()
	done := h.clusterState.awaitAck(id)
	defer h.clusterState.dropAck(id)
	h.cluster.Publish(busStop, conversationID, stopMessage{To: owner, ID: id, User: user})
	t := time.NewTimer(stopAckWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		slog.Warn("web cluster: turn owner did not confirm stop, stopping here", "conversation_id", conversationID, "owner", owner)
		h.stopTurn(ctx, conversationID, user)
	case <-ctx.Done():
	}
}
