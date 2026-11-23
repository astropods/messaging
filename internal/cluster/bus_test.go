package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/testpg"
)

const postgresNotifyLimit = 8000

type recorded struct {
	mu   sync.Mutex
	envs []Envelope
}

func (r *recorded) add(env Envelope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.envs = append(r.envs, env)
}

func (r *recorded) snapshot() []Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Envelope(nil), r.envs...)
}

func encodeEnvelope(t *testing.T, from, kind string, data any) []string {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	env, err := json.Marshal(Envelope{Kind: kind, From: from, Data: raw})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return encode(env)
}

func TestEncodeKeepsEveryPayloadUnderTheNotifyLimit(t *testing.T) {
	escapeHeavy := strings.Repeat(`<a href="x">&amp;</a>"\`, 4000)
	for _, tc := range []struct {
		name string
		text string
	}{
		{"small", "hello"},
		{"escape-heavy markdown", escapeHeavy},
		{"multibyte", strings.Repeat("héllo wörld ✓ ", 3000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, p := range encodeEnvelope(t, "pod-a", "k", map[string]string{"text": tc.text}) {
				if len(p) >= postgresNotifyLimit {
					t.Fatalf("payload %d is %d bytes, want under %d", i, len(p), postgresNotifyLimit)
				}
			}
		})
	}
}

func TestReceiveReassemblesFragmentsInAnyOrder(t *testing.T) {
	text := strings.Repeat(`<p>"quoted" & ünïcode</p>`, 2000)
	payloads := encodeEnvelope(t, "pod-a", "k", map[string]string{"text": text})
	if len(payloads) < 3 {
		t.Fatalf("got %d payloads, want a message large enough to fragment", len(payloads))
	}

	b := New("", "pod-b")
	var got recorded
	b.Handle("k", got.add)
	for i := len(payloads) - 1; i >= 0; i-- {
		b.receive(payloads[i])
	}

	envs := got.snapshot()
	if len(envs) != 1 {
		t.Fatalf("delivered %d envelopes, want 1", len(envs))
	}
	var data map[string]string
	if err := json.Unmarshal(envs[0].Data, &data); err != nil || data["text"] != text {
		t.Fatalf("reassembled text differs from the original (err %v)", err)
	}
}

func TestReceiveIgnoresItsOwnMessagesAndUnknownKinds(t *testing.T) {
	b := New("", "pod-a")
	var got recorded
	b.Handle("k", got.add)

	for _, p := range encodeEnvelope(t, "pod-a", "k", "mine") {
		b.receive(p)
	}
	for _, p := range encodeEnvelope(t, "pod-b", "other-kind", "x") {
		b.receive(p)
	}
	b.receive("garbage")
	b.receive("")

	if n := len(got.snapshot()); n != 0 {
		t.Fatalf("delivered %d envelopes, want 0", n)
	}
}

func TestReceiveDropsAFragmentSetThatDoesNotMatch(t *testing.T) {
	payloads := encodeEnvelope(t, "pod-a", "k", strings.Repeat("x", 3*maxPayload))
	b := New("", "pod-b")
	var got recorded
	b.Handle("k", got.add)

	var f fragment
	if err := json.Unmarshal([]byte(payloads[0][1:]), &f); err != nil {
		t.Fatalf("decode fragment: %v", err)
	}
	f.N++
	bad, _ := json.Marshal(f)
	b.receive(string(prefixFragment) + string(bad))
	for _, p := range payloads[1:] {
		b.receive(p)
	}

	if n := len(got.snapshot()); n != 0 {
		t.Fatalf("delivered %d envelopes from an inconsistent fragment set, want 0", n)
	}
}

func runBus(t *testing.T, url, pod string) *Bus {
	t.Helper()
	b := New(url, pod)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		b.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return b
}

func waitFor(t *testing.T, what string, cond func() bool) {
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

func TestBusDeliversToTheOtherSidecarInPublishOrder(t *testing.T) {
	url := testpg.URL(t)
	a := runBus(t, url, "pod-a")
	b := runBus(t, url, "pod-b")

	var ready recorded
	b.Handle("ping", ready.add)
	waitFor(t, "pod-b to start listening", func() bool {
		a.Publish("ping", "", nil)
		return len(ready.snapshot()) > 0
	})

	var got recorded
	b.Handle("seq", got.add)
	var mine recorded
	a.Handle("seq", mine.add)
	large := strings.Repeat(`<b>"big"</b>`, 2000)
	const n = 50
	for i := range n {
		body := fmt.Sprint(i)
		if i == n/2 {
			body = large
		}
		a.Publish("seq", fmt.Sprintf("conv-%d", i), body)
	}
	waitFor(t, "all messages on pod-b", func() bool { return len(got.snapshot()) == n })

	for i, env := range got.snapshot() {
		if env.From != "pod-a" || env.Conversation != fmt.Sprintf("conv-%d", i) {
			t.Fatalf("message %d is from %q for %q, want pod-a in publish order", i, env.From, env.Conversation)
		}
	}
	var body string
	if err := json.Unmarshal(got.snapshot()[n/2].Data, &body); err != nil || body != large {
		t.Fatalf("the fragmented message arrived altered (err %v)", err)
	}
	if len(mine.snapshot()) != 0 {
		t.Fatal("pod-a delivered its own messages to itself")
	}
}
