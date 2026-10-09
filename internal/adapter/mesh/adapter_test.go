package mesh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/gorilla/websocket"

	"github.com/astropods/messaging/internal/adapter"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

func signGrant(t *testing.T, taskID string, expires time.Time) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := jwt.Signed(signer).Claims(map[string]any{"scope": "research", "task_id": taskID, "exp": expires.Unix()}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func deployToken(t *testing.T, iss string) string {
	t.Helper()
	enc := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return enc(map[string]string{"alg": "HS256", "typ": "JWT"}) + "." + enc(map[string]string{"iss": iss, "sub": "dep-1"}) + ".c2ln"
}

type fakeGateway struct {
	t      *testing.T
	frames chan *frame
	hello  chan *frame
	conn   chan *websocket.Conn
	url    string
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	g := &fakeGateway{t: t, frames: make(chan *frame, 64), hello: make(chan *frame, 1), conn: make(chan *websocket.Conn, 1)}
	upgrader := websocket.Upgrader{Subprotocols: []string{subprotocol}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		var hello frame
		if err := c.ReadJSON(&hello); err != nil {
			return
		}
		g.hello <- &hello
		_ = c.WriteJSON(&frame{Type: "welcome", Re: hello.Seq, Address: "acct/dep-1", HeartbeatIntervalS: 30, Scopes: []string{"research"}})
		g.conn <- c
		for {
			var f frame
			if err := c.ReadJSON(&f); err != nil {
				return
			}
			g.frames <- &f
		}
	}))
	t.Cleanup(srv.Close)
	g.url = "ws" + strings.TrimPrefix(srv.URL, "http")
	return g
}

func (g *fakeGateway) next(match func(*frame) bool) *frame {
	g.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-g.frames:
			if match(f) {
				return f
			}
		case <-deadline:
			g.t.Fatal("no matching frame from the adapter")
			return nil
		}
	}
}

func startAdapter(t *testing.T, g *fakeGateway) (*Adapter, chan *pb.Message, *websocket.Conn) {
	t.Helper()
	return startAdapterWithSkills(t, g, nil)
}

func startAdapterWithSkills(t *testing.T, g *fakeGateway, skills []Skill) (*Adapter, chan *pb.Message, *websocket.Conn) {
	t.Helper()
	a := New(Config{URL: g.url, Token: deployToken(t, "https://astro.test/"), Name: "writer", Skills: skills})
	if err := a.Initialize(context.Background(), adapter.Config{}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	got := make(chan *pb.Message, 4)
	a.SetMessageHandler(func(_ context.Context, m *pb.Message) error { got <- m; return nil })
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	select {
	case c := <-g.conn:
		return a, got, c
	case <-time.After(5 * time.Second):
		t.Fatal("adapter never connected")
		return nil, nil, nil
	}
}

func TestCardAcceptsTextAndDataParts(t *testing.T) {
	g := newFakeGateway(t)
	startAdapter(t, g)
	hello := <-g.hello
	if hello.Card == nil || !slices.Equal(hello.Card.Accepts, []string{"text", "data"}) {
		t.Fatalf("hello card = %+v, want accepts [text data] so the gateway offers room tasks with a data part", hello.Card)
	}
}

func TestCardAdvertisesTheAgentNameThenItsDeclaredSkillsWithDescriptions(t *testing.T) {
	g := newFakeGateway(t)
	startAdapterWithSkills(t, g, []Skill{
		{Name: "github.issue.investigate", Description: "Investigates a new GitHub issue."},
		{Name: "agent.impostor", Description: "reserved prefix"},
		{Name: "Not Valid", Description: "breaks the pattern"},
		{Name: "github.issue.investigate", Description: "a second entry for the same name"},
		{Name: "notes.summarize"},
	})
	hello := <-g.hello
	want := []skill{
		{Name: "agent.writer"},
		{Name: "github.issue.investigate", Description: "Investigates a new GitHub issue."},
		{Name: "notes.summarize"},
	}
	if hello.Card == nil || !slices.Equal(hello.Card.Skills, want) {
		t.Fatalf("hello card skills = %+v, want %+v: the identity first, declared skills in order with descriptions, reserved and invalid names dropped, the first entry kept for a name", hello.Card, want)
	}
}

func TestCardWithoutDeclaredSkillsAdvertisesOnlyTheAgentName(t *testing.T) {
	g := newFakeGateway(t)
	startAdapter(t, g)
	hello := <-g.hello
	if hello.Card == nil || !slices.Equal(hello.Card.Skills, []skill{{Name: "agent.writer"}}) {
		t.Fatalf("hello card skills = %+v, want only agent.writer", hello.Card)
	}
}

func TestTaskScopeAndGrantReachTheAgentAndTravelBack(t *testing.T) {
	g := newFakeGateway(t)
	a, got, conn := startAdapter(t, g)

	parts := append(text("Draft the Q3 summary"), part{Type: "data", Data: json.RawMessage(`{"room_task_id":"rt_1","inputs":[{"id":"a1","name":"q3.pdf","content_type":"application/pdf"}]}`)})
	offer := &envelope{ID: "m1", From: "acct/overseer", To: "acct/dep-1", Kind: "task", TaskID: "t_1", Scope: "research", Parts: parts, Metadata: json.RawMessage(`{"on_behalf_of":{"kind":"user","id":"user-1"},"room_task_id":"rt_1"}`)}
	if err := conn.WriteJSON(&frame{Type: "deliver", DeliveryID: "d_1", Offer: true, Envelope: offer}); err != nil {
		t.Fatal(err)
	}
	claim := g.next(func(f *frame) bool { return f.Type == "claim" })
	first := signGrant(t, "t_1", time.Now().Add(5*time.Minute))
	if err := conn.WriteJSON(&frame{Type: "granted", Re: claim.Seq, TaskID: "t_1", Grant: first}); err != nil {
		t.Fatal(err)
	}

	var msg *pb.Message
	select {
	case msg = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the task never reached the agent")
	}
	if scope := msg.GetPlatformContext().GetPlatformData()["mesh_scope"]; scope != "research" {
		t.Errorf("message mesh_scope = %q, want research", scope)
	}
	if msg.GetContent() != "Draft the Q3 summary" {
		t.Errorf("message content = %q, want only the task's text", msg.GetContent())
	}
	data := msg.GetPlatformContext().GetPlatformData()
	if got := data["mesh_data"]; got != `[{"room_task_id":"rt_1","inputs":[{"id":"a1","name":"q3.pdf","content_type":"application/pdf"}]}]` {
		t.Errorf("mesh_data = %s, want the task's data part, so the agent sees its input documents", got)
	}
	if got := data["mesh_metadata"]; !strings.Contains(got, `"on_behalf_of":{"kind":"user","id":"user-1"}`) {
		t.Errorf("mesh_metadata = %s, want who the task is on behalf of", got)
	}

	rg, ok := a.RoomGrant("t_1")
	if !ok || rg.Grant != first || rg.RoomID != "research" || rg.APIURL != "https://astro.test" {
		t.Fatalf("room grant = %+v, %v; want the claim's grant for research at https://astro.test", rg, ok)
	}

	refreshed := signGrant(t, "t_1", time.Now().Add(10*time.Minute))
	if err := conn.WriteJSON(&frame{Type: "granted", TaskID: "t_1", Grant: refreshed}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if rg, _ := a.RoomGrant("t_1"); rg.Grant == refreshed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("an unprompted granted did not replace the grant")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := a.HandleAgentResponse(context.Background(), &pb.AgentResponse{ConversationId: "t_1", Payload: &pb.AgentResponse_Content{Content: &pb.ContentChunk{Type: pb.ContentChunk_END, Content: "done"}}}); err != nil {
		t.Fatalf("agent response: %v", err)
	}
	status := g.next(func(f *frame) bool { return f.Type == "send" && f.Envelope != nil && f.Envelope.Kind == "status" })
	if status.Envelope.Scope != "research" || status.Envelope.State != "completed" {
		t.Errorf("status = %+v, want completed in scope research", status.Envelope)
	}
	if _, ok := a.RoomGrant("t_1"); ok {
		t.Errorf("room grant still served after the task finished")
	}
}

func TestExpiredGrantIsNotServed(t *testing.T) {
	a := New(Config{})
	a.track(&turn{conversationID: "t_9", taskID: "t_9", scope: "research", grant: signGrant(t, "t_9", time.Now().Add(-time.Minute))})
	if _, ok := a.RoomGrant("t_9"); ok {
		t.Fatal("an expired grant was served")
	}
}
