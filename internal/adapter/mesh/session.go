package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	protocolVersion = "0.1"
	subprotocol     = "amp.v1alpha1"
	replyTimeout    = 15 * time.Second
	minBackoff      = time.Second
	maxBackoff      = 30 * time.Second
)

type part struct {
	Type string          `json:"type"`
	Text string          `json:"text,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

type envelope struct {
	ID       string          `json:"id"`
	From     string          `json:"from,omitempty"`
	To       string          `json:"to"`
	Kind     string          `json:"kind"`
	TaskID   string          `json:"task_id,omitempty"`
	ReplyTo  string          `json:"reply_to,omitempty"`
	State    string          `json:"state,omitempty"`
	Parts    []part          `json:"parts,omitempty"`
	Scope    string          `json:"scope,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

type skill struct {
	Name string `json:"name"`
}

type card struct {
	Name          string  `json:"name"`
	Kind          string  `json:"kind"`
	Skills        []skill  `json:"skills,omitempty"`
	Accepts       []string `json:"accepts,omitempty"`
	MaxConcurrent int      `json:"max_concurrent,omitempty"`
}

type ampError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *ampError) Error() string { return e.Code + ": " + e.Message }

type credential struct {
	Kind  string `json:"kind"`
	Token string `json:"token"`
}

type frame struct {
	Type               string      `json:"type"`
	Seq                int64       `json:"seq"`
	Re                 int64       `json:"re,omitempty"`
	Versions           []string    `json:"versions,omitempty"`
	Credential         *credential `json:"credential,omitempty"`
	Card               *card       `json:"card,omitempty"`
	Address            string      `json:"address,omitempty"`
	HeartbeatIntervalS int         `json:"heartbeat_interval_s,omitempty"`
	Envelope           *envelope   `json:"envelope,omitempty"`
	TaskID             string      `json:"task_id,omitempty"`
	DeliveryID         string      `json:"delivery_id,omitempty"`
	Offer              bool        `json:"offer,omitempty"`
	DelayS             int         `json:"delay_s,omitempty"`
	Grant              string      `json:"grant,omitempty"`
	Scopes             []string    `json:"scopes,omitempty"`
	Error              *ampError   `json:"error,omitempty"`
	Reason             string      `json:"reason,omitempty"`
}

func textOf(parts []part) string {
	out := ""
	for _, p := range parts {
		if p.Type != "text" {
			continue
		}
		if out != "" {
			out += "\n"
		}
		out += p.Text
	}
	return out
}

func text(s string) []part {
	if s == "" {
		return nil
	}
	return []part{{Type: "text", Text: s}}
}

type session struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	seq     atomic.Int64
	address string

	mu      sync.Mutex
	pending map[int64]chan *frame
	closed  bool
}

func dial(ctx context.Context, url, token string, c card) (*session, *frame, error) {
	dialer := websocket.Dialer{Subprotocols: []string{subprotocol}, HandshakeTimeout: 10 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, url, http.Header{})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, nil, err
	}
	s := &session{conn: conn, pending: map[int64]chan *frame{}}
	if err := s.write(&frame{Type: "hello", Versions: []string{protocolVersion}, Credential: &credential{Kind: "bearer", Token: token}, Card: &c}); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(replyTimeout))
	var welcome frame
	if err := conn.ReadJSON(&welcome); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("read welcome: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	if welcome.Type == "error" && welcome.Error != nil {
		_ = conn.Close()
		return nil, nil, welcome.Error
	}
	if welcome.Type != "welcome" {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("expected welcome, got %s", welcome.Type)
	}
	s.address = welcome.Address
	return s, &welcome, nil
}

func (s *session) send(f *frame, waiter chan *frame) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	f.Seq = s.seq.Add(1)
	if waiter != nil {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return errors.New("mesh session closed")
		}
		s.pending[f.Seq] = waiter
		s.mu.Unlock()
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return s.conn.WriteJSON(f)
}

func (s *session) write(f *frame) error {
	return s.send(f, nil)
}

func (s *session) call(ctx context.Context, f *frame) (*frame, error) {
	ch := make(chan *frame, 1)
	if err := s.send(f, ch); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, replyTimeout)
	defer cancel()
	select {
	case reply, ok := <-ch:
		if !ok {
			return nil, errors.New("mesh session closed")
		}
		if reply.Type == "error" && reply.Error != nil {
			return nil, reply.Error
		}
		return reply, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, f.Seq)
		s.mu.Unlock()
		return nil, fmt.Errorf("no reply to %s: %w", f.Type, ctx.Err())
	}
}

func (s *session) read(onFrame func(*frame)) error {
	for {
		var f frame
		if err := s.conn.ReadJSON(&f); err != nil {
			return err
		}
		if f.Re != 0 {
			s.mu.Lock()
			ch, ok := s.pending[f.Re]
			delete(s.pending, f.Re)
			s.mu.Unlock()
			if ok {
				ch <- &f
				continue
			}
		}
		if f.Type == "goodbye" {
			return fmt.Errorf("gateway said goodbye: %s", f.Reason)
		}
		if f.Type == "error" && f.Error != nil {
			slog.Warn("[Mesh] gateway reported an error", "code", f.Error.Code, "message", f.Error.Message, "re", f.Re)
			continue
		}
		onFrame(&f)
	}
}

func (s *session) close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		for seq, ch := range s.pending {
			close(ch)
			delete(s.pending, seq)
		}
	}
	s.mu.Unlock()
	_ = s.conn.Close()
}
