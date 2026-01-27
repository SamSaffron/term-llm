package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAddLineTool_Spec(t *testing.T) {
	tool := NewAddLineTool()
	spec := tool.Spec()

	if spec.Name != AddLineToolName {
		t.Errorf("expected name %s, got %s", AddLineToolName, spec.Name)
	}
	if spec.Description == "" {
		t.Error("expected non-empty description")
	}
	if spec.Schema == nil {
		t.Error("expected non-nil schema")
	}
}

func TestRemoveLineTool_Spec(t *testing.T) {
	tool := NewRemoveLineTool()
	spec := tool.Spec()

	if spec.Name != RemoveLineToolName {
		t.Errorf("expected name %s, got %s", RemoveLineToolName, spec.Name)
	}
	if spec.Description == "" {
		t.Error("expected non-empty description")
	}
	if spec.Schema == nil {
		t.Error("expected non-nil schema")
	}
}

func TestAddLineTool_Preview(t *testing.T) {
	tool := NewAddLineTool()

	tests := []struct {
		name     string
		args     AddLineArgs
		expected string
	}{
		{
			name:     "simple content",
			args:     AddLineArgs{Content: "New line content"},
			expected: "Add: New line content",
		},
		{
			name:     "long content truncated",
			args:     AddLineArgs{Content: "This is a very long line that should be truncated because it exceeds the maximum preview length"},
			expected: "Add: This is a very long line that should be truncat...",
		},
		{
			name:     "multiline takes first",
			args:     AddLineArgs{Content: "First line\nSecond line"},
			expected: "Add: First line",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, _ := json.Marshal(tt.args)
			preview := tool.Preview(data)
			if preview != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, preview)
			}
		})
	}
}

func TestRemoveLineTool_Preview(t *testing.T) {
	tool := NewRemoveLineTool()

	tests := []struct {
		name     string
		args     RemoveLineArgs
		expected string
	}{
		{
			name:     "simple match",
			args:     RemoveLineArgs{Match: "line to remove"},
			expected: "Remove: line to remove",
		},
		{
			name:     "long match truncated",
			args:     RemoveLineArgs{Match: "This is a very long match text that should be truncated"},
			expected: "Remove: This is a very long match text that should be t...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, _ := json.Marshal(tt.args)
			preview := tool.Preview(data)
			if preview != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, preview)
			}
		})
	}
}

func TestAddLineTool_Execute_NoExecutor(t *testing.T) {
	tool := NewAddLineTool()

	args := AddLineArgs{Content: "test"}
	data, _ := json.Marshal(args)

	_, err := tool.Execute(context.Background(), data)
	if err == nil {
		t.Error("expected error when executor not set")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("expected 'not configured' error, got %v", err)
	}
}

func TestRemoveLineTool_Execute_NoExecutor(t *testing.T) {
	tool := NewRemoveLineTool()

	args := RemoveLineArgs{Match: "test"}
	data, _ := json.Marshal(args)

	_, err := tool.Execute(context.Background(), data)
	if err == nil {
		t.Error("expected error when executor not set")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("expected 'not configured' error, got %v", err)
	}
}

func TestAddLineTool_Execute_InvalidArgs(t *testing.T) {
	tool := NewAddLineTool()
	tool.SetExecutor(func(content, after string) (string, error) {
		return "ok", nil
	})

	_, err := tool.Execute(context.Background(), []byte(`{invalid json`))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestAddLineTool_Execute_EmptyContent(t *testing.T) {
	tool := NewAddLineTool()
	tool.SetExecutor(func(content, after string) (string, error) {
		return "ok", nil
	})

	args := AddLineArgs{Content: ""}
	data, _ := json.Marshal(args)

	_, err := tool.Execute(context.Background(), data)
	if err == nil {
		t.Error("expected error for empty content")
	}
	if !strings.Contains(err.Error(), "content is required") {
		t.Errorf("expected 'content is required' error, got %v", err)
	}
}

func TestRemoveLineTool_Execute_EmptyMatch(t *testing.T) {
	tool := NewRemoveLineTool()
	tool.SetExecutor(func(match string) (string, error) {
		return "ok", nil
	})

	args := RemoveLineArgs{Match: ""}
	data, _ := json.Marshal(args)

	_, err := tool.Execute(context.Background(), data)
	if err == nil {
		t.Error("expected error for empty match")
	}
	if !strings.Contains(err.Error(), "match is required") {
		t.Errorf("expected 'match is required' error, got %v", err)
	}
}

func TestAddLineTool_Execute_Success(t *testing.T) {
	tool := NewAddLineTool()

	var receivedContent, receivedAfter string
	tool.SetExecutor(func(content, after string) (string, error) {
		receivedContent = content
		receivedAfter = after
		return "Added line", nil
	})

	args := AddLineArgs{
		Content: "New line",
		After:   "existing line",
	}
	data, _ := json.Marshal(args)

	result, err := tool.Execute(context.Background(), data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Added line" {
		t.Errorf("expected 'Added line', got %q", result)
	}
	if receivedContent != "New line" {
		t.Errorf("expected content 'New line', got %q", receivedContent)
	}
	if receivedAfter != "existing line" {
		t.Errorf("expected after 'existing line', got %q", receivedAfter)
	}
}

func TestRemoveLineTool_Execute_Success(t *testing.T) {
	tool := NewRemoveLineTool()

	var receivedMatch string
	tool.SetExecutor(func(match string) (string, error) {
		receivedMatch = match
		return "Removed line", nil
	})

	args := RemoveLineArgs{Match: "line to remove"}
	data, _ := json.Marshal(args)

	result, err := tool.Execute(context.Background(), data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Removed line" {
		t.Errorf("expected 'Removed line', got %q", result)
	}
	if receivedMatch != "line to remove" {
		t.Errorf("expected match 'line to remove', got %q", receivedMatch)
	}
}

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
