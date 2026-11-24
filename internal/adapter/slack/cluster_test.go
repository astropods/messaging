package slack

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/cluster"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/slack-go/slack/slackevents"
)

// memoryBus delivers every publish to every other member, like the Postgres bus.
type memoryBus struct {
	mu      sync.Mutex
	members []*memoryBusMember
}

type memoryBusMember struct {
	bus      *memoryBus
	pod      string
	handlers map[string]func(cluster.Envelope)
}

func (b *memoryBus) join(pod string) *memoryBusMember {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := &memoryBusMember{bus: b, pod: pod, handlers: map[string]func(cluster.Envelope){}}
	b.members = append(b.members, m)
	return m
}

func (m *memoryBusMember) Handle(kind string, fn func(cluster.Envelope)) { m.handlers[kind] = fn }

func (m *memoryBusMember) Publish(kind, conversationID string, data any) {
	raw, _ := json.Marshal(data)
	env := cluster.Envelope{Kind: kind, From: m.pod, Conversation: conversationID, Data: raw}
	m.bus.mu.Lock()
	members := append([]*memoryBusMember(nil), m.bus.members...)
	m.bus.mu.Unlock()
	for _, other := range members {
		if other == m {
			continue
		}
		if fn := other.handlers[kind]; fn != nil {
			fn(env)
		}
	}
}

type fakeClaimer struct {
	mu     sync.Mutex
	seen   map[string]bool
	err    error
	prunes int
}

func (c *fakeClaimer) ClaimSlackEvent(_ context.Context, id string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return false, c.err
	}
	if c.seen[id] {
		return false, nil
	}
	c.seen[id] = true
	return true, nil
}

func (c *fakeClaimer) PruneSlackEvents(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prunes++
	return nil
}

type recordedFeedback struct {
	mu  sync.Mutex
	fbs []*pb.PlatformFeedback
}

func (r *recordedFeedback) handler(_ context.Context, fb *pb.PlatformFeedback) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fbs = append(r.fbs, fb)
	return nil
}

func (r *recordedFeedback) stops() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, fb := range r.fbs {
		if fb.GetStreamControl().GetAction() == pb.StreamControl_STOP {
			n++
		}
	}
	return n
}

func clusteredAdapter(bus *memoryBus, pod string, claimer EventClaimer) (*SlackAdapter, *recordedFeedback) {
	a := New()
	fb := &recordedFeedback{}
	a.SetFeedbackHandler(fb.handler)
	a.SetCluster(bus.join(pod), claimer)
	return a, fb
}

func eventPayload(id string) json.RawMessage {
	return json.RawMessage(`{"type":"event_callback","event_id":"` + id + `","event":{"type":"app_mention"}}`)
}

func TestFirstDeliveryDropsARetryOnAnotherSidecar(t *testing.T) {
	bus := &memoryBus{}
	claimer := &fakeClaimer{seen: map[string]bool{}}
	a, _ := clusteredAdapter(bus, "pod-a", claimer)
	b, _ := clusteredAdapter(bus, "pod-b", claimer)
	ctx := context.Background()

	if !a.firstDelivery(ctx, eventPayload("Ev1")) {
		t.Fatal("the first delivery of an event was dropped")
	}
	if b.firstDelivery(ctx, eventPayload("Ev1")) {
		t.Fatal("a retry of the same event on the other sidecar was delivered again")
	}
	if !b.firstDelivery(ctx, eventPayload("Ev2")) {
		t.Fatal("a different event was dropped")
	}
}

func TestFirstDeliveryFailsOpen(t *testing.T) {
	bus := &memoryBus{}
	a, _ := clusteredAdapter(bus, "pod-a", &fakeClaimer{seen: map[string]bool{}, err: errors.New("db down")})
	ctx := context.Background()

	if !a.firstDelivery(ctx, eventPayload("Ev1")) {
		t.Fatal("a failed claim dropped the event, want it delivered")
	}
	if !a.firstDelivery(ctx, json.RawMessage(`{"type":"event_callback"}`)) {
		t.Fatal("an event with no event_id was dropped")
	}
	if !New().firstDelivery(ctx, eventPayload("Ev1")) {
		t.Fatal("a sidecar with no cluster dropped an event")
	}
}

func TestFirstDeliveryPrunesAtMostOncePerInterval(t *testing.T) {
	bus := &memoryBus{}
	claimer := &fakeClaimer{seen: map[string]bool{}}
	a, _ := clusteredAdapter(bus, "pod-a", claimer)
	for _, id := range []string{"Ev1", "Ev2", "Ev3"} {
		a.firstDelivery(context.Background(), eventPayload(id))
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		claimer.mu.Lock()
		n := claimer.prunes
		claimer.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	claimer.mu.Lock()
	defer claimer.mu.Unlock()
	if claimer.prunes != 1 {
		t.Fatalf("pruned %d times for three claims, want once per %s", claimer.prunes, slackEventPruneInterval)
	}
}

func TestStopPressOnTheOtherSidecarStopsTheTurnWhereItRuns(t *testing.T) {
	bus := &memoryBus{}
	claimer := &fakeClaimer{seen: map[string]bool{}}
	owner, ownerFeedback := clusteredAdapter(bus, "pod-a", claimer)
	other, otherFeedback := clusteredAdapter(bus, "pod-b", claimer)
	idle, idleFeedback := clusteredAdapter(bus, "pod-c", claimer)
	_ = idle

	const channel, thread = "C123", "1700000000.000100"
	conversationID := channel + "-" + thread
	owner.bufferMu.Lock()
	owner.contentBuffers[conversationID] = "partial"
	owner.bufferMu.Unlock()

	other.handleAgentSessionStopped(context.Background(), &slackevents.AgentSessionStoppedEvent{
		Channel: channel, ThreadTimestamp: thread, User: "U1",
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && ownerFeedback.stops() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if n := ownerFeedback.stops(); n != 1 {
		t.Fatalf("the sidecar running the turn got %d stops, want 1", n)
	}
	if !owner.turnStopped(conversationID) {
		t.Fatal("the running turn was not marked stopped")
	}
	if n := idleFeedback.stops(); n != 0 {
		t.Fatalf("a sidecar not running the thread got %d stops, want 0", n)
	}
	if n := otherFeedback.stops(); n != 1 {
		t.Fatalf("the sidecar Slack sent the press to got %d stops, want 1, as before clustering", n)
	}
}
