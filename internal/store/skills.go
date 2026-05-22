package store

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// MaxSkillNameLength caps the kebab-case identifier. Long enough for
// composite names like "summarize-thread-and-post-back" but short enough to
// stay readable in the playground popover.
const MaxSkillNameLength = 64

// MaxSkillDescriptionLength caps both Description and LongDescription so a
// chatty agent can't bloat the catalog. ~1 KB of UTF-8 covers a tooltip plus
// a paragraph of help text.
const MaxSkillDescriptionLength = 1024

// skillNameRE enforces kebab-case: lowercase letters, digits, and hyphens;
// must start with a letter. By construction this excludes spaces, capitals,
// and `<`/`>` (so XML tags can never appear in a name).
var skillNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ErrInvalidSkillName is returned by Add when Skill.Name violates the
// name rules. The error message describes which rule was broken so the
// caller (typically the gRPC handler) can surface a useful warning.
var ErrInvalidSkillName = errors.New("invalid skill name")

// SkillsStore holds the set of user-invocable skills currently advertised by
// the agent. The agent pushes Add/Remove over the gRPC stream; the web
// adapter reads the list to populate the slash-command menu.
//
// Names are case-insensitive: agents may send any casing in Add/Remove and
// the playground may invoke with any casing — the store normalizes to
// lowercase on the way in and on every lookup.
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

// Add registers or replaces a skill. Returns an error when the skill is nil
// or any field violates the rules: the raw Name must be already lowercase
// kebab-case (capitals are a hard reject, not silently lowercased — agents
// should see the validation feedback at the source), length
// ≤ MaxSkillNameLength, descriptions ≤ MaxSkillDescriptionLength.
//
// Note: case-insensitive *lookup* is provided by Get/Remove. That's a
// client-convenience for the playground (typing `/Agent-Card` in the popover
// still finds `agent-card`); it is not a license for agents to register
// mixed-case names.
func (s *SkillsStore) Add(skill *pb.Skill) error {
	if skill == nil {
		return fmt.Errorf("%w: skill is nil", ErrInvalidSkillName)
	}
	if skill.Name == "" {
		return fmt.Errorf("%w: name is empty", ErrInvalidSkillName)
	}
	if len(skill.Name) > MaxSkillNameLength {
		return fmt.Errorf("%w: name exceeds %d characters", ErrInvalidSkillName, MaxSkillNameLength)
	}
	if !skillNameRE.MatchString(skill.Name) {
		return fmt.Errorf("%w: name must be kebab-case (lowercase a-z, 0-9, '-'; start with a letter)", ErrInvalidSkillName)
	}
	if len(skill.Description) > MaxSkillDescriptionLength {
		return fmt.Errorf("description exceeds %d characters", MaxSkillDescriptionLength)
	}
	if len(skill.LongDescription) > MaxSkillDescriptionLength {
		return fmt.Errorf("long_description exceeds %d characters", MaxSkillDescriptionLength)
	}

	// Copy so a later mutation by the caller can't reach into the stored
	// catalog. Name is already canonical (lowercase) by the regex above, so
	// the map key matches Name exactly — no normalization needed on store.
	stored := &pb.Skill{
		Name:            skill.Name,
		Description:     skill.Description,
		LongDescription: skill.LongDescription,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.skills[skill.Name] = stored
	return nil
}

// Remove deregisters a skill by name (case-insensitive). Unknown names are
// a no-op.
func (s *SkillsStore) Remove(name string) {
	key, ok := normalizeSkillName(name)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.skills, key)
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

// Get returns the skill with the given name (case-insensitive), or nil if
// not registered.
func (s *SkillsStore) Get(name string) *pb.Skill {
	key, ok := normalizeSkillName(name)
	if !ok {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.skills[key]
}

// normalizeSkillName lowercases + trims the input and validates against the
// kebab-case rules Add enforces on stored names. Returns the canonical form
// and true when the input *could* match a stored skill; returns "" and false
// otherwise so Get/Remove can short-circuit before taking the store lock —
// any name that fails the regex was never accepted by Add and can't be in
// the map.
func normalizeSkillName(name string) (string, bool) {
	canonical := strings.ToLower(strings.TrimSpace(name))
	if canonical == "" || len(canonical) > MaxSkillNameLength {
		return "", false
	}
	if !skillNameRE.MatchString(canonical) {
		return "", false
	}
	return canonical, true
}
