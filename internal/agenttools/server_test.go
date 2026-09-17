package agenttools

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/astropods/messaging/internal/a2awire"
	"github.com/astropods/messaging/internal/peers"
)

// fakeCluster stands in for astro-server plus one peer agent, on one httptest
// server. Both the registry (which reads the token's iss) and the client (which
// dials the peer URL) point at it, so these tests exercise the real HTTP paths
// rather than stubbing the collaborators out.
type fakeCluster struct {
	srv       *httptest.Server
	peerReply func(text string) a2awire.Response
	peerCard  *a2awire.Card
	peers     []peers.Peer
	asked     []string
}

func newFakeCluster(t *testing.T) *fakeCluster {
	t.Helper()
	c := &fakeCluster{
		peerCard: &a2awire.Card{
			Name:        "billing-bot",
			Description: "Answers billing and invoice questions.",
			Skills: []a2awire.Skill{
				{ID: "lookup_invoice", Name: "Look up an invoice"},
				{ID: "issue_refund", Name: "Issue a refund"},
			},
		},
	}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/deployments/a2a/peers", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"peers": c.peers})
	})
	mux.HandleFunc("GET /.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		if c.peerCard == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(c.peerCard)
	})
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		var req a2awire.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var params a2awire.SendParams
		_ = json.Unmarshal(req.Params, &params)
		c.asked = append(c.asked, params.Message.Text())

		res := a2awire.ResultResponse(req.ID, a2awire.Task{
			ID: "task-1", ContextID: "ctx-1", Kind: "task",
			Status:    a2awire.TaskStatus{State: a2awire.StateCompleted},
			Artifacts: []a2awire.Artifact{{Parts: []a2awire.Part{{Kind: a2awire.PartKindText, Text: "Invoice 12 is paid."}}}},
		})
		if c.peerReply != nil {
			res = c.peerReply(params.Message.Text())
			res.ID = req.ID
		}
		_ = json.NewEncoder(w).Encode(res)
	})

	c.srv = httptest.NewServer(mux)
	t.Cleanup(c.srv.Close)
	// One peer, served by this same test server.
	c.peers = []peers.Peer{{
		DeploymentID: "dep-billing", AgentName: "billing-bot",
		DisplayName: "Billing Bot", URL: c.srv.URL,
	}}
	return c
}

// server builds a Server whose registry points at the fake cluster.
func (c *fakeCluster) server(t *testing.T) *Server {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	claims, err := json.Marshal(map[string]any{"sub": "dep-caller", "iss": c.srv.URL})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	token := enc([]byte(`{"alg":"HS256"}`)) + "." + enc(claims) + ".sig"

	registry, err := peers.NewFromToken(token)
	if err != nil {
		t.Fatalf("NewFromToken: %v", err)
	}
	return New(registry, peers.NewClient(), "127.0.0.1:0", "caller-bot")
}

// mcpCall drives one MCP request against the handler and returns the result.
func mcpCall(t *testing.T, s *Server, method string, params any) map[string]any {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, MCPPath, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	s.mcpHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("MCP returned %d: %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Result map[string]any  `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode MCP response: %v (%s)", err, rec.Body.String())
	}
	if envelope.Error != nil {
		t.Fatalf("MCP error: %s", envelope.Error)
	}
	return envelope.Result
}

// toolText joins the text content of a tools/call result, and reports isError.
func toolResultText(t *testing.T, result map[string]any) (string, bool) {
	t.Helper()
	isErr, _ := result["isError"].(bool)
	content, ok := result["content"].([]any)
	if !ok {
		t.Fatalf("result has no content array: %+v", result)
	}
	var b strings.Builder
	for _, item := range content {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := entry["text"].(string); ok {
			b.WriteString(text)
		}
	}
	return b.String(), isErr
}

func TestMCPExposesTheAgentNetworkTools(t *testing.T) {
	cluster := newFakeCluster(t)

	result := mcpCall(t, cluster.server(t), "tools/list", nil)

	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list returned no tools array: %+v", result)
	}
	names := map[string]bool{}
	for _, item := range tools {
		if tool, ok := item.(map[string]any); ok {
			names[tool["name"].(string)] = true
		}
	}
	for _, want := range []string{"list_agents", "ask_agent"} {
		if !names[want] {
			t.Errorf("tool %q missing; without it a model cannot answer questions about other agents", want)
		}
	}
}

func TestListAgentsNamesEachPeerWithWhatItDoes(t *testing.T) {
	cluster := newFakeCluster(t)

	result := mcpCall(t, cluster.server(t), "tools/call",
		map[string]any{"name": "list_agents", "arguments": map[string]any{}})
	text, isErr := toolResultText(t, result)

	if isErr {
		t.Fatalf("list_agents reported an error: %s", text)
	}
	for _, want := range []string{"Billing Bot", "Answers billing and invoice questions.", "Look up an invoice"} {
		if !strings.Contains(text, want) {
			t.Errorf("list_agents output missing %q; the model needs the name and what it does.\nGot:\n%s", want, text)
		}
	}
}

func TestListAgentsStillNamesAPeerWhoseCardIsUnavailable(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.peerCard = nil

	result := mcpCall(t, cluster.server(t), "tools/call",
		map[string]any{"name": "list_agents", "arguments": map[string]any{}})
	text, isErr := toolResultText(t, result)

	if isErr {
		t.Fatalf("list_agents reported an error: %s", text)
	}
	if !strings.Contains(text, "Billing Bot") {
		t.Errorf("a reachable peer must still be listed by name when its card cannot be fetched.\nGot:\n%s", text)
	}
}

func TestListAgentsSaysSoWhenThereAreNoPeers(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.peers = []peers.Peer{}

	result := mcpCall(t, cluster.server(t), "tools/call",
		map[string]any{"name": "list_agents", "arguments": map[string]any{}})
	text, isErr := toolResultText(t, result)

	if isErr {
		t.Fatalf("an empty organization is not an error: %s", text)
	}
	if !strings.Contains(text, "no other") {
		t.Errorf("output = %q, want it to state plainly that there are none", text)
	}
}

func TestAskAgentReturnsThePeersAnswer(t *testing.T) {
	cluster := newFakeCluster(t)

	result := mcpCall(t, cluster.server(t), "tools/call", map[string]any{
		"name":      "ask_agent",
		"arguments": map[string]any{"agent": "Billing Bot", "message": "is invoice 12 paid?"},
	})
	text, isErr := toolResultText(t, result)

	if isErr {
		t.Fatalf("ask_agent reported an error: %s", text)
	}
	if text != "Invoice 12 is paid." {
		t.Errorf("ask_agent returned %q, want the peer's reply text", text)
	}
	if len(cluster.asked) != 1 || cluster.asked[0] != "is invoice 12 paid?" {
		t.Errorf("peer received %v, want the caller's message forwarded verbatim", cluster.asked)
	}
}

func TestAskAgentReportsAvailableNamesForAnUnknownAgent(t *testing.T) {
	cluster := newFakeCluster(t)

	result := mcpCall(t, cluster.server(t), "tools/call", map[string]any{
		"name":      "ask_agent",
		"arguments": map[string]any{"agent": "payroll", "message": "hi"},
	})
	text, isErr := toolResultText(t, result)

	if !isErr {
		t.Error("want isError so the model knows the call did not happen")
	}
	if !strings.Contains(text, "Billing Bot") {
		t.Errorf("output = %q, want the available agent named so the model can retry", text)
	}
}

func TestAskAgentReportsAPeerThatFailedTheTask(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.peerReply = func(string) a2awire.Response {
		return a2awire.ResultResponse(nil, a2awire.Task{
			ID: "task-1", Kind: "task",
			Status: a2awire.TaskStatus{State: a2awire.StateFailed, Message: "invoice service timed out"},
		})
	}

	result := mcpCall(t, cluster.server(t), "tools/call", map[string]any{
		"name":      "ask_agent",
		"arguments": map[string]any{"agent": "billing-bot", "message": "hi"},
	})
	text, isErr := toolResultText(t, result)

	if !isErr {
		t.Error("want isError when the peer failed the task")
	}
	if !strings.Contains(text, "invoice service timed out") {
		t.Errorf("output = %q, want the peer's reason", text)
	}
	if !strings.Contains(text, "Billing Bot") {
		t.Errorf("output = %q, want the peer named so the model knows who failed", text)
	}
}

func TestAskAgentTellsTheModelHowToContinueAStillWorkingTask(t *testing.T) {
	cluster := newFakeCluster(t)
	cluster.peerReply = func(string) a2awire.Response {
		return a2awire.ResultResponse(nil, a2awire.Task{
			ID: "task-slow", ContextID: "ctx-slow", Kind: "task",
			Status: a2awire.TaskStatus{State: a2awire.StateWorking},
		})
	}

	result := mcpCall(t, cluster.server(t), "tools/call", map[string]any{
		"name":      "ask_agent",
		"arguments": map[string]any{"agent": "billing-bot", "message": "slow one"},
	})
	text, isErr := toolResultText(t, result)

	if isErr {
		t.Errorf("a slow peer is not an error: %s", text)
	}
	if !strings.Contains(text, "ctx-slow") {
		t.Errorf("output = %q, want the context_id so the model can continue", text)
	}
}

func TestRESTPeersListMatchesTheToolView(t *testing.T) {
	cluster := newFakeCluster(t)
	s := cluster.server(t)

	rec := httptest.NewRecorder()
	s.handleListPeers(rec, httptest.NewRequest(http.MethodGet, "/a2a/peers", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Agents []AgentSummary `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Agents) != 1 || body.Agents[0].Name != "Billing Bot" {
		t.Fatalf("agents = %+v, want the one peer", body.Agents)
	}
	if body.Agents[0].Description == "" {
		t.Error("description missing; the REST view should carry the same card detail the tool does")
	}
}

func TestRESTCallForwardsToThePeerAndReturnsTheReply(t *testing.T) {
	cluster := newFakeCluster(t)
	s := cluster.server(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/a2a/call",
		strings.NewReader(`{"agent":"billing-bot","message":"is invoice 12 paid?"}`))
	s.handleCall(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["text"] != "Invoice 12 is paid." {
		t.Errorf("text = %v, want the peer's reply", body["text"])
	}
	if body["done"] != true {
		t.Errorf("done = %v, want true for a completed task", body["done"])
	}
	if body["context_id"] == "" {
		t.Error("context_id missing; a caller needs it to thread a follow-up")
	}
}

func TestRESTCallReportsAnUnknownAgentAsNotFound(t *testing.T) {
	cluster := newFakeCluster(t)
	s := cluster.server(t)

	rec := httptest.NewRecorder()
	s.handleCall(rec, httptest.NewRequest(http.MethodPost, "/a2a/call",
		strings.NewReader(`{"agent":"payroll","message":"hi"}`)))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an agent that is not in this organization", rec.Code)
	}
}

func TestRESTCallRejectsAMalformedBody(t *testing.T) {
	cluster := newFakeCluster(t)
	s := cluster.server(t)

	rec := httptest.NewRecorder()
	s.handleCall(rec, httptest.NewRequest(http.MethodPost, "/a2a/call", strings.NewReader(`{not json`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDefaultListenAddrIsLoopbackOnly(t *testing.T) {
	if !strings.HasPrefix(DefaultListenAddr, "127.0.0.1:") {
		t.Errorf("DefaultListenAddr = %q; this surface calls peers as the agent, so binding beyond loopback would let any pod act as it", DefaultListenAddr)
	}
	if got := New(nil, nil, "", "agent").listenAddr; got != DefaultListenAddr {
		t.Errorf("empty listen address fell back to %q, want %q", got, DefaultListenAddr)
	}
}
