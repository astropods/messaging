// Package cluster connects the messaging sidecars that share one Postgres chat
// database, so a turn running in one sidecar can be watched, stopped and
// answered through another.
package cluster

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Channel is the NOTIFY channel every sidecar of a deployment listens on.
const Channel = "astro_messaging_bus"

// maxPayload stays under Postgres's 8000-byte NOTIFY limit.
const maxPayload = 7800

const (
	outboxSize      = 4096
	fragmentTimeout = 30 * time.Second
	maxBackoff      = 30 * time.Second
)

const (
	prefixEnvelope = 'E'
	prefixFragment = 'F'
)

// Envelope is one bus message. Data is the kind's own JSON payload.
type Envelope struct {
	Kind         string          `json:"k"`
	From         string          `json:"f"`
	Conversation string          `json:"c,omitempty"`
	Data         json.RawMessage `json:"d,omitempty"`
}

type fragment struct {
	ID   string `json:"id"`
	I    int    `json:"i"`
	N    int    `json:"n"`
	Part string `json:"p"`
}

type partial struct {
	parts []string
	got   int
	seen  time.Time
}

// Bus delivers envelopes to every other sidecar, in the order this sidecar published them.
type Bus struct {
	url string
	pod string
	out chan string

	mu       sync.RWMutex
	handlers map[string]func(Envelope)

	partials map[string]*partial
}

func New(url, pod string) *Bus {
	return &Bus{
		url:      url,
		pod:      pod,
		out:      make(chan string, outboxSize),
		handlers: map[string]func(Envelope){},
		partials: map[string]*partial{},
	}
}

// Pod is this sidecar's id on the bus.
func (b *Bus) Pod() string { return b.pod }

// Handle registers fn for kind. Handlers run one at a time, in delivery order, so they must not block.
func (b *Bus) Handle(kind string, fn func(Envelope)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[kind] = fn
}

// Publish queues an envelope without blocking. A full outbox drops it.
func (b *Bus) Publish(kind, conversationID string, data any) {
	env := Envelope{Kind: kind, From: b.pod, Conversation: conversationID}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			slog.Error("cluster: marshal bus message failed", "kind", kind, "error", err)
			return
		}
		env.Data = raw
	}
	raw, err := json.Marshal(env)
	if err != nil {
		slog.Error("cluster: marshal bus envelope failed", "kind", kind, "error", err)
		return
	}
	for _, payload := range encode(raw) {
		select {
		case b.out <- payload:
		default:
			slog.Warn("cluster: bus outbox full, message dropped", "kind", kind, "conversation_id", conversationID)
			return
		}
	}
}

// encode returns one payload, or fragments split on rune boundaries when the envelope is too large.
func encode(raw []byte) []string {
	if len(raw)+1 <= maxPayload {
		return []string{string(prefixEnvelope) + string(raw)}
	}
	s := string(raw)
	var pieces []string
	const pieceBytes = maxPayload / 2
	for len(s) > 0 {
		n := min(pieceBytes, len(s))
		for n < len(s) && !utf8.RuneStart(s[n]) {
			n--
		}
		pieces = append(pieces, s[:n])
		s = s[n:]
	}
	id := uuid.NewString()
	out := make([]string, 0, len(pieces))
	for i, p := range pieces {
		f, _ := json.Marshal(fragment{ID: id, I: i, N: len(pieces), Part: p})
		out = append(out, string(prefixFragment)+string(f))
	}
	return out
}

// Run listens and publishes until ctx ends, reconnecting after a failure.
func (b *Bus) Run(ctx context.Context) {
	go b.publishLoop(ctx)
	b.listenLoop(ctx)
}

func (b *Bus) listenLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := b.listenOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("cluster: bus listener disconnected", "error", err, "retry_in", backoff)
		if !sleep(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (b *Bus) listenOnce(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, b.url)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background()) //nolint:errcheck
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return err
	}
	slog.Info("cluster: bus listening", "pod", b.pod)
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		b.receive(n.Payload)
	}
}

func (b *Bus) publishLoop(ctx context.Context) {
	var conn *pgx.Conn
	defer func() {
		if conn != nil {
			_ = conn.Close(context.Background())
		}
	}()
	backoff := time.Second
	for {
		var payload string
		select {
		case <-ctx.Done():
			return
		case payload = <-b.out:
		}
		for attempt := 0; ; attempt++ {
			if conn == nil {
				c, err := pgx.Connect(ctx, b.url)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					slog.Warn("cluster: bus publisher connect failed", "error", err, "retry_in", backoff)
					if !sleep(ctx, backoff) {
						return
					}
					backoff = min(backoff*2, maxBackoff)
					continue
				}
				conn, backoff = c, time.Second
			}
			_, err := conn.Exec(ctx, "SELECT pg_notify($1, $2)", Channel, payload)
			if err == nil {
				break
			}
			_ = conn.Close(context.Background())
			conn = nil
			if ctx.Err() != nil {
				return
			}
			if attempt > 0 {
				slog.Warn("cluster: bus publish failed, message dropped", "error", err)
				break
			}
		}
	}
}

func (b *Bus) receive(payload string) {
	if payload == "" {
		return
	}
	var raw []byte
	switch payload[0] {
	case prefixEnvelope:
		raw = []byte(payload[1:])
	case prefixFragment:
		var f fragment
		if err := json.Unmarshal([]byte(payload[1:]), &f); err != nil || f.N <= 0 || f.I < 0 || f.I >= f.N {
			slog.Warn("cluster: bad bus fragment dropped", "error", err)
			return
		}
		raw = b.assemble(f)
		if raw == nil {
			return
		}
	default:
		return
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		slog.Warn("cluster: bad bus envelope dropped", "error", err)
		return
	}
	if env.From == b.pod {
		return
	}
	b.mu.RLock()
	fn := b.handlers[env.Kind]
	b.mu.RUnlock()
	if fn != nil {
		fn(env)
	}
}

// assemble is called only from the listener goroutine, so partials needs no lock.
func (b *Bus) assemble(f fragment) []byte {
	now := time.Now()
	for id, p := range b.partials {
		if now.Sub(p.seen) > fragmentTimeout {
			delete(b.partials, id)
		}
	}
	p := b.partials[f.ID]
	if p == nil {
		p = &partial{parts: make([]string, f.N)}
		b.partials[f.ID] = p
	}
	if len(p.parts) != f.N {
		delete(b.partials, f.ID)
		return nil
	}
	p.seen = now
	if p.parts[f.I] == "" {
		p.got++
	}
	p.parts[f.I] = f.Part
	if p.got < f.N {
		return nil
	}
	delete(b.partials, f.ID)
	size := 0
	for _, s := range p.parts {
		size += len(s)
	}
	out := make([]byte, 0, size)
	for _, s := range p.parts {
		out = append(out, s...)
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
