package store

import (
	"errors"
	"strings"
	"sync"
	"testing"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// mustAdd is a tiny helper for the happy-path table tests below — fail-fast
// on unexpected validation rejection so the test that's actually about
// behavior doesn't get muddied by `if err != nil` noise.
func mustAdd(t *testing.T, s *SkillsStore, sk *pb.Skill) {
	t.Helper()
	if err := s.Add(sk); err != nil {
		t.Fatalf("Add(%+v) returned unexpected error: %v", sk, err)
	}
}

func TestSkillsStore_EmptyByDefault(t *testing.T) {
	s := NewSkillsStore()
	if got := s.List(); len(got) != 0 {
		t.Errorf("expected empty list, got %d entries", len(got))
	}
	if s.Get("anything") != nil {
		t.Error("expected nil for missing skill")
	}
}

func TestSkillsStore_AddAndList(t *testing.T) {
	s := NewSkillsStore()

	mustAdd(t, s, &pb.Skill{Name: "review", Description: "Review a PR"})
	mustAdd(t, s, &pb.Skill{Name: "agent-card", Description: "Update agent card"})

	list := s.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(list))
	}
	// List is sorted by name, so agent-card precedes review.
	if list[0].Name != "agent-card" || list[1].Name != "review" {
		t.Errorf("expected sorted order [agent-card, review], got [%s, %s]", list[0].Name, list[1].Name)
	}
}

func TestSkillsStore_AddReplacesByName(t *testing.T) {
	s := NewSkillsStore()

	mustAdd(t, s, &pb.Skill{Name: "review", Description: "v1"})
	mustAdd(t, s, &pb.Skill{Name: "review", Description: "v2"})

	if got := s.Get("review"); got == nil || got.Description != "v2" {
		t.Errorf("expected description 'v2', got %+v", got)
	}
	if len(s.List()) != 1 {
		t.Errorf("expected 1 skill after re-add, got %d", len(s.List()))
	}
}

func TestSkillsStore_Add_InvalidName(t *testing.T) {
	// One bad name per row — covers each rule with a concrete example so a
	// failing case names the rule that broke, not a generic "invalid".
	cases := []struct {
		why     string
		skill   *pb.Skill
		wantErr error
	}{
		{"nil skill", nil, ErrInvalidSkillName},
		{"empty name", &pb.Skill{Name: ""}, ErrInvalidSkillName},
		{"whitespace-only name", &pb.Skill{Name: "   "}, ErrInvalidSkillName},
		{"starts with a digit", &pb.Skill{Name: "1up"}, ErrInvalidSkillName},
		{"starts with hyphen", &pb.Skill{Name: "-leading"}, ErrInvalidSkillName},
		{"contains a space", &pb.Skill{Name: "two words"}, ErrInvalidSkillName},
		{"contains uppercase", &pb.Skill{Name: "ReviewPR"}, ErrInvalidSkillName},
		{"contains underscore", &pb.Skill{Name: "snake_case"}, ErrInvalidSkillName},
		{"contains XML angle bracket", &pb.Skill{Name: "tag<x>"}, ErrInvalidSkillName},
		{"too long", &pb.Skill{Name: strings.Repeat("a", MaxSkillNameLength+1)}, ErrInvalidSkillName},
	}
	for _, tc := range cases {
		t.Run(tc.why, func(t *testing.T) {
			s := NewSkillsStore()
			err := s.Add(tc.skill)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tc.why)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("expected error to wrap %v, got %v", tc.wantErr, err)
			}
			if len(s.List()) != 0 {
				t.Errorf("rejected skill must not be stored, found %d entries", len(s.List()))
			}
		})
	}
}

func TestSkillsStore_Add_OversizedDescription(t *testing.T) {
	s := NewSkillsStore()
	long := strings.Repeat("x", MaxSkillDescriptionLength+1)

	if err := s.Add(&pb.Skill{Name: "ok", Description: long}); err == nil {
		t.Error("expected error for oversized description")
	}
	if err := s.Add(&pb.Skill{Name: "ok", LongDescription: long}); err == nil {
		t.Error("expected error for oversized long_description")
	}
	if len(s.List()) != 0 {
		t.Errorf("rejected skill must not be stored, found %d entries", len(s.List()))
	}

	// At-the-limit lengths are accepted (off-by-one regression guard).
	atLimit := strings.Repeat("x", MaxSkillDescriptionLength)
	mustAdd(t, s, &pb.Skill{Name: "ok", Description: atLimit, LongDescription: atLimit})
}

func TestSkillsStore_Add_NameAtMaxLength(t *testing.T) {
	s := NewSkillsStore()
	name := "a" + strings.Repeat("b", MaxSkillNameLength-1) // exactly MaxSkillNameLength chars
	mustAdd(t, s, &pb.Skill{Name: name})

	if s.Get(name) == nil {
		t.Errorf("name at the length limit should be accepted")
	}
}

func TestSkillsStore_CaseInsensitiveLookup(t *testing.T) {
	// Capitals are rejected at registration (see TestSkillsStore_Add_InvalidName),
	// but Get/Remove are case-insensitive so a playground popover query like
	// `/Agent-Card` still resolves to the lowercase-stored skill.
	s := NewSkillsStore()
	mustAdd(t, s, &pb.Skill{Name: "agent-card", Description: "lowercase canonical"})

	if s.Get("AGENT-CARD") == nil {
		t.Error("Get should match case-insensitively")
	}
	if s.Get("Agent-Card") == nil {
		t.Error("Get should match mixed-case")
	}
	if s.Get("  agent-card  ") == nil {
		t.Error("Get should trim surrounding whitespace")
	}

	s.Remove("Agent-Card")
	if s.Get("agent-card") != nil {
		t.Error("Remove should match case-insensitively")
	}
}

func TestSkillsStore_Remove(t *testing.T) {
	s := NewSkillsStore()

	mustAdd(t, s, &pb.Skill{Name: "review"})
	mustAdd(t, s, &pb.Skill{Name: "agent-card"})
	s.Remove("review")

	if s.Get("review") != nil {
		t.Error("expected review to be removed")
	}
	if s.Get("agent-card") == nil {
		t.Error("expected agent-card to remain")
	}

	// Removing an unknown name is a no-op.
	s.Remove("does-not-exist")
	if len(s.List()) != 1 {
		t.Errorf("expected 1 skill after no-op remove, got %d", len(s.List()))
	}
}

func TestSkillsStore_ConcurrentAccess(t *testing.T) {
	s := NewSkillsStore()
	var wg sync.WaitGroup

	for range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = s.Add(&pb.Skill{Name: "review", Description: "desc"})
		}()
		go func() {
			defer wg.Done()
			_ = s.List()
		}()
	}
	wg.Wait()

	if s.Get("review") == nil {
		t.Error("expected review to be present after concurrent writes")
	}
}
