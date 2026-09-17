package a2a

import (
	"strings"
	"sync"
	"time"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// maxTrackedTasks bounds the task registry. Entries are evicted oldest-first,
// which is the safety valve for a turn that neither ends nor is cancelled.
const maxTrackedTasks = 4096

// taskEntry is one A2A task plus the state needed to resolve it from the
// agent's streamed reply.
type taskEntry struct {
	id        string
	contextID string
	created   time.Time

	mu       sync.Mutex
	state    string
	partial  strings.Builder
	failure  string
	done     chan struct{}
	finished bool
}

func newTaskEntry(id, contextID string) *taskEntry {
	return &taskEntry{
		id:        id,
		contextID: contextID,
		created:   time.Now(),
		state:     stateSubmitted,
		done:      make(chan struct{}),
	}
}

// record accumulates one content chunk. START and REPLACE reset the buffer,
// every chunk appends: agents stream a reply as deltas and typically send an
// empty END, so no single chunk holds the whole reply.
func (t *taskEntry) record(chunk *pb.ContentChunk) {
	if chunk == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	if chunk.Type == pb.ContentChunk_START || chunk.Type == pb.ContentChunk_REPLACE {
		t.partial.Reset()
	}
	t.partial.WriteString(chunk.Content)
	t.state = stateWorking
	if chunk.Type == pb.ContentChunk_END {
		t.state = stateCompleted
		t.closeLocked()
	}
}

// fail terminates the task as failed with a reason the caller sees on the task
// status.
func (t *taskEntry) fail(reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	t.state = stateFailed
	t.failure = reason
	t.closeLocked()
}

// cancel terminates the task as canceled. It reports false when the task has
// already reached a terminal state, which A2A surfaces as TaskNotCancelable.
func (t *taskEntry) cancel() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return false
	}
	t.state = stateCanceled
	t.closeLocked()
	return true
}

func (t *taskEntry) closeLocked() {
	if t.finished {
		return
	}
	t.finished = true
	close(t.done)
}

// snapshot renders the current state as an A2A task. Safe to call while the
// turn is still streaming; the artifact then holds the text seen so far.
func (t *taskEntry) snapshot() Task {
	t.mu.Lock()
	defer t.mu.Unlock()
	task := Task{
		ID:        t.id,
		ContextID: t.contextID,
		Kind:      "task",
		Status: TaskStatus{
			State:     t.state,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Message:   t.failure,
		},
	}
	if text := t.partial.String(); text != "" {
		task.Artifacts = []Artifact{{
			ArtifactID: t.id,
			Parts:      []Part{{Kind: partKindText, Text: text}},
		}}
	}
	return task
}

// registry tracks tasks by their own id and by the conversation the agent
// replies on, so a streamed response can be routed back to the waiting task.
type registry struct {
	mu       sync.Mutex
	byID     map[string]*taskEntry
	byThread map[string]*taskEntry
}

func newRegistry() *registry {
	return &registry{
		byID:     make(map[string]*taskEntry),
		byThread: make(map[string]*taskEntry),
	}
}

// add registers a task as the active turn for its conversation. A second send
// on the same conversation supersedes the first, matching the agent stream,
// which only ever reports progress for the turn it is currently running.
func (r *registry) add(t *taskEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.byID) >= maxTrackedTasks {
		r.evictOldestLocked()
	}
	r.byID[t.id] = t
	r.byThread[t.contextID] = t
}

func (r *registry) evictOldestLocked() {
	var oldest *taskEntry
	for _, t := range r.byID {
		if oldest == nil || t.created.Before(oldest.created) {
			oldest = t
		}
	}
	if oldest == nil {
		return
	}
	delete(r.byID, oldest.id)
	if cur, ok := r.byThread[oldest.contextID]; ok && cur == oldest {
		delete(r.byThread, oldest.contextID)
	}
}

func (r *registry) byTaskID(id string) *taskEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[id]
}

func (r *registry) active(contextID string) *taskEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byThread[contextID]
}

// all snapshots every tracked task, for the shared-stream disconnect path that
// has to finalize each in-flight turn.
func (r *registry) all() []*taskEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*taskEntry, 0, len(r.byID))
	for _, t := range r.byID {
		out = append(out, t)
	}
	return out
}
