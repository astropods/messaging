package store

import (
	"sync"
	"testing"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

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

	s.Add(&pb.Skill{Name: "review", Description: "Review a PR"})
	s.Add(&pb.Skill{Name: "agent-card", Description: "Update agent card"})

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

	s.Add(&pb.Skill{Name: "review", Description: "v1"})
	s.Add(&pb.Skill{Name: "review", Description: "v2"})

	if got := s.Get("review"); got == nil || got.Description != "v2" {
		t.Errorf("expected description 'v2', got %+v", got)
	}
	if len(s.List()) != 1 {
		t.Errorf("expected 1 skill after re-add, got %d", len(s.List()))
	}
}

func TestSkillsStore_IgnoresInvalid(t *testing.T) {
	s := NewSkillsStore()

	s.Add(nil)
	s.Add(&pb.Skill{Name: "", Description: "no name"})

	if got := s.List(); len(got) != 0 {
		t.Errorf("expected nil/unnamed skills to be ignored, got %d entries", len(got))
	}
}

func TestSkillsStore_Remove(t *testing.T) {
	s := NewSkillsStore()

	s.Add(&pb.Skill{Name: "review"})
	s.Add(&pb.Skill{Name: "agent-card"})
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
			s.Add(&pb.Skill{Name: "review", Description: "desc"})
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
