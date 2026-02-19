package store

import (
	"sync"
	"testing"

	pb "github.com/astromode-ai/astro-messaging/pkg/gen/astro/messaging/v1"
)

func TestAgentConfigStore_InitiallyNil(t *testing.T) {
	s := NewAgentConfigStore()
	if s.Get() != nil {
		t.Error("expected nil config before any Set")
	}
}

func TestAgentConfigStore_SetAndGet(t *testing.T) {
	s := NewAgentConfigStore()

	config := &pb.AgentConfig{
		SystemPrompt: "You are a helpful assistant.",
		Tools: []*pb.AgentToolConfig{
			{
				Name:        "search",
				Title:       "Web Search",
				Description: "Search the web",
				Type:        "other",
			},
		},
	}

	s.Set(config)

	got := s.Get()
	if got == nil {
		t.Fatal("expected non-nil config after Set")
	}
	if got.SystemPrompt != "You are a helpful assistant." {
		t.Errorf("SystemPrompt: expected 'You are a helpful assistant.', got %q", got.SystemPrompt)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(got.Tools))
	}
	if got.Tools[0].Name != "search" {
		t.Errorf("Tool name: expected 'search', got %q", got.Tools[0].Name)
	}
}

func TestAgentConfigStore_OverwritesPrevious(t *testing.T) {
	s := NewAgentConfigStore()

	s.Set(&pb.AgentConfig{SystemPrompt: "first"})
	s.Set(&pb.AgentConfig{SystemPrompt: "second"})

	got := s.Get()
	if got.SystemPrompt != "second" {
		t.Errorf("expected overwritten prompt 'second', got %q", got.SystemPrompt)
	}
}

func TestAgentConfigStore_SetNilConfig(t *testing.T) {
	s := NewAgentConfigStore()

	// Set a real config first
	s.Set(&pb.AgentConfig{SystemPrompt: "hello"})
	// Then set nil (should clear it)
	s.Set(nil)

	if s.Get() != nil {
		t.Error("expected nil config after setting nil")
	}
}

func TestAgentConfigStore_EmptyConfig(t *testing.T) {
	s := NewAgentConfigStore()

	s.Set(&pb.AgentConfig{})

	got := s.Get()
	if got == nil {
		t.Fatal("expected non-nil config")
	}
	if got.SystemPrompt != "" {
		t.Errorf("expected empty system prompt, got %q", got.SystemPrompt)
	}
	if len(got.Tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(got.Tools))
	}
}

func TestAgentConfigStore_ConfigWithGraph(t *testing.T) {
	s := NewAgentConfigStore()

	config := &pb.AgentConfig{
		SystemPrompt: "Agent with graph tool",
		Tools: []*pb.AgentToolConfig{
			{
				Name:        "workflow",
				Title:       "Workflow",
				Description: "A graph workflow",
				Type:        "graph",
				Graph: &pb.AgentToolGraph{
					Nodes: []*pb.AgentToolGraphNode{
						{Id: "start", Name: "Start", Type: "start"},
						{Id: "process", Name: "Process", Type: "step"},
					},
					Edges: []*pb.AgentToolGraphEdge{
						{Id: "e1", Source: "start", Target: "process"},
					},
				},
			},
		},
	}

	s.Set(config)

	got := s.Get()
	if got.Tools[0].Graph == nil {
		t.Fatal("expected graph to be non-nil")
	}
	if len(got.Tools[0].Graph.Nodes) != 2 {
		t.Errorf("expected 2 nodes, got %d", len(got.Tools[0].Graph.Nodes))
	}
	if len(got.Tools[0].Graph.Edges) != 1 {
		t.Errorf("expected 1 edge, got %d", len(got.Tools[0].Graph.Edges))
	}
	if got.Tools[0].Graph.Edges[0].Source != "start" {
		t.Errorf("edge source: expected 'start', got %q", got.Tools[0].Graph.Edges[0].Source)
	}
}

func TestAgentConfigStore_ConcurrentAccess(t *testing.T) {
	s := NewAgentConfigStore()

	var wg sync.WaitGroup

	// Concurrent writers
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			s.Set(&pb.AgentConfig{
				SystemPrompt: "prompt",
				Tools: []*pb.AgentToolConfig{
					{Name: "tool", Title: "Tool", Type: "other"},
				},
			})
		}(i)
	}

	// Concurrent readers
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Get()
		}()
	}

	wg.Wait()

	// After all goroutines finish, should have a valid config
	got := s.Get()
	if got == nil {
		t.Fatal("expected non-nil config after concurrent writes")
	}
	if got.SystemPrompt != "prompt" {
		t.Errorf("expected prompt 'prompt', got %q", got.SystemPrompt)
	}
}
