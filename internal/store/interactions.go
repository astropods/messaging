package store

import (
	"context"
	"errors"
	"sync"
	"time"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

var ErrInteractionNotFound = errors.New("interaction not found")

type InteractionStatus string

const (
	InteractionPending   InteractionStatus = "pending"
	InteractionSubmitted InteractionStatus = "submitted"
	InteractionDeclined  InteractionStatus = "declined"
	InteractionCancelled InteractionStatus = "cancelled"
	InteractionResponded InteractionStatus = "responded"
)

// Interaction is a Renderable and its response lifecycle.
type Interaction struct {
	ID             string
	ConversationID string
	UserID         string // authorized responder (the conversation's user)
	// Seq orders the interaction; the SQLite backing allocates it in the space
	// shared with chat messages so the two interleave.
	Seq         int
	Renderable  *pb.Renderable
	Status      InteractionStatus
	Response    *pb.RenderableResponse // nil until answered
	CreatedAt   time.Time
	RespondedAt time.Time
}

// InteractionStore is the persistence seam for interactions: in-memory here,
// SQLite-backed in the store phase.
type InteractionStore interface {
	AppendInteraction(ctx context.Context, conversationID, userID string, r *pb.Renderable) (Interaction, error)
	GetInteraction(ctx context.Context, conversationID, interactionID string) (interaction Interaction, found bool, err error)
	// RecordInteractionResponse resolves a pending interaction. recorded is false
	// when it was already answered (a re-POST or a concurrent second responder),
	// so the caller can replay the result rather than deliver it twice.
	RecordInteractionResponse(ctx context.Context, conversationID, interactionID string, resp *pb.RenderableResponse) (interaction Interaction, recorded bool, err error)
	ListInteractions(ctx context.Context, conversationID string) ([]Interaction, error)
	// PendingInteractions returns the still-pending FIFO queue, ordered by seq.
	PendingInteractions(ctx context.Context, conversationID string) ([]Interaction, error)
}

func StatusForAction(a pb.RenderableAction) InteractionStatus {
	switch a {
	case pb.RenderableAction_RENDERABLE_ACTION_SUBMIT:
		return InteractionSubmitted
	case pb.RenderableAction_RENDERABLE_ACTION_DECLINE:
		return InteractionDeclined
	case pb.RenderableAction_RENDERABLE_ACTION_CANCEL:
		return InteractionCancelled
	case pb.RenderableAction_RENDERABLE_ACTION_RESPOND:
		return InteractionResponded
	default:
		return InteractionPending
	}
}

type MemoryInteractionStore struct {
	mu     sync.Mutex
	byConv map[string]*convInteractions
}

type convInteractions struct {
	order   []string // interaction ids in seq order
	byID    map[string]*Interaction
	nextSeq int
}

func NewMemoryInteractionStore() *MemoryInteractionStore {
	return &MemoryInteractionStore{byConv: make(map[string]*convInteractions)}
}

func (m *MemoryInteractionStore) conv(conversationID string) *convInteractions {
	c := m.byConv[conversationID]
	if c == nil {
		c = &convInteractions{byID: make(map[string]*Interaction)}
		m.byConv[conversationID] = c
	}
	return c
}

func (m *MemoryInteractionStore) AppendInteraction(_ context.Context, conversationID, userID string, r *pb.Renderable) (Interaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c := m.conv(conversationID)
	// Idempotent by id: a re-emitted Renderable returns the existing record rather
	// than resetting an answered one or duplicating it in the queue.
	if existing := c.byID[r.GetId()]; existing != nil {
		return *existing, nil
	}
	c.nextSeq++
	it := &Interaction{
		ID:             r.GetId(),
		ConversationID: conversationID,
		UserID:         userID,
		Seq:            c.nextSeq,
		Renderable:     r,
		Status:         InteractionPending,
		CreatedAt:      time.Now(),
	}
	c.byID[it.ID] = it
	c.order = append(c.order, it.ID)
	return *it, nil
}

func (m *MemoryInteractionStore) GetInteraction(_ context.Context, conversationID, interactionID string) (Interaction, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c := m.byConv[conversationID]
	if c == nil {
		return Interaction{}, false, nil
	}
	it := c.byID[interactionID]
	if it == nil {
		return Interaction{}, false, nil
	}
	return *it, true, nil
}

func (m *MemoryInteractionStore) RecordInteractionResponse(_ context.Context, conversationID, interactionID string, resp *pb.RenderableResponse) (Interaction, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c := m.byConv[conversationID]
	if c == nil {
		return Interaction{}, false, ErrInteractionNotFound
	}
	it := c.byID[interactionID]
	if it == nil {
		return Interaction{}, false, ErrInteractionNotFound
	}
	if it.Status != InteractionPending {
		return *it, false, nil
	}
	it.Status = StatusForAction(resp.GetAction())
	it.Response = resp
	it.RespondedAt = time.Now()
	return *it, true, nil
}

func (m *MemoryInteractionStore) ListInteractions(_ context.Context, conversationID string) ([]Interaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c := m.byConv[conversationID]
	if c == nil {
		return nil, nil
	}
	out := make([]Interaction, 0, len(c.order))
	for _, id := range c.order {
		out = append(out, *c.byID[id])
	}
	return out, nil
}

func (m *MemoryInteractionStore) PendingInteractions(_ context.Context, conversationID string) ([]Interaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c := m.byConv[conversationID]
	if c == nil {
		return nil, nil
	}
	out := make([]Interaction, 0, len(c.order))
	for _, id := range c.order {
		if it := c.byID[id]; it.Status == InteractionPending {
			out = append(out, *it)
		}
	}
	return out, nil
}
