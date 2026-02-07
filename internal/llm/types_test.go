package llm

import (
	"encoding/json"
	"testing"
)

func TestStripDisplayMarkers(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no markers",
			input: "Edited test.go: replaced 5 lines with 7 lines.",
			want:  "Edited test.go: replaced 5 lines with 7 lines.",
		},
		{
			name:  "diff marker removed",
			input: "Edited test.go: replaced 5 lines with 7 lines.\n__DIFF__:abc123base64blob==",
			want:  "Edited test.go: replaced 5 lines with 7 lines.",
		},
		{
			name:  "image marker removed",
			input: "Generated image.\n__IMAGE__:/tmp/image.png",
			want:  "Generated image.",
		},
		{
			name:  "multiple markers removed",
			input: "Applied changes to a.go: 10 lines -> 12 lines.\n\n__DIFF__:blob1\nApplied changes to b.go: 5 lines -> 6 lines.\n\n__DIFF__:blob2",
			want:  "Applied changes to a.go: 10 lines -> 12 lines.\n\nApplied changes to b.go: 5 lines -> 6 lines.",
		},
		{
			name:  "plain content with trailing newline preserved",
			input: "file1.txt\nfile2.txt\n",
			want:  "file1.txt\nfile2.txt\n",
		},
		{
			name:  "plain content with trailing space preserved",
			input: "some output ",
			want:  "some output ",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "only marker",
			input: "__DIFF__:abc123",
			want:  "",
		},
		{
			name:  "marker with trailing newline",
			input: "Updated /path/file.go: 10 lines -> 12 lines.\n__DIFF__:abc123\n",
			want:  "Updated /path/file.go: 10 lines -> 12 lines.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripDisplayMarkers(tt.input)
			if got != tt.want {
				t.Errorf("stripDisplayMarkers() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestToolResultMessage_DisplayField(t *testing.T) {
	raw := "Edited test.go: replaced 5 lines with 7 lines.\n__DIFF__:abc123base64blob=="
	msg := ToolResultMessage("call-1", "edit_file", raw, nil)

	if len(msg.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(msg.Parts))
	}
	result := msg.Parts[0].ToolResult
	if result == nil {
		t.Fatal("expected ToolResult to be non-nil")
	}

	// Content should be clean (no markers)
	if result.Content != "Edited test.go: replaced 5 lines with 7 lines." {
		t.Errorf("Content = %q, want clean text without marker", result.Content)
	}

	// Display should preserve raw output
	if result.Display != raw {
		t.Errorf("Display = %q, want raw output %q", result.Display, raw)
	}
}

func TestToolResultMessage_NoMarkers(t *testing.T) {
	raw := "Created new file: /tmp/test.go (10 lines)."
	msg := ToolResultMessage("call-1", "write_file", raw, nil)

	result := msg.Parts[0].ToolResult
	// When no markers, Content and Display should both match
	if result.Content != raw {
		t.Errorf("Content = %q, want %q", result.Content, raw)
	}
	if result.Display != raw {
		t.Errorf("Display = %q, want %q", result.Display, raw)
	}
}

func TestToolResultMessage_ImageMarker(t *testing.T) {
	raw := "Generated image successfully.\n__IMAGE__:/tmp/generated.png"
	msg := ToolResultMessage("call-2", "generate_image", raw, nil)

	result := msg.Parts[0].ToolResult
	if result.Content != "Generated image successfully." {
		t.Errorf("Content = %q, want clean text without image marker", result.Content)
	}
	if result.Display != raw {
		t.Errorf("Display = %q, want raw output %q", result.Display, raw)
	}
}

func TestToolResult_SessionRoundTrip(t *testing.T) {
	original := ToolResult{
		ID:      "call-1",
		Name:    "edit_file",
		Content: "Edited test.go: replaced 5 lines with 7 lines.",
		Display: "Edited test.go: replaced 5 lines with 7 lines.\n__DIFF__:abc123base64blob==",
		IsError: false,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var restored ToolResult
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if restored.Content != original.Content {
		t.Errorf("Content = %q, want %q", restored.Content, original.Content)
	}
	if restored.Display != original.Display {
		t.Errorf("Display = %q, want %q", restored.Display, original.Display)
	}
	if restored.ID != original.ID {
		t.Errorf("ID = %q, want %q", restored.ID, original.ID)
	}
	if restored.Name != original.Name {
		t.Errorf("Name = %q, want %q", restored.Name, original.Name)
	}
}

func TestStripDisplayMarkers_TrailingBlankLines(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "marker between content blocks preserves blank line",
			input: "Line 1\n\n__DIFF__:abc\nLine 2",
			want:  "Line 1\n\nLine 2",
		},
		{
			name:  "trailing marker eats preceding blank lines",
			input: "Line 1\n\n__DIFF__:abc",
			want:  "Line 1",
		},
		{
			name:  "trailing image marker eats preceding blank lines",
			input: "Line 1\n\n__IMAGE__:/tmp/img.png",
			want:  "Line 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripDisplayMarkers(tt.input)
			if got != tt.want {
				t.Errorf("stripDisplayMarkers() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestToolErrorMessage_NoDisplayField(t *testing.T) {
	msg := ToolErrorMessage("call-1", "edit_file", "file not found", nil)

	result := msg.Parts[0].ToolResult
	if result.Display != "" {
		t.Errorf("ToolErrorMessage should not set Display, got %q", result.Display)
	}
	if result.Content != "file not found" {
		t.Errorf("Content = %q, want %q", result.Content, "file not found")
	}
	if !result.IsError {
		t.Error("expected IsError = true")
	}
}
