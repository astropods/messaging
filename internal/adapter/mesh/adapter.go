package mesh

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/authz"
	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

const (
	Platform      = "mesh"
	agentPrefix   = "agent."
	maxConcurrent = 4
)

var skillPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

type Config struct {
	URL   string
	Token string
	Name  string
}

type turn struct {
	conversationID string
	taskID         string
	requester      string
	replyTo        string
	scope          string
	grant          string
	text           strings.Builder
	lastStatus     string
}

type RoomGrant struct {
	RoomID    string
	Grant     string
	ExpiresAt time.Time
	APIURL    string
}

type Adapter struct {
	cfg     Config
	apiURL  string
	handler adapter.MessageHandler
	configs *store.AgentConfigStore

	session atomic.Pointer[session]
	cancel  context.CancelFunc
	done    chan struct{}

	mu    sync.Mutex
	turns map[string]*turn
}

func New(cfg Config) *Adapter {
	return &Adapter{cfg: cfg, turns: map[string]*turn{}}
}

func (a *Adapter) Initialize(_ context.Context, _ adapter.Config) error {
	if a.cfg.URL == "" {
		return errors.New("mesh: ASTRO_MESH_URL is required")
	}
	if a.cfg.Token == "" {
		return errors.New("mesh: ASTRO_AUTHZ_TOKEN is required")
	}
	claims, err := authz.DecodeToken(a.cfg.Token)
	if err != nil {
		return fmt.Errorf("mesh: decode ASTRO_AUTHZ_TOKEN: %w", err)
	}
	a.apiURL = strings.TrimSuffix(claims.Issuer, "/")
	return nil
}

func (a *Adapter) Start(ctx context.Context) error {
	ctx, a.cancel = context.WithCancel(ctx)
	a.done = make(chan struct{})
	go a.run(ctx)
	return nil
}

func (a *Adapter) Stop(_ context.Context) error {
	if a.cancel != nil {
		a.cancel()
	}
	if s := a.session.Load(); s != nil {
		_ = s.write(&frame{Type: "goodbye", Reason: "messaging stopping"})
		s.close()
	}
	if a.done != nil {
		<-a.done
	}
	return nil
}

func (a *Adapter) GetPlatformName() string { return Platform }

func (a *Adapter) IsHealthy(_ context.Context) bool { return a.session.Load() != nil }

func (a *Adapter) Capabilities() adapter.AdapterCapabilities {
	return adapter.AdapterCapabilities{SupportsStreaming: true, SupportsStatusUpdates: true}
}

func (a *Adapter) SetMessageHandler(h adapter.MessageHandler) { a.handler = h }

func (a *Adapter) SetAgentConfigStore(s *store.AgentConfigStore) { a.configs = s }

func (a *Adapter) SetFeedbackHandler(adapter.FeedbackHandler) {}

func (a *Adapter) HydrateThread(context.Context, string, *store.ThreadHistoryStore) error {
	return nil
}

func (a *Adapter) skills() []string {
	var out []string
	if identity := agentPrefix + strings.ToLower(a.cfg.Name); a.cfg.Name != "" && skillPattern.MatchString(identity) {
		out = append(out, identity)
	}
	if a.configs == nil {
		return out
	}
	cfg := a.configs.Get()
	if cfg == nil {
		return out
	}
	for _, s := range cfg.GetSkills() {
		name := s.GetName()
		switch {
		case strings.HasPrefix(name, agentPrefix):
			slog.Warn("[Mesh] ignoring declared skill with the reserved agent. prefix", "skill", name)
		case !skillPattern.MatchString(name):
			slog.Warn("[Mesh] ignoring declared skill that is not a valid skill name", "skill", name)
		case !slices.Contains(out, name):
			out = append(out, name)
		}
	}
	return out
}

func (a *Adapter) card(skills []string) card {
	c := card{Name: a.cfg.Name, Kind: "agent", MaxConcurrent: maxConcurrent}
	if c.Name == "" {
		c.Name = "agent"
	}
	for _, s := range skills {
		c.Skills = append(c.Skills, skill{Name: s})
	}
	return c
}

func (a *Adapter) run(ctx context.Context) {
	defer close(a.done)
	backoff := minBackoff
	for ctx.Err() == nil {
		started := time.Now()
		err := a.connect(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > maxBackoff {
			backoff = minBackoff
		}
		var ae *ampError
		if errors.As(err, &ae) && !ae.Retryable {
			slog.Error("[Mesh] gateway rejected the session; retrying slowly", "code", ae.Code, "message", ae.Message)
			backoff = maxBackoff
		} else {
			slog.Warn("[Mesh] session ended; reconnecting", "error", err, "in", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (a *Adapter) connect(ctx context.Context) error {
	skills := a.skills()
	s, welcome, err := dial(ctx, a.cfg.URL, a.cfg.Token, a.card(skills))
	if err != nil {
		return err
	}
	a.session.Store(s)
	defer func() {
		a.session.CompareAndSwap(s, nil)
		s.close()
		a.dropTurns()
	}()
	slog.Info("[Mesh] joined the mesh", "address", s.address, "skills", skills)

	interval := time.Duration(welcome.HeartbeatIntervalS) * time.Second
	if interval <= 0 {
		interval = 10 * time.Second
	}
	stop := make(chan struct{})
	defer close(stop)
	if a.configs != nil {
		go a.watchSkills(stop, s, skills)
	}
	go func() {
		tick := time.NewTicker(interval / 2)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				s.close()
				return
			case <-tick.C:
				if err := s.write(&frame{Type: "heartbeat"}); err != nil {
					s.close()
					return
				}
			}
		}
	}()

	return s.read(func(f *frame) {
		switch {
		case f.Type == "deliver" && f.Envelope != nil:
			go a.deliver(ctx, s, f)
		case f.Type == "granted" && f.Grant != "":
			a.refreshGrant(f.TaskID, f.Grant)
		}
	})
}

func (a *Adapter) watchSkills(stop <-chan struct{}, s *session, sent []string) {
	for {
		changed := a.configs.Changed()
		select {
		case <-stop:
			return
		case <-changed:
			if !slices.Equal(a.skills(), sent) {
				slog.Info("[Mesh] agent changed its skills; rejoining")
				s.close()
				return
			}
		}
	}
}

func (a *Adapter) dropTurns() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id := range a.turns {
		delete(a.turns, id)
	}
}

func (a *Adapter) activeTasks() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, t := range a.turns {
		if t.taskID != "" {
			n++
		}
	}
	return n
}

func (a *Adapter) deliver(ctx context.Context, s *session, f *frame) {
	env := f.Envelope
	if f.Offer {
		a.offer(ctx, s, f)
		return
	}
	defer func() { _ = s.write(&frame{Type: "ack", DeliveryID: f.DeliveryID}) }()
	if env.Kind != "message" || a.handler == nil {
		return
	}
	t := &turn{conversationID: "mesh-" + uuid.NewString(), requester: env.From, replyTo: env.ID, scope: env.Scope, grant: f.Grant}
	a.track(t)
	if err := a.handler(ctx, a.toMessage(env, t.conversationID)); err != nil {
		slog.Warn("[Mesh] forward message to agent failed", "from", env.From, "error", err)
		a.untrack(t.conversationID)
	}
}

func (a *Adapter) offer(ctx context.Context, s *session, f *frame) {
	env := f.Envelope
	if a.handler == nil || a.activeTasks() >= maxConcurrent {
		_ = s.write(&frame{Type: "nack", DeliveryID: f.DeliveryID, DelayS: 1})
		return
	}
	granted, err := s.call(ctx, &frame{Type: "claim", TaskID: env.TaskID})
	if err != nil {
		slog.Info("[Mesh] claim failed", "task_id", env.TaskID, "error", err)
		return
	}
	t := &turn{conversationID: env.TaskID, taskID: env.TaskID, requester: env.From, scope: env.Scope, grant: granted.Grant}
	a.track(t)
	slog.Info("[Mesh] task claimed", "task_id", env.TaskID, "from", env.From)
	if err := a.handler(ctx, a.toMessage(env, t.conversationID)); err != nil {
		slog.Warn("[Mesh] forward task to agent failed; releasing it", "task_id", env.TaskID, "error", err)
		a.untrack(t.conversationID)
		_ = s.write(&frame{Type: "release", TaskID: env.TaskID})
	}
}

func (a *Adapter) toMessage(env *envelope, conversationID string) *pb.Message {
	return &pb.Message{
		Id:        uuid.NewString(),
		Timestamp: timestamppb.Now(),
		Platform:  Platform,
		PlatformContext: &pb.PlatformContext{
			MessageId: env.ID,
			ChannelId: env.From,
			ThreadId:  conversationID,
			PlatformData: map[string]string{
				"mesh_kind":    env.Kind,
				"mesh_task_id": env.TaskID,
				"mesh_from":    env.From,
				"mesh_to":      env.To,
				"mesh_scope":   env.Scope,
			},
			EventKind: pb.PlatformContext_EVENT_KIND_DM,
		},
		User:           &pb.User{Id: env.From, Username: env.From},
		Content:        textOf(env.Parts),
		ConversationId: conversationID,
	}
}

func (a *Adapter) track(t *turn) {
	a.mu.Lock()
	a.turns[t.conversationID] = t
	a.mu.Unlock()
}

func (a *Adapter) untrack(conversationID string) *turn {
	a.mu.Lock()
	defer a.mu.Unlock()
	t := a.turns[conversationID]
	delete(a.turns, conversationID)
	return t
}

func (a *Adapter) lookup(conversationID string) *turn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turns[conversationID]
}

func (a *Adapter) HandleAgentResponse(_ context.Context, resp *pb.AgentResponse) error {
	t := a.lookup(resp.ConversationId)
	if t == nil {
		return nil
	}
	s := a.session.Load()
	if s == nil {
		return adapter.ErrAgentUnreachable
	}
	switch p := resp.Payload.(type) {
	case *pb.AgentResponse_Content:
		switch p.Content.Type {
		case pb.ContentChunk_START, pb.ContentChunk_REPLACE:
			t.text.Reset()
			t.text.WriteString(p.Content.Content)
		case pb.ContentChunk_DELTA:
			t.text.WriteString(p.Content.Content)
		case pb.ContentChunk_END:
			t.text.WriteString(p.Content.Content)
			a.untrack(t.conversationID)
			return a.finish(s, t, "completed", t.text.String())
		}
	case *pb.AgentResponse_Status:
		if t.taskID == "" {
			return nil
		}
		label := p.Status.CustomMessage
		if label == "" {
			label = strings.ToLower(p.Status.Status.String())
		}
		if label == t.lastStatus {
			return nil
		}
		t.lastStatus = label
		return s.write(&frame{Type: "send", Envelope: &envelope{
			ID: uuid.NewString(), To: t.requester, Kind: "status", TaskID: t.taskID, State: "working", Parts: text(label), Scope: t.scope,
		}})
	case *pb.AgentResponse_Error:
		a.untrack(t.conversationID)
		return a.finish(s, t, "failed", p.Error.Message)
	}
	return nil
}

func (a *Adapter) finish(s *session, t *turn, state, body string) error {
	if t.taskID == "" {
		return s.write(&frame{Type: "send", Envelope: &envelope{
			ID: uuid.NewString(), To: t.requester, Kind: "message", ReplyTo: t.replyTo, Parts: text(body), Scope: t.scope,
		}})
	}
	slog.Info("[Mesh] task finished", "task_id", t.taskID, "state", state)
	return s.write(&frame{Type: "send", Envelope: &envelope{
		ID: uuid.NewString(), To: t.requester, Kind: "status", TaskID: t.taskID, State: state, Parts: text(body), Scope: t.scope,
	}})
}

func (a *Adapter) refreshGrant(taskID, grant string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t := a.turns[taskID]; t != nil && t.taskID == taskID {
		t.grant = grant
	}
}

func (a *Adapter) RoomGrant(conversationID string) (RoomGrant, bool) {
	a.mu.Lock()
	t := a.turns[conversationID]
	var scope, grant string
	if t != nil {
		scope, grant = t.scope, t.grant
	}
	a.mu.Unlock()
	if grant == "" || scope == "" {
		return RoomGrant{}, false
	}
	expires, err := grantExpiry(grant)
	if err != nil || !time.Now().Before(expires) {
		return RoomGrant{}, false
	}
	return RoomGrant{RoomID: scope, Grant: grant, ExpiresAt: expires, APIURL: a.apiURL}, true
}
