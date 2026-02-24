package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/astropods/messaging/internal/adapter"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name     string
		opts     []Option
		wantAddr string
		wantMaps bool
	}{
		{
			name:     "default_listen_addr",
			opts:     nil,
			wantAddr: defaultListenAddr,
			wantMaps: true,
		},
		{
			name:     "with_listen_addr",
			opts:     []Option{WithListenAddr(":9999")},
			wantAddr: ":9999",
			wantMaps: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(tt.opts...)
			assert.Equal(t, tt.wantAddr, a.listenAddr)
			if tt.wantMaps {
				require.NotNil(t, a.pending)
				require.NotNil(t, a.buffers)
			}
		})
	}
}

func TestMCPAdapter_GetPlatformName(t *testing.T) {
	a := New()
	assert.Equal(t, "mcp", a.GetPlatformName())
}

func TestMCPAdapter_Capabilities(t *testing.T) {
	a := New()
	caps := a.Capabilities()
	assert.False(t, caps.SupportsThreads, "MCP should not support threads")
	assert.True(t, caps.SupportsStreaming, "MCP should support streaming")
}

func TestMCPAdapter_IsHealthy(t *testing.T) {
	a := New()
	assert.False(t, a.IsHealthy(t.Context()), "IsHealthy should be false before Start")
}

func TestMCPAdapter_Initialize(t *testing.T) {
	a := New()
	cfg := adapter.Config{}
	err := a.Initialize(t.Context(), cfg)
	require.NoError(t, err)
	assert.Equal(t, cfg, a.config)
}

func TestMCPAdapter_UpdateFromAgentConfig(t *testing.T) {
	tests := []struct {
		name          string
		config        *pb.AgentConfig
		setupServer   bool
		wantServerNil bool
		wantServerSet bool
	}{
		{
			name:          "nil_config_no_panic",
			config:        nil,
			setupServer:   false,
			wantServerNil: true,
		},
		{
			name:          "with_mcp_definition",
			config:        nil,
			setupServer:   true,
			wantServerNil: false,
			wantServerSet: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New()
			if tt.setupServer {
				a.mu.Lock()
				a.mcpServer = a.buildMCPServerLocked(nil)
				a.mu.Unlock()
				tt.config = &pb.AgentConfig{
					McpDefinition: []*pb.AgentToolConfig{
						{
							Name:            "fetch_readme",
							Description:     "Fetch a README",
							InputSchemaJson: `{"type":"object","required":["repo"]}`,
						},
					},
				}
			}
			a.UpdateFromAgentConfig(tt.config)
			a.mu.Lock()
			svc := a.mcpServer
			a.mu.Unlock()
			if tt.wantServerNil {
				assert.Nil(t, svc)
			}
			if tt.wantServerSet {
				require.NotNil(t, svc)
			}
		})
	}
}

func TestMCPAdapter_HandleAgentResponse(t *testing.T) {
	t.Run("empty_conversation_id", func(t *testing.T) {
		a := New()
		resp := &pb.AgentResponse{ConversationId: ""}
		err := a.HandleAgentResponse(t.Context(), resp)
		require.NoError(t, err)
	})

	t.Run("unknown_conversation_id", func(t *testing.T) {
		a := New()
		resp := &pb.AgentResponse{
			ConversationId: "unknown-id",
			Payload:        &pb.AgentResponse_Content{Content: &pb.ContentChunk{Type: pb.ContentChunk_END}},
		}
		err := a.HandleAgentResponse(t.Context(), resp)
		require.NoError(t, err)
	})

	t.Run("end_delivers_to_pending", func(t *testing.T) {
		a := New()
		convID := "test-conv-1"
		ch := make(chan string, 1)
		a.mu.Lock()
		a.pending[convID] = ch
		a.buffers[convID] = &strings.Builder{}
		a.mu.Unlock()

		resp := &pb.AgentResponse{
			ConversationId: convID,
			Payload:        &pb.AgentResponse_Content{Content: &pb.ContentChunk{Type: pb.ContentChunk_END}},
		}
		err := a.HandleAgentResponse(t.Context(), resp)
		require.NoError(t, err)

		select {
		case text := <-ch:
			assert.Empty(t, text)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for response on pending channel")
		}
		a.mu.Lock()
		_, stillPending := a.pending[convID]
		a.mu.Unlock()
		assert.False(t, stillPending)
	})

	t.Run("delta_then_end_accumulates", func(t *testing.T) {
		a := New()
		convID := "test-conv-2"
		ch := make(chan string, 1)
		a.mu.Lock()
		a.pending[convID] = ch
		a.buffers[convID] = &strings.Builder{}
		a.mu.Unlock()

		for _, frag := range []string{"hello ", "world"} {
			resp := &pb.AgentResponse{
				ConversationId: convID,
				Payload: &pb.AgentResponse_Content{
					Content: &pb.ContentChunk{Type: pb.ContentChunk_DELTA, Content: frag},
				},
			}
			err := a.HandleAgentResponse(t.Context(), resp)
			require.NoError(t, err)
		}
		resp := &pb.AgentResponse{
			ConversationId: convID,
			Payload:        &pb.AgentResponse_Content{Content: &pb.ContentChunk{Type: pb.ContentChunk_END}},
		}
		err := a.HandleAgentResponse(t.Context(), resp)
		require.NoError(t, err)

		select {
		case text := <-ch:
			assert.Equal(t, "hello world", text)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for response on pending channel")
		}
	})

	t.Run("error_delivers_to_pending", func(t *testing.T) {
		a := New()
		convID := "test-conv-3"
		ch := make(chan string, 1)
		a.mu.Lock()
		a.pending[convID] = ch
		a.buffers[convID] = &strings.Builder{}
		a.mu.Unlock()

		resp := &pb.AgentResponse{
			ConversationId: convID,
			Payload: &pb.AgentResponse_Error{
				Error: &pb.ErrorResponse{Message: "something failed"},
			},
		}
		err := a.HandleAgentResponse(t.Context(), resp)
		require.NoError(t, err)

		select {
		case text := <-ch:
			assert.Equal(t, "[Error] something failed", text)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for error on pending channel")
		}
		a.mu.Lock()
		_, stillPending := a.pending[convID]
		a.mu.Unlock()
		assert.False(t, stillPending)
	})

	t.Run("error_empty_message", func(t *testing.T) {
		a := New()
		convID := "test-conv-4"
		ch := make(chan string, 1)
		a.mu.Lock()
		a.pending[convID] = ch
		a.buffers[convID] = &strings.Builder{}
		a.mu.Unlock()

		resp := &pb.AgentResponse{
			ConversationId: convID,
			Payload:        &pb.AgentResponse_Error{Error: nil},
		}
		err := a.HandleAgentResponse(t.Context(), resp)
		require.NoError(t, err)

		select {
		case text := <-ch:
			assert.Equal(t, "[Error] agent error", text)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for error on pending channel")
		}
	})
}

func TestMCPAdapter_HydrateThread(t *testing.T) {
	a := New()
	err := a.HydrateThread(t.Context(), "any-id", nil)
	require.NoError(t, err)
}

func TestBuildMCPServerLocked(t *testing.T) {
	tests := []struct {
		name   string
		config *pb.AgentConfig
	}{
		{
			name:   "nil_config_send_message_only",
			config: nil,
		},
		{
			name:   "empty_mcp_definition_send_message_only",
			config: &pb.AgentConfig{McpDefinition: []*pb.AgentToolConfig{}},
		},
		{
			name: "skips_empty_tool_name",
			config: &pb.AgentConfig{
				McpDefinition: []*pb.AgentToolConfig{
					{Name: "  ", Description: "ignored"},
					{Name: "ok_tool", Description: "used"},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New()
			server := a.buildMCPServerLocked(tt.config)
			require.NotNil(t, server)
		})
	}
}

// Ensure MCPAdapter implements adapter.Adapter
var _ adapter.Adapter = (*MCPAdapter)(nil)

// Ensure MCPAdapter implements adapter.AgentConfigReceiver
var _ adapter.AgentConfigReceiver = (*MCPAdapter)(nil)
