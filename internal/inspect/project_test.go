package inspect

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func inspectMessage(id int64, role llm.Role, parts ...llm.Part) session.Message {
	return session.Message{ID: id, Role: role, Sequence: int(id), CreatedAt: time.Unix(1760000000, 0), Parts: parts}
}

func TestInspectProjectionKinds(t *testing.T) {
	tests := []struct {
		name                      string
		role                      llm.Role
		parts                     []llm.Part
		kind, text, title, format string
	}{
		{"system", llm.RoleSystem, []llm.Part{{Type: llm.PartText, Text: "system instructions"}}, "system", "system instructions", "system", "text"},
		{"developer", llm.RoleDeveloper, []llm.Part{{Type: llm.PartText, Text: "developer instructions"}}, "developer", "developer instructions", "developer", "text"},
		{"goal", llm.RoleUser, []llm.Part{{Type: llm.PartGoalSteering}, {Type: llm.PartText, Text: "hidden goal"}}, "goal_steering", "hidden goal", "goal_steering", "text"},
		{"path note", llm.RoleDeveloper, []llm.Part{{Type: llm.PartPathNote}, {Type: llm.PartText, Text: "path context"}}, "path_note", "path context", "path_note", "text"},
		{"user", llm.RoleUser, []llm.Part{{Type: llm.PartText, Text: "question"}}, "user", "question", "user", "text"},
		{"assistant", llm.RoleAssistant, []llm.Part{{Type: llm.PartText, Text: "answer"}}, "assistant_text", "answer", "assistant_text", "text"},
		{"event", llm.RoleEvent, []llm.Part{{Type: llm.PartText, Text: "model changed"}}, "event", "model changed", "event", "text"},
		{"image", llm.RoleUser, []llm.Part{{Type: llm.PartImage, ImageData: &llm.ToolImageData{MediaType: "image/png", Base64: base64.StdEncoding.EncodeToString([]byte("image-bytes"))}}}, "image", "[Image: image/png, 11 bytes]", "image/png", "text"},
		{"file", llm.RoleUser, []llm.Part{{Type: llm.PartFile, FileData: &llm.ToolFileData{Filename: "file.pdf", MediaType: "application/pdf", SizeBytes: 123, Base64: "PRIVATE"}}}, "file", "[File: file.pdf, application/pdf, 123 bytes]", "file.pdf", "text"},
		{"file text", llm.RoleUser, []llm.Part{{Type: llm.PartFile, Text: "readable file"}}, "file", "readable file", "file", "text"},
		{"tool args", llm.RoleAssistant, []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call1", Name: "shell", Arguments: json.RawMessage(`{"z":2,"a":"ok"}`)}}}, "tool_call", "{\n  \"a\": \"ok\",\n  \"z\": 2\n}", "shell", "json"},
		{"tool result", llm.RoleTool, []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "call1", Name: "shell", Content: strings.Repeat("line\n", 100), IsError: true}}}, "tool_result", strings.Repeat("line\n", 100), "shell", "text"},
		{"structured result", llm.RoleTool, []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{Name: "shell", ContentParts: []llm.ToolContentPart{{Type: llm.ToolContentPartText, Text: "first"}, {Type: llm.ToolContentPartImageData, ImageData: &llm.ToolImageData{Base64: "PRIVATE"}}, {Type: llm.ToolContentPartText, Text: " second"}}}}}, "tool_result", "first second\n[1 image(s) attached]", "shell", "text"},
		{"attachments", llm.RoleTool, []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{Name: "show_media", Content: "done", Images: []string{"secret-path"}, Media: []llm.MediaArtifact{{}}, ContentParts: []llm.ToolContentPart{{ImageData: &llm.ToolImageData{Base64: "PRIVATE"}}}}}}, "tool_result", "done\n[3 image(s) attached]", "show_media", "text"},
		{"skill", llm.RoleDeveloper, []llm.Part{{Type: llm.PartSkillActivation, SkillActivation: &llm.SkillActivationProvenance{Name: "testing", Source: "local", AllowedToolsPresent: true}}}, "skill_activation", "Skill activation: testing\nSource: local\nAllowed tools: (none)", "testing", "text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Project([]session.Message{inspectMessage(901, tt.role, tt.parts...)}, Options{})
			if len(d.Blocks) != 1 {
				t.Fatalf("blocks = %+v", d.Blocks)
			}
			block := d.Blocks[0]
			wantIndex := len(tt.parts) - 1
			if block.ID != fmt.Sprintf("m901.p%d.%s", wantIndex, tt.kind) || block.Kind != tt.kind || block.Role != tt.role || block.Title != tt.title {
				t.Fatalf("block = %+v", block)
			}
			if block.MessageID != "901" || block.Sequence != 901 || block.CreatedAt != 1760000000000 || block.PartIndex != wantIndex {
				t.Fatalf("identity = %+v", block)
			}
			got := d.ResolveBodies([]string{block.ID})
			if len(got.Bodies) != 1 || got.Bodies[0].Text != tt.text || got.Bodies[0].Format != tt.format {
				t.Fatalf("body = %+v", got)
			}
			if block.TotalBytes != len(tt.text) || block.TotalLines != strings.Count(tt.text, "\n")+1 || !block.HasBody {
				t.Fatalf("sizes = %+v", block)
			}
		})
	}
}

func TestInspectReasoning(t *testing.T) {
	tests := []struct {
		name        string
		part        llm.Part
		kind        llm.ReasoningKind
		title, text string
		encrypted   int
	}{
		{"legacy summary", llm.Part{ReasoningContent: "**Plan**\n\nDo this"}, llm.ReasoningKindSummary, "Plan", "Do this", 0},
		{"summary parts", llm.Part{ReasoningKind: llm.ReasoningKindSummary, ReasoningContent: "old", ReasoningSummaryParts: []string{" **Plan** ", " first ", " ", " second "}}, llm.ReasoningKindSummary, "Plan", "first\n\nsecond", 0},
		{"raw", llm.Part{ReasoningKind: llm.ReasoningKindRaw, ReasoningContent: " raw thoughts ", ReasoningSummaryTitle: "Explicit"}, llm.ReasoningKindRaw, "Explicit", "raw thoughts", 0},
		{"unknown", llm.Part{ReasoningKind: llm.ReasoningKindUnknown, ReasoningContent: "unknown text"}, llm.ReasoningKindUnknown, "Thinking...", "unknown text", 0},
		{"encrypted", llm.Part{ReasoningKind: llm.ReasoningKindEncrypted, ReasoningContent: "CIPHERTEXT", ReasoningSummaryParts: []string{"HIDDEN"}, ReasoningEncryptedContent: "SECRET"}, llm.ReasoningKindEncrypted, "Encrypted reasoning", "", 16},
		{"blank summary and encrypted", llm.Part{ReasoningSummaryParts: []string{" ", "\n"}, ReasoningEncryptedContent: "SECRET"}, llm.ReasoningKindEncrypted, "Encrypted reasoning", "", 6},
		{"only encrypted", llm.Part{ReasoningEncryptedContent: "SECRET"}, llm.ReasoningKindEncrypted, "Encrypted reasoning", "", 6},
		{"summary and replay", llm.Part{ReasoningContent: "safe", ReasoningEncryptedContent: "SECRET"}, llm.ReasoningKindSummary, "Thinking...", "safe", 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.part.Type, tt.part.Text = llm.PartText, "answer"
			d := Project([]session.Message{inspectMessage(1, llm.RoleAssistant, tt.part)}, Options{})
			if len(d.Blocks) != 2 || d.Blocks[0].ID != "m1.p0.reasoning" || d.Blocks[1].ID != "m1.p0.assistant_text" {
				t.Fatalf("blocks = %+v", d.Blocks)
			}
			block := d.Blocks[0]
			if block.ReasoningKind != tt.kind || block.Title != tt.title || block.EncryptedBytes != tt.encrypted || block.HasBody != (tt.text != "") {
				t.Fatalf("reasoning = %+v", block)
			}
			response := d.ResolveBodies([]string{block.ID})
			if tt.text != "" && (len(response.Bodies) != 1 || response.Bodies[0].Text != tt.text) {
				t.Fatalf("response = %+v", response)
			}
			indexJSON, _ := json.Marshal(d)
			bodyJSON, _ := json.Marshal(response)
			for _, text := range []string{string(indexJSON), string(bodyJSON)} {
				for _, secret := range []string{"SECRET", "CIPHERTEXT", "HIDDEN"} {
					if strings.Contains(text, secret) {
						t.Fatalf("leaked %s: %s", secret, text)
					}
				}
			}
		})
	}
}

func TestInspectExclusions(t *testing.T) {
	excluded := []llm.PartType{llm.PartProviderReplay, llm.PartDiscoveryCall, llm.PartDiscoveryOutput, llm.PartToolActivity, llm.PartConversationStart, llm.PartPlatformContext, llm.PartDisplayText, llm.PartAgentMention, llm.PartDiffComment, llm.PartGoalSteering, llm.PartPathNote}
	for _, typ := range excluded {
		t.Run(string(typ), func(t *testing.T) {
			msg := inspectMessage(3, llm.RoleAssistant, llm.Part{Type: typ, Text: "SECRET", ReasoningContent: "SECRET", ReasoningEncryptedContent: "SECRET"})
			msg.TextContent = "SECRET fallback"
			d := Project([]session.Message{msg}, Options{})
			if len(d.Blocks) != 0 {
				t.Fatalf("excluded %s = %+v", typ, d.Blocks)
			}
			indexJSON, _ := json.Marshal(d)
			response := d.ResolveBodies([]string{"m3.p0.assistant_text", "m3.p0.reasoning"})
			bodyJSON, _ := json.Marshal(response)
			if strings.Contains(string(indexJSON)+string(bodyJSON), "SECRET") {
				t.Fatal("excluded content leaked")
			}
		})
	}
}

func TestInspectCompaction(t *testing.T) {
	summary := "[Context Compaction]\nsummary"
	if !llm.IsInternalCompactionSummaryText(summary) {
		t.Fatal("invalid fixture summary")
	}
	messages := []session.Message{
		inspectMessage(1, llm.RoleSystem, llm.Part{Type: llm.PartText, Text: "system"}),
		inspectMessage(2, llm.RoleUser, llm.Part{Type: llm.PartText, Text: summary}),
		inspectMessage(3, llm.RoleAssistant, llm.Part{Type: llm.PartText, Text: "tail"}),
		inspectMessage(4, llm.RoleUser, llm.Part{Type: llm.PartText, Text: "active"}),
		inspectMessage(5, llm.RoleUser, llm.Part{Type: llm.PartText, Text: summary}),
		inspectMessage(6, llm.RoleAssistant, llm.Part{Type: llm.PartText, Text: "tail2"}),
	}
	messages[2].CompactionTail = true
	messages[5].CompactionTail = true
	for _, seq := range []int{2, 4, 5} {
		t.Run(fmt.Sprint(seq), func(t *testing.T) {
			d := Project(messages, Options{Session: &session.Session{CompactionSeq: seq, CompactionCount: 2}})
			summaries, markers := 0, 0
			for _, block := range d.Blocks {
				switch block.Kind {
				case "compaction_summary":
					summaries++
					if block.Compaction == nil || block.Compaction.Ordinal != summaries || block.Compaction.TailCount != 1 || block.Compaction.ActiveBoundary != (block.Sequence == seq) {
						t.Fatalf("summary = %+v", block)
					}
				case "compaction_boundary":
					markers++
					if block.Sequence != seq || block.HasBody || block.ID != "m4.compaction_boundary" {
						t.Fatalf("marker = %+v", block)
					}
				}
				if block.Sequence == 3 || block.Sequence == 6 {
					want := fmt.Sprintf("m%d.p0.compaction_summary", block.Sequence-1)
					if !block.CompactionTail || block.CompactionSummaryID != want {
						t.Fatalf("tail = %+v", block)
					}
				}
			}
			wantMarkers := 0
			if seq == 4 {
				wantMarkers = 1
			}
			if summaries != 2 || markers != wantMarkers {
				t.Fatalf("summaries %d markers %d", summaries, markers)
			}
		})
	}
}

func TestInspectStableIDsAndLegacyFallback(t *testing.T) {
	msg := inspectMessage(80, llm.RoleAssistant, llm.Part{Type: llm.PartText, Text: "answer", CreatedAt: 123})
	first := Project([]session.Message{msg}, Options{}).Blocks[0]
	second := Project([]session.Message{inspectMessage(79, llm.RoleUser), msg}, Options{}).Blocks[0]
	if first.ID != second.ID || first.CreatedAt != 123 {
		t.Fatalf("unstable IDs: %+v / %+v", first, second)
	}
	msg.ID = 0
	runtime := Project([]session.Message{msg}, Options{Scope: "runtime_history"})
	if runtime.Blocks[0].ID != "i0.p0.assistant_text" {
		t.Fatal(runtime.Blocks)
	}
	msg.Parts = nil
	msg.TextContent = "legacy text"
	legacy := Project([]session.Message{msg}, Options{})
	if len(legacy.Blocks) != 1 || legacy.Blocks[0].Preview != "legacy text" {
		t.Fatal(legacy.Blocks)
	}
	empty := Project(nil, Options{})
	data, _ := json.Marshal(empty)
	if !strings.Contains(string(data), `"blocks":[]`) {
		t.Fatal(string(data))
	}
}

func TestInspectRuntime(t *testing.T) {
	messages := []session.Message{inspectMessage(1, llm.RoleSystem, llm.Part{Type: llm.PartText, Text: "persisted"})}
	for _, tt := range []struct {
		name    string
		opts    RuntimeOptions
		matches *bool
	}{
		{name: "cold"},
		{name: "busy", opts: RuntimeOptions{Loaded: true, UnavailableReason: "runtime_busy"}},
		{name: "matching", opts: RuntimeOptions{Loaded: true, SystemPrompt: "persisted"}, matches: boolPointer(true)},
		{name: "different", opts: RuntimeOptions{Loaded: true, SystemPrompt: "new"}, matches: boolPointer(false)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.Tools = []llm.ToolSpec{{Name: "shell", Description: "Execute", Schema: map[string]any{"type": "object"}}}
			tt.opts.ToolDiscovery = &llm.ToolDiscoveryDiagnostics{ResolvedMode: "on"}
			d := Project(messages, Options{Runtime: tt.opts})
			rt := d.Header.Runtime
			if !reflect.DeepEqual(rt.SystemPromptMatchesPersisted, tt.matches) {
				t.Fatalf("runtime = %+v", rt)
			}
			unavailable := !tt.opts.Loaded || tt.opts.UnavailableReason != ""
			body := d.ResolveBodies([]string{"runtime.system_prompt", "runtime.tool.0"})
			if unavailable {
				if len(rt.Tools) != 0 || rt.ToolDiscovery != nil || rt.SystemPromptBlockID != "" || len(body.Missing) != 2 {
					t.Fatalf("unavailable runtime = %+v / %+v", rt, body)
				}
				return
			}
			if len(rt.Tools) != 1 || rt.ToolDiscovery == nil || len(body.Bodies) != 2 {
				t.Fatalf("runtime = %+v / %+v", rt, body)
			}
			schema := body.Bodies[1]
			var decoded struct {
				Name, Description string
				Schema            map[string]any
			}
			if schema.Format != "json" || json.Unmarshal([]byte(schema.Text), &decoded) != nil || decoded.Name != "shell" || decoded.Schema["type"] != "object" {
				t.Fatalf("schema = %+v", schema)
			}
		})
	}
}

func boolPointer(b bool) *bool { return &b }

func TestInspectHeaderAndToolMetadata(t *testing.T) {
	meta := &session.Session{Provider: "provider", ProviderKey: "provider-key", Model: "model", ReasoningEffort: "high", Agent: "developer", CompactionSeq: 20, CompactionCount: 2}
	messages := []session.Message{inspectMessage(1, llm.RoleTool, llm.Part{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "call1", Name: "shell", Content: "error details", IsError: true}})}
	d := Project(messages, Options{Session: meta})
	if d.Header.Provider != meta.Provider || d.Header.Model != meta.Model || d.Header.ReasoningEffort != meta.ReasoningEffort || d.Header.Agent != meta.Agent || d.Header.CompactionSeq != 20 || d.Header.CompactionCount != 2 {
		t.Fatalf("header = %+v", d.Header)
	}
	if got := d.Blocks[0].Tool; got == nil || got.CallID != "call1" || got.Name != "shell" || !got.IsError {
		t.Fatalf("tool = %+v", got)
	}
}

func TestInspectInvalidRuntimeSchema(t *testing.T) {
	d := Project(nil, Options{Runtime: RuntimeOptions{Loaded: true, Tools: []llm.ToolSpec{{Name: "malformed", Schema: map[string]any{"invalid": make(chan int)}}}}})
	if len(d.Header.Runtime.Tools) != 0 || len(d.ResolveBodies([]string{"runtime.tool.0"}).Missing) != 1 {
		t.Fatal("malformed runtime schema advertised a JSON body")
	}
}
