package ui

import (
	"testing"
)

func TestHasDiff(t *testing.T) {
	tests := []struct {
		name     string
		old      string
		new      string
		expected bool
	}{
		{
			name:     "identical",
			old:      "hello\nworld",
			new:      "hello\nworld",
			expected: false,
		},
		{
			name:     "different",
			old:      "hello\nworld",
			new:      "hello\nearth",
			expected: true,
		},
		{
			name:     "blank line difference",
			old:      "a\n\nb",
			new:      "a\n\n\nb",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HasDiff(tt.old, tt.new)
			if got != tt.expected {
				t.Errorf("HasDiff() = %v, want %v", got, tt.expected)
			}
		})
	}
}

