package a2a

// protocolVersion is the A2A protocol revision this adapter implements.
const protocolVersion = "0.3.0"

// Task states defined by A2A. Only the ones this adapter can reach are listed.
const (
	stateSubmitted = "submitted"
	stateWorking   = "working"
	stateCompleted = "completed"
	stateCanceled  = "canceled"
	stateFailed    = "failed"
)

// Card is the A2A agent card served at /.well-known/agent-card.json.
type Card struct {
	ProtocolVersion    string       `json:"protocolVersion"`
	Name               string       `json:"name"`
	Description        string       `json:"description"`
	URL                string       `json:"url"`
	PreferredTransport string       `json:"preferredTransport"`
	Version            string       `json:"version"`
	Capabilities       Capabilities `json:"capabilities"`
	DefaultInputModes  []string     `json:"defaultInputModes"`
	DefaultOutputModes []string     `json:"defaultOutputModes"`
	Skills             []Skill      `json:"skills"`
}

type Capabilities struct {
	Streaming              bool `json:"streaming"`
	PushNotifications      bool `json:"pushNotifications"`
	StateTransitionHistory bool `json:"stateTransitionHistory"`
}

type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

// Part is a single piece of message or artifact content. Only text parts are
// handled; a non-text part is rejected rather than silently dropped.
type Part struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
}

const partKindText = "text"

// Message is an A2A message in either direction.
type Message struct {
	Role      string         `json:"role"`
	Parts     []Part         `json:"parts"`
	MessageID string         `json:"messageId"`
	ContextID string         `json:"contextId,omitempty"`
	TaskID    string         `json:"taskId,omitempty"`
	Kind      string         `json:"kind,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Text joins every text part. A2A allows a message to be split across parts,
// so reading only the first would truncate multi-part input.
func (m Message) Text() string {
	out := ""
	for _, p := range m.Parts {
		if p.Kind == partKindText {
			out += p.Text
		}
	}
	return out
}

type Artifact struct {
	ArtifactID string `json:"artifactId"`
	Parts      []Part `json:"parts"`
}

type TaskStatus struct {
	State     string `json:"state"`
	Timestamp string `json:"timestamp"`
	Message   string `json:"message,omitempty"`
}

// Task is the A2A unit of work returned by message/send and tasks/get.
type Task struct {
	ID        string     `json:"id"`
	ContextID string     `json:"contextId"`
	Status    TaskStatus `json:"status"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
	Kind      string     `json:"kind"`
}

// sendParams is the params object of message/send.
type sendParams struct {
	Message Message `json:"message"`
}

// taskIDParams is the params object of tasks/get and tasks/cancel.
type taskIDParams struct {
	ID string `json:"id"`
}
