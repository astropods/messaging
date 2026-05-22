package store

import (
	"sort"
	"sync"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// SkillsStore holds the set of user-invocable skills currently advertised by
// the agent. The agent pushes Add/Remove over the gRPC stream; the web
// adapter reads the list to populate the slash-command menu.
type SkillsStore struct {
	skills map[string]*pb.Skill
	mu     sync.RWMutex
}

// NewSkillsStore creates an empty SkillsStore.
func NewSkillsStore() *SkillsStore {
	return &SkillsStore{
		skills: make(map[string]*pb.Skill),
	}
}

// Add registers or replaces a skill keyed by Skill.Name. A nil skill or one
// with an empty name is ignored — the agent should not be able to inject
// unaddressable entries.
func (s *SkillsStore) Add(skill *pb.Skill) {
	if skill == nil || skill.Name == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skills[skill.Name] = skill
}

// Remove deregisters a skill by name. Unknown names are a no-op.
func (s *SkillsStore) Remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.skills, name)
}

// List returns all skills sorted by name for stable ordering in the UI.
func (s *SkillsStore) List() []*pb.Skill {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*pb.Skill, 0, len(s.skills))
	for _, sk := range s.skills {
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the skill with the given name, or nil if not registered.
func (s *SkillsStore) Get(name string) *pb.Skill {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.skills[name]
}
