package tools

import (
	"testing"
)

func TestFindBestMatch(t *testing.T) {
	tests := []struct {
		name       string
		lines      []string
		searchText string
		expected   int
	}{
		{
			name:       "exact match",
			lines:      []string{"Line one", "Line two", "Line three"},
			searchText: "Line two",
			expected:   1,
		},
		{
			name:       "case insensitive exact match",
			lines:      []string{"Line one", "LINE TWO", "Line three"},
			searchText: "line two",
			expected:   1,
		},
		{
			name:       "contains match",
			lines:      []string{"# Header", "Some content here", "More content"},
			searchText: "content here",
			expected:   1,
		},
		{
			name:       "partial word match",
			lines:      []string{"## Implementation", "- Add feature", "- Fix bug"},
			searchText: "Implementation",
			expected:   0,
		},
		{
			name:       "word overlap",
			lines:      []string{"Add user authentication", "Fix login bug", "Update tests"},
			searchText: "user auth",
			expected:   0,
		},
		{
			name:       "no match",
			lines:      []string{"Line one", "Line two", "Line three"},
			searchText: "completely different",
			expected:   -1,
		},
		{
			name:       "empty search",
			lines:      []string{"Line one", "Line two"},
			searchText: "",
			expected:   -1,
		},
		{
			name:       "empty lines",
			lines:      []string{},
			searchText: "something",
			expected:   -1,
		},
		{
			name:       "best coverage match",
			lines:      []string{"Short", "A very long line with many words", "Medium line"},
			searchText: "long line",
			expected:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FindBestMatch(tt.lines, tt.searchText)
			if result != tt.expected {
				t.Errorf("expected %d, got %d", tt.expected, result)
			}
		})
	}
}
