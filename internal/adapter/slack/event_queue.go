package slack

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/slack-go/slack/slackevents"
)

// queueInnerEvent handles ev off the socket loop, after the earlier events in its channel.
func (a *SlackAdapter) queueInnerEvent(ctx context.Context, ev slackevents.EventsAPIEvent, payload json.RawMessage) {
	a.events.run(eventChannel(ev.InnerEvent), func() {
		a.handleInnerEvent(ctx, ev.InnerEvent, ev.TeamID, payload)
	})
}

// channelQueue runs tasks with the same key one at a time in arrival order,
// and tasks with different keys in parallel. The zero value is ready to use.
type channelQueue struct {
	mu      sync.Mutex
	pending map[string][]func()
}

func (q *channelQueue) run(key string, task func()) {
	q.mu.Lock()
	if tasks, busy := q.pending[key]; busy {
		q.pending[key] = append(tasks, task)
		q.mu.Unlock()
		return
	}
	if q.pending == nil {
		q.pending = map[string][]func(){}
	}
	q.pending[key] = nil
	q.mu.Unlock()
	go q.drain(key, task)
}

func (q *channelQueue) drain(key string, task func()) {
	for {
		task()
		q.mu.Lock()
		tasks := q.pending[key]
		if len(tasks) == 0 {
			delete(q.pending, key)
			q.mu.Unlock()
			return
		}
		task, q.pending[key] = tasks[0], tasks[1:]
		q.mu.Unlock()
	}
}

func eventChannel(ev slackevents.EventsAPIInnerEvent) string {
	switch e := ev.Data.(type) {
	case *slackevents.MessageEvent:
		return e.Channel
	case *slackevents.AppMentionEvent:
		return e.Channel
	case *slackevents.ReactionAddedEvent:
		return e.Item.Channel
	case *slackevents.ReactionRemovedEvent:
		return e.Item.Channel
	case *slackevents.AssistantThreadStartedEvent:
		return e.AssistantThread.ChannelID
	case *slackevents.AgentSessionStoppedEvent:
		return e.Channel
	}
	return ""
}
