package ui

import (
	"reflect"
	"testing"
)

func TestComputeLCS(t *testing.T) {
	tests := []struct {
		name     string
		old      []string
		new      []string
		expected []string
	}{
		{
			name:     "identical",
			old:      []string{"a", "b", "c"},
			new:      []string{"a", "b", "c"},
			expected: []string{"a", "b", "c"},
		},
		{
			name:     "empty old",
			old:      []string{},
			new:      []string{"a", "b"},
			expected: nil,
		},
		{
			name:     "empty new",
			old:      []string{"a", "b"},
			new:      []string{},
			expected: nil,
		},
		{
			name:     "no common",
			old:      []string{"a", "b"},
			new:      []string{"c", "d"},
			expected: nil,
		},
		{
			name:     "one line changed in middle",
			old:      []string{"a", "b", "c"},
			new:      []string{"a", "x", "c"},
			expected: []string{"a", "c"},
		},
		{
			name:     "insertion",
			old:      []string{"a", "c"},
			new:      []string{"a", "b", "c"},
			expected: []string{"a", "c"},
		},
		{
			name:     "deletion",
			old:      []string{"a", "b", "c"},
			new:      []string{"a", "c"},
			expected: []string{"a", "c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := computeLCS(tt.old, tt.new)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("computeLCS() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestComputeChanges(t *testing.T) {
	tests := []struct {
		name     string
		old      []string
		new      []string
		expected []change
	}{
		{
			name:     "no changes",
			old:      []string{"a", "b", "c"},
			new:      []string{"a", "b", "c"},
			expected: nil,
		},
		{
			name: "single line change",
			old:  []string{"a", "b", "c"},
			new:  []string{"a", "x", "c"},
			expected: []change{
				{oldStart: 1, oldCount: 1, newStart: 1, newCount: 1},
			},
		},
		{
			name: "two separate changes",
			old:  []string{"a", "b", "c", "d", "e"},
			new:  []string{"a", "x", "c", "y", "e"},
			expected: []change{
				{oldStart: 1, oldCount: 1, newStart: 1, newCount: 1},
				{oldStart: 3, oldCount: 1, newStart: 3, newCount: 1},
			},
		},
		{
			name: "insertion at start",
			old:  []string{"b", "c"},
			new:  []string{"a", "b", "c"},
			expected: []change{
				{oldStart: 0, oldCount: 0, newStart: 0, newCount: 1},
			},
		},
		{
			name: "insertion in middle",
			old:  []string{"a", "c"},
			new:  []string{"a", "b", "c"},
			expected: []change{
				{oldStart: 1, oldCount: 0, newStart: 1, newCount: 1},
			},
		},
		{
			name: "deletion",
			old:  []string{"a", "b", "c"},
			new:  []string{"a", "c"},
			expected: []change{
				{oldStart: 1, oldCount: 1, newStart: 1, newCount: 0},
			},
		},
		{
			name: "multiple insertions",
			old:  []string{"a", "d"},
			new:  []string{"a", "b", "c", "d"},
			expected: []change{
				{oldStart: 1, oldCount: 0, newStart: 1, newCount: 2},
			},
		},
		{
			name: "change at start and end",
			old:  []string{"a", "b", "c"},
			new:  []string{"x", "b", "y"},
			expected: []change{
				{oldStart: 0, oldCount: 1, newStart: 0, newCount: 1},
				{oldStart: 2, oldCount: 1, newStart: 2, newCount: 1},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := computeChanges(tt.old, tt.new)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("computeChanges() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestWrapLine(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		maxWidth int
		expected []string
	}{
		{
			name:     "short line no wrap",
			line:     "hello",
			maxWidth: 20,
			expected: []string{"hello"},
		},
		{
			name:     "exact width",
			line:     "hello",
			maxWidth: 5,
			expected: []string{"hello"},
		},
		{
			name:     "wrap at space",
			line:     "hello world",
			maxWidth: 8,
			expected: []string{"hello ", "  world"},
		},
		{
			name:     "wrap long word",
			line:     "abcdefghij",
			maxWidth: 5,
			expected: []string{"abcde", "  fgh", "  ij"}, // continuation lines have width-2
		},
		{
			name:     "zero width returns original",
			line:     "hello",
			maxWidth: 0,
			expected: []string{"hello"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := wrapLine(tt.line, tt.maxWidth, 0)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("wrapLine(%q, %d, 0) = %v, want %v", tt.line, tt.maxWidth, result, tt.expected)
			}
		})
	}
}
