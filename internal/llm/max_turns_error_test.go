package llm

import (
	"fmt"
	"testing"
)

func TestIsMaxTurnsExceeded(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "typed", err: &MaxTurnsExceededError{MaxTurns: 600}, want: true},
		{name: "wrapped typed", err: fmt.Errorf("wrap: %w", &MaxTurnsExceededError{MaxTurns: 3}), want: true},
		{name: "legacy string", err: fmt.Errorf("agentic loop exceeded max turns (3)"), want: true},
		{name: "other", err: fmt.Errorf("context overflow"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsMaxTurnsExceeded(tt.err); got != tt.want {
				t.Fatalf("IsMaxTurnsExceeded(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
