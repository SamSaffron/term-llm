package core

import (
	"strings"
	"testing"
)

func TestChooseModelAndTruncate(t *testing.T) {
	if got := ChooseModel("  ", "fb"); got != "fb" {
		t.Fatalf("ChooseModel blank = %q", got)
	}
	if got := ChooseModel("m", "fb"); got != "m" {
		t.Fatalf("ChooseModel = %q", got)
	}
	if got := Truncate("abcdef", 3); got != "abc..." {
		t.Fatalf("Truncate = %q", got)
	}
	if got := Truncate("abc", 3); got != "abc" {
		t.Fatalf("Truncate short = %q", got)
	}
}

func TestFlattenSystemUser(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Parts: []Part{{Type: PartText, Text: "sys"}}},
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "a"}, {Type: PartFile, Text: "b"}}},
		{Role: RoleTool, Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{Content: "res"}}}},
	}
	sys, user := FlattenSystemUser(msgs)
	if sys != "sys" || user != "ab\n\nres" {
		t.Fatalf("got %q %q", sys, user)
	}
}

func TestTruncateToolResultContentKeepsImages(t *testing.T) {
	parts := []ToolContentPart{
		{Type: ToolContentPartText, Text: strings.Repeat("a", 50)},
		{Type: ToolContentPartImageData, ImageData: &ToolImageData{MediaType: "image/png", Base64: "AAAA"}},
		{Type: ToolContentPartText, Text: strings.Repeat("b", 50)},
	}
	content, out := TruncateToolResultContent("", parts, 20)
	if len(out) != 3 || out[1].Type != ToolContentPartImageData {
		t.Fatalf("parts not preserved: %+v", out)
	}
	if !strings.Contains(content, "chars truncated") || parts[0].Text != strings.Repeat("a", 50) {
		t.Fatalf("unexpected content %q / input mutated", content)
	}
	if _, _, ok := ToolResultImageData(out[1]); !ok {
		t.Fatal("expected valid image data")
	}
}

func TestSanitizeToolHistory(t *testing.T) {
	call := func(id string) Part {
		return Part{Type: PartToolCall, ToolCall: &ToolCall{ID: id, Name: "n"}}
	}
	msgs := []Message{
		{Role: RoleEvent},
		{Role: RoleAssistant, Parts: []Part{call("1"), call("2")}},
		{Role: RoleTool, Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{ID: "1"}}, {Type: PartToolResult, ToolResult: &ToolResult{ID: "x"}}}},
	}
	out := SanitizeToolHistory(msgs)
	if len(out) != 2 {
		t.Fatalf("len = %d", len(out))
	}
	if out[0].Parts[1].Type != PartText || !strings.Contains(out[0].Parts[1].Text, "interrupted") {
		t.Fatalf("orphan call not converted: %+v", out[0].Parts)
	}
	if len(out[1].Parts) != 1 {
		t.Fatalf("orphan result kept: %+v", out[1].Parts)
	}
}
