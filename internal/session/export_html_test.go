package session

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestExportToHTMLRendersTranscriptAndMetadata(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 34, 0, 0, time.UTC)
	sess := &Session{
		ID: "abcdef123456", Name: "HTML export", GeneratedLongTitle: "A polished transcript",
		Provider: "OpenAI", Model: "gpt-test", Agent: "developer", Origin: OriginWeb,
		Mode: ModeAsk, Status: StatusComplete, ReasoningEffort: "high", CreatedAt: now, UpdatedAt: now.Add(time.Minute),
		UserTurns: 1, LLMTurns: 1, ToolCalls: 1, InputTokens: 1200, CachedInputTokens: 20, CacheWriteTokens: 30, OutputTokens: 400,
	}
	messages := []Message{
		{Role: llm.RoleUser, TextContent: "Hello **world**", Parts: []llm.Part{{Type: llm.PartText, Text: "Hello **world**"}}, CreatedAt: now},
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: "```go\nfmt.Println(\"hi\")\n```"}, {Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call-1", Name: "shell", Arguments: []byte(`{"command":"go test"}`)}}}, CreatedAt: now.Add(time.Second), DurationMs: 1250},
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "call-1", Name: "shell", Content: "ok"}}}, CreatedAt: now.Add(2 * time.Second)},
	}

	html, err := ExportToHTML(sess, messages, ExportOptions{})
	if err != nil {
		t.Fatalf("ExportToHTML: %v", err)
	}
	for _, want := range []string{"<!doctype html>", "HTML export", "OpenAI", "gpt-test", "developer", "1,200", "Hello <strong>world</strong>", "language-go", "fmt.Println", "shell", "go test", "ok", "1.25s", "data-action=\"theme\"", "prefers-color-scheme:dark", "@media print", "<details"} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	if strings.Index(html, "Hello") > strings.Index(html, "fmt.Println") {
		t.Error("message role ordering was not preserved")
	}
}

func TestExportToHTMLUsesCompactHeaderControlsAndModelLabel(t *testing.T) {
	sess := &Session{
		ID: "s", Provider: "ChatGPT (gpt-5.6-sol, effort=medium)", Model: "gpt-5.6-sol-medium", CreatedAt: time.Now(),
	}
	messages := []Message{{
		Role:  llm.RoleAssistant,
		Parts: []llm.Part{{Type: llm.PartText, ReasoningContent: "summary", ReasoningKind: llm.ReasoningKindSummary}},
	}}
	html, err := ExportToHTML(sess, messages, ExportOptions{IncludeReasoningSummaries: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`data-action="theme"`, ">Dark mode</span>", `data-action="details"`, ">Expand details</span>", "ChatGPT (gpt-5.6-sol, effort=medium)", "min-height:44px", "prefers-color-scheme: dark", `name="theme-color"`} {
		if !strings.Contains(html, want) {
			t.Errorf("compact header missing %q", want)
		}
	}
	for _, unwanted := range []string{`data-action="expand"`, `data-action="collapse"`, " / gpt-5.6-sol-medium"} {
		if strings.Contains(html, unwanted) {
			t.Errorf("compact header contains %q", unwanted)
		}
	}
}

func TestExportToHTMLGroupsConsecutiveTools(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 34, 0, 0, time.UTC)
	sess := &Session{ID: "s", Provider: "p", Model: "m", CreatedAt: now}
	messages := []Message{
		{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "run tools"}}},
		{Role: llm.RoleAssistant, Parts: []llm.Part{
			{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call-1", Name: "grep"}},
			{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call-2", Name: "shell"}},
			{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call-3", Name: "read_file"}},
		}},
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "call-1", Content: "one"}}}},
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "call-2", Content: "two"}}}},
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "call-3", Content: "three"}}}},
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: "done"}}},
	}

	html, err := ExportToHTML(sess, messages, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(html, `class="message tool"`); got != 1 {
		t.Fatalf("tool message groups = %d, want 1", got)
	}
	for _, want := range []string{`class="tool-group"`, "3 tool calls completed", "grep", "shell", "read_file"} {
		if !strings.Contains(html, want) {
			t.Errorf("grouped HTML missing %q", want)
		}
	}
}

func TestExportToHTMLFooterLinksToTermLLM(t *testing.T) {
	sess := &Session{ID: "s", Provider: "p", Model: "m", CreatedAt: time.Now()}
	html, err := ExportToHTML(sess, nil, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, `<a href="https://term-llm.com/">Exported from term-llm</a>`) {
		t.Error("footer does not link its term-llm attribution")
	}
	if strings.Contains(html, "requires no term-llm server") {
		t.Error("footer contains unnecessary static transcript claim")
	}
}

func TestExportToHTMLVisibilityReasoningToolsAndMedia(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("png bytes"))
	sess := &Session{ID: "s", Provider: "p", Model: "m", CreatedAt: time.Now()}
	messages := []Message{
		{Role: llm.RoleSystem, TextContent: "secret system", Parts: []llm.Part{{Type: llm.PartText, Text: "secret system"}}},
		{Role: llm.RoleAssistant, Parts: []llm.Part{
			{Type: llm.PartText, ReasoningContent: "safe summary", ReasoningKind: llm.ReasoningKindSummary},
			{Type: llm.PartText, ReasoningContent: "raw thoughts", ReasoningKind: llm.ReasoningKindRaw},
			{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "orphan", Name: "bad-json", Arguments: []byte(`{"oops"`)}},
		}},
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "missing", Name: "failed", Content: "boom", IsError: true, Diffs: []llm.DiffData{{File: "main.go", Old: "old", New: "new"}}, ContentParts: []llm.ToolContentPart{{Type: llm.ToolContentPartImageData, ImageData: &llm.ToolImageData{MediaType: "image/png", Base64: png}}}}}}},
		{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartFile, FileData: &llm.ToolFileData{Filename: "notes.txt", MediaType: "text/plain", SizeBytes: 12}, FilePath: "/private/do-not-read"}}},
	}

	without, err := ExportToHTML(sess, messages, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"secret system", "safe summary", "raw thoughts", "/private/do-not-read"} {
		if strings.Contains(without, hidden) {
			t.Errorf("default HTML leaked %q", hidden)
		}
	}

	with, err := ExportToHTML(sess, messages, ExportOptions{IncludeSystem: true, IncludeReasoningSummaries: true, IncludeRawReasoning: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"secret system", "safe summary", "raw thoughts", "Raw reasoning", "bad-json", "orphan", "failed", "boom", "error", "main.go", "old", "new", "data:image/png;base64,", "notes.txt", "text/plain", "12 bytes"} {
		if !strings.Contains(with, want) {
			t.Errorf("opt-in HTML missing %q", want)
		}
	}
}

func TestExportToHTMLPreventsTranscriptXSS(t *testing.T) {
	sess := &Session{ID: "s", Name: `<script>alert("title")</script>`, Provider: `<img src=x onerror=alert(1)>`, Model: "m", CreatedAt: time.Now()}
	messages := []Message{
		{
			Role: llm.RoleUser,
			Parts: []llm.Part{
				{Type: llm.PartText, Text: `<script>alert("message")</script><img src=x onerror=alert(2)> [bad](javascript:alert(3))`},
				{Type: llm.PartImage, ImageData: &llm.ToolImageData{MediaType: `image/svg+xml" onload="alert(4)`, Base64: "PHN2Zz4="}},
			},
		},
		{
			Role: llm.RoleAssistant,
			Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{
				ID: "x", Name: `</summary><script>alert(5)</script>`, Arguments: []byte(`</script><script>alert(6)</script>`),
			}}},
		},
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "x", Content: `<img onerror=alert(7)>`}}}},
		{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartFile, FileData: &llm.ToolFileData{Filename: `"><img onerror=alert(8)>`, MediaType: "text/plain"}}}},
	}

	html, err := ExportToHTML(sess, messages, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"alert(\"title\")</script>", "<img src=x onerror=", "javascript:alert", `image/svg+xml" onload=`, "alert(5)</script>", "alert(6)</script>"} {
		if strings.Contains(html, bad) {
			t.Errorf("HTML contains executable transcript fragment %q", bad)
		}
	}
	if strings.Count(strings.ToLower(html), "<script") != 1 {
		t.Fatalf("expected only the fixed exporter script, got %d script tags", strings.Count(strings.ToLower(html), "<script"))
	}
	if strings.Contains(html, "data:image/svg") {
		t.Error("unsafe image MIME type was inlined")
	}
}

func TestVisibleExportMessages(t *testing.T) {
	messages := []Message{
		{Sequence: 1, TextContent: "before"},
		{Sequence: 2, TextContent: "[Context Compaction] summary"},
		{Sequence: 3, TextContent: "duplicate", CompactionTail: true},
		*NewMessage("s", llm.GoalSteeringText("internal continuation"), 4),
		{Sequence: 5, TextContent: "after"},
	}
	got := VisibleExportMessages(messages)
	if len(got) != 3 || got[0].Sequence != 1 || got[1].Sequence != 2 || got[2].Sequence != 5 {
		t.Fatalf("VisibleExportMessages = %#v", got)
	}
	if len(messages) != 5 {
		t.Fatal("input slice was mutated")
	}
}
