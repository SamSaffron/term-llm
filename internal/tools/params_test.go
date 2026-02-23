package tools

import (
	"encoding/json"
	"testing"
)

func TestWarnUnknownParams(t *testing.T) {
	tests := []struct {
		name      string
		args      json.RawMessage
		knownKeys []string
		expected  string
	}{
		{
			name:      "empty args",
			args:      json.RawMessage(`{}`),
			knownKeys: []string{"a", "b"},
			expected:  "",
		},
		{
			name:      "all known keys",
			args:      json.RawMessage(`{"a": 1, "b": 2}`),
			knownKeys: []string{"a", "b"},
			expected:  "",
		},
		{
			name:      "one unknown key",
			args:      json.RawMessage(`{"a": 1, "xyz": true}`),
			knownKeys: []string{"a"},
			expected:  "Unknown parameter 'xyz' was ignored\n",
		},
		{
			name:      "multiple unknown keys sorted",
			args:      json.RawMessage(`{"z": 1, "a": 2, "b": 3}`),
			knownKeys: []string{},
			expected:  "Unknown parameter 'a' was ignored\nUnknown parameter 'b' was ignored\nUnknown parameter 'z' was ignored\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := WarnUnknownParams(tt.args, tt.knownKeys)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}
