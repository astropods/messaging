package store

import (
	"sync"

	pb "github.com/astromode-ai/astro-messaging/pkg/gen/astro/messaging/v1"
)

// AgentConfigStore stores the agent's declared configuration in memory.
type AgentConfigStore struct {
	config *pb.AgentConfig
	mu     sync.RWMutex
}

// NewAgentConfigStore creates a new AgentConfigStore.
func NewAgentConfigStore() *AgentConfigStore {
	return &AgentConfigStore{}
}

// Set stores the agent config.
func (s *AgentConfigStore) Set(config *pb.AgentConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config = config
}

// Get returns the stored agent config, or nil if not yet received.
func (s *AgentConfigStore) Get() *pb.AgentConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}
