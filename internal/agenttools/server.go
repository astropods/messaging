// Package agenttools exposes peer discovery and peer calls to the agent
// running beside this sidecar.
//
// Two surfaces over the same operations. A plain REST pair for agent code that
// wants to call it directly, and an MCP endpoint so an MCP-capable agent gets
// the same operations as model-callable tools with no agent code at all. The
// MCP half is the one that makes "do you see other agents?" answerable: a
// model needs tools, not an API.
//
// Bound to loopback. The agent container shares the pod's network namespace, so
// 127.0.0.1 reaches this from the agent and from nowhere else. That matters
// because these endpoints spend the deployment's own identity to call peers:
// anything that can reach them can act as this agent.
package agenttools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/astropods/messaging/internal/peers"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultListenAddr binds loopback only, deliberately. See the package doc.
const DefaultListenAddr = "127.0.0.1:8110"

// MCPPath is where the MCP Streamable HTTP endpoint is served.
const MCPPath = "/mcp"

// cardTimeout bounds the per-peer card fetch that enriches list_agents. Short:
// a peer that is slow to describe itself must not stall the whole listing, and
// the name alone is still a useful answer.
const cardTimeout = 3 * time.Second

// Server serves the agent-facing peer surface on loopback.
type Server struct {
	registry   *peers.Registry
	client     *peers.Client
	listenAddr string
	agentName  string
	server     *http.Server
}

func New(registry *peers.Registry, client *peers.Client, listenAddr, agentName string) *Server {
	if listenAddr == "" {
		listenAddr = DefaultListenAddr
	}
	return &Server{registry: registry, client: client, listenAddr: listenAddr, agentName: agentName}
}

// Start serves until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /a2a/peers", s.handleListPeers)
	mux.HandleFunc("POST /a2a/call", s.handleCall)
	mux.Handle(MCPPath, s.mcpHandler())
	mux.Handle(MCPPath+"/", s.mcpHandler())

	s.server = &http.Server{
		Addr:              s.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: a peer call can legitimately run for as long as the
		// client's own Ask budget allows.
		IdleTimeout: 120 * time.Second,
	}

	slog.Info("[A2A] Starting agent tools server", "addr", s.listenAddr, "mcp", MCPPath)
	go func() {
		if err := s.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("[A2A] Agent tools server error", "err", err)
		}
	}()

	<-ctx.Done()
	return s.Stop(context.Background())
}

func (s *Server) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.server.Shutdown(shutdownCtx)
}

// --- MCP ---

// listAgentsArgs is empty: listing takes no input. MCP still requires a schema,
// which the SDK derives from this type.
type listAgentsArgs struct{}

type askAgentArgs struct {
	Agent     string `json:"agent" jsonschema:"the name of the agent to ask, as returned by list_agents"`
	Message   string `json:"message" jsonschema:"the question or instruction to send to that agent"`
	ContextID string `json:"context_id,omitempty" jsonschema:"continue an earlier conversation by passing the context_id from a previous ask_agent reply"`
}

// AgentSummary is one entry in list_agents' structured result.
type AgentSummary struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Skills      []string `json:"skills,omitempty"`
}

func (s *Server) mcpHandler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "astropods-agent-network",
		Version: "1.0.0",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: "list_agents",
		Description: "List the other AI agents in this organization that can be contacted, " +
			"with what each one does. Call this to answer questions about which agents " +
			"are available or reachable.",
	}, s.toolListAgents)

	mcp.AddTool(server, &mcp.Tool{
		Name: "ask_agent",
		Description: "Send a question or instruction to another agent in this organization " +
			"and return its reply. Use list_agents first to find the agent's name.",
	}, s.toolAskAgent)

	// Stateless: this server has exactly one client, the agent in the same pod,
	// and nothing here needs to survive between requests.
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
}

func (s *Server) toolListAgents(ctx context.Context, _ *mcp.CallToolRequest, _ listAgentsArgs) (*mcp.CallToolResult, any, error) {
	summaries, err := s.summarize(ctx)
	if err != nil {
		return toolError(fmt.Sprintf("Could not look up the other agents: %v", err)), nil, nil
	}
	if len(summaries) == 0 {
		return toolText("There are no other A2A-enabled agents in this organization."), summaries, nil
	}
	return toolText(renderAgentList(summaries)), summaries, nil
}

func (s *Server) toolAskAgent(ctx context.Context, _ *mcp.CallToolRequest, args askAgentArgs) (*mcp.CallToolResult, any, error) {
	peer, err := s.registry.Find(ctx, args.Agent)
	if err != nil {
		// Returned as tool content, not a Go error: the model should read the
		// available names and retry, not see a transport failure.
		return toolError(err.Error()), nil, nil
	}
	reply, err := s.client.Ask(ctx, peer.URL, args.ContextID, args.Message)
	if err != nil {
		return toolError(fmt.Sprintf("%s could not answer: %v", peer.Name(), err)), nil, nil
	}
	if !reply.Done {
		return toolText(fmt.Sprintf(
			"%s is still working (task %s). Ask again with context_id %q to continue.",
			peer.Name(), reply.TaskID, reply.ContextID)), reply, nil
	}
	return toolText(reply.Text), reply, nil
}

// summarize lists peers and enriches each with its agent card, concurrently.
// A peer whose card cannot be fetched still appears, by name: a reachable agent
// we can't describe is more useful to report than to hide.
func (s *Server) summarize(ctx context.Context) ([]AgentSummary, error) {
	found, err := s.registry.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]AgentSummary, len(found))
	var wg sync.WaitGroup
	for i, peer := range found {
		out[i] = AgentSummary{Name: peer.Name()}
		wg.Add(1)
		go func(i int, peer peers.Peer) {
			defer wg.Done()
			cardCtx, cancel := context.WithTimeout(ctx, cardTimeout)
			defer cancel()
			card, err := s.client.Card(cardCtx, peer.URL)
			if err != nil {
				slog.Debug("[A2A] Peer card fetch failed", "peer", peer.Name(), "err", err)
				return
			}
			out[i].Description = card.Description
			for _, skill := range card.Skills {
				out[i].Skills = append(out[i].Skills, skill.Name)
			}
		}(i, peer)
	}
	wg.Wait()
	return out, nil
}

// --- REST ---

func (s *Server) handleListPeers(w http.ResponseWriter, r *http.Request) {
	summaries, err := s.summarize(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": summaries})
}

type callRequest struct {
	Agent     string `json:"agent"`
	Message   string `json:"message"`
	ContextID string `json:"context_id,omitempty"`
}

func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	var body callRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be JSON with agent and message"})
		return
	}
	peer, err := s.registry.Find(r.Context(), body.Agent)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	reply, err := s.client.Ask(r.Context(), peer.URL, body.ContextID, body.Message)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agent":      peer.Name(),
		"text":       reply.Text,
		"task_id":    reply.TaskID,
		"context_id": reply.ContextID,
		"state":      reply.State,
		"done":       reply.Done,
	})
}

func renderAgentList(summaries []AgentSummary) string {
	noun := "agents"
	if len(summaries) == 1 {
		noun = "agent"
	}
	out := fmt.Sprintf("%d other %s in this organization:\n", len(summaries), noun)
	for _, a := range summaries {
		out += "\n- " + a.Name
		if a.Description != "" {
			out += ": " + a.Description
		}
		if len(a.Skills) > 0 {
			out += "\n  Skills: "
			for i, skill := range a.Skills {
				if i > 0 {
					out += ", "
				}
				out += skill
			}
		}
	}
	return out
}

func toolText(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func toolError(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("[A2A] Error writing agent tools response", "err", err)
	}
}
