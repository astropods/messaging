package adapter

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrNoAgentStream_DirectMatch(t *testing.T) {
	if !errors.Is(ErrNoAgentStream, ErrNoAgentStream) {
		t.Error("ErrNoAgentStream should match itself")
	}
}

func TestErrNoAgentStream_WrappedMatch(t *testing.T) {
	wrapped := fmt.Errorf("%w for conversation: conv-123", ErrNoAgentStream)
	if !errors.Is(wrapped, ErrNoAgentStream) {
		t.Error("wrapped ErrNoAgentStream should match via errors.Is")
	}
}

func TestErrNoAgentStream_UnrelatedNoMatch(t *testing.T) {
	other := fmt.Errorf("something else went wrong")
	if errors.Is(other, ErrNoAgentStream) {
		t.Error("unrelated error should not match ErrNoAgentStream")
	}
}
