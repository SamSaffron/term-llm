package inspect

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/session"
)

func inspectMessage(id int64, role core.Role, parts ...core.Part) session.Message {
	return session.Message{ID: id, Role: role, Sequence: int(id), CreatedAt: time.Unix(1760000000, 0), Parts: parts}
}

func TestInspectProjectionKinds(t *testing.T) {
	tests := []struct {
		name                      string
		role                      core.Role
		parts                     []core.Part
		kind, text, title, format string
	}{
		{"system", core.RoleSystem, []core.Part{{Type: core.PartText, Text: "system instructions"}}, "system", "system instructions", "system", "text"},
		{"developer", core.RoleDeveloper, []core.Part{{Type: core.PartText, Text: "developer instructions"}}, "developer", "developer instructions", "developer", "text"},
		{"goal", core.RoleUser, []core.Part{{Type: core.PartGoalSteering}, {Type: core.PartText, Text: "hidden goal"}}, "goal_steering", "hidden goal", "goal_steering", "text"},
		{"job notification", core.RoleUser, []core.Part{{Type: core.PartJobNotification}, {Type: core.PartText, Text: "queued job done"}}, "job_notification", "queued job done", "job_notification", "text"},
		{"path note", core.RoleDeveloper, []core.Part{{Type: core.PartPathNote}, {Type: core.PartText, Text: "path context"}}, "path_note", "path context", "path_note", "text"},
		{"user", core.RoleUser, []core.Part{{Type: core.PartText, Text: "question"}}, "user", "question", "user", "text"},
		{"assistant", core.RoleAssistant, []core.Part{{Type: core.PartText, Text: "answer"}}, "assistant_text", "answer", "assistant_text", "text"},
		{"event", core.RoleEvent, []core.Part{{Type: core.PartText, Text: "model changed"}}, "event", "model changed", "event", "text"},
		{"image", core.RoleUser, []core.Part{{Type: core.PartImage, ImageData: &core.ToolImageData{MediaType: "image/png", Base64: base64.StdEncoding.EncodeToString([]byte("image-bytes"))}}}, "image", "[Image: image/png, 11 bytes]", "image/png", "text"},
		{"file", core.RoleUser, []core.Part{{Type: core.PartFile, FileData: &core.ToolFileData{Filename: "file.pdf", MediaType: "application/pdf", SizeBytes: 123, Base64: "PRIVATE"}}}, "file", "[File: file.pdf, application/pdf, 123 bytes]", "file.pdf", "text"},
		{"file text", core.RoleUser, []core.Part{{Type: core.PartFile, Text: "readable file"}}, "file", "readable file", "file", "text"},
		{"tool args", core.RoleAssistant, []core.Part{{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "call1", Name: "shell", Arguments: json.RawMessage(`{"z":2,"a":"ok"}`)}}}, "tool_call", "{\n  \"a\": \"ok\",\n  \"z\": 2\n}", "shell", "json"},
		{"tool result", core.RoleTool, []core.Part{{Type: core.PartToolResult, ToolResult: &core.ToolResult{ID: "call1", Name: "shell", Content: strings.Repeat("line\n", 100), IsError: true}}}, "tool_result", strings.Repeat("line\n", 100), "shell", "text"},
		{"structured result", core.RoleTool, []core.Part{{Type: core.PartToolResult, ToolResult: &core.ToolResult{Name: "shell", ContentParts: []core.ToolContentPart{{Type: core.ToolContentPartText, Text: "first"}, {Type: core.ToolContentPartImageData, ImageData: &core.ToolImageData{Base64: "PRIVATE"}}, {Type: core.ToolContentPartText, Text: " second"}}}}}, "tool_result", "first second\n[1 image(s) attached]", "shell", "text"},
		{"attachments", core.RoleTool, []core.Part{{Type: core.PartToolResult, ToolResult: &core.ToolResult{Name: "show_media", Content: "done", Images: []string{"secret-path"}, Media: []core.MediaArtifact{{}}, ContentParts: []core.ToolContentPart{{ImageData: &core.ToolImageData{Base64: "PRIVATE"}}}}}}, "tool_result", "done\n[3 image(s) attached]", "show_media", "text"},
		{"skill", core.RoleDeveloper, []core.Part{{Type: core.PartSkillActivation, SkillActivation: &core.SkillActivationProvenance{Name: "testing", Source: "local", AllowedToolsPresent: true}}}, "skill_activation", "Skill activation: testing\nSource: local\nAllowed tools: (none)", "testing", "text"},
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
		part        core.Part
		kind        core.ReasoningKind
		title, text string
		encrypted   int
	}{
		{"legacy summary", core.Part{ReasoningContent: "**Plan**\n\nDo this"}, core.ReasoningKindSummary, "Plan", "Do this", 0},
		{"summary parts", core.Part{ReasoningKind: core.ReasoningKindSummary, ReasoningContent: "old", ReasoningSummaryParts: []string{" **Plan** ", " first ", " ", " second "}}, core.ReasoningKindSummary, "Plan", "first\n\nsecond", 0},
		{"raw", core.Part{ReasoningKind: core.ReasoningKindRaw, ReasoningContent: " raw thoughts ", ReasoningSummaryTitle: "Explicit"}, core.ReasoningKindRaw, "Explicit", "raw thoughts", 0},
		{"unknown", core.Part{ReasoningKind: core.ReasoningKindUnknown, ReasoningContent: "unknown text"}, core.ReasoningKindUnknown, "Thinking...", "unknown text", 0},
		{"encrypted", core.Part{ReasoningKind: core.ReasoningKindEncrypted, ReasoningContent: "CIPHERTEXT", ReasoningSummaryParts: []string{"HIDDEN"}, ReasoningEncryptedContent: "SECRET"}, core.ReasoningKindEncrypted, "Encrypted reasoning", "", 16},
		{"blank summary and encrypted", core.Part{ReasoningSummaryParts: []string{" ", "\n"}, ReasoningEncryptedContent: "SECRET"}, core.ReasoningKindEncrypted, "Encrypted reasoning", "", 6},
		{"only encrypted", core.Part{ReasoningEncryptedContent: "SECRET"}, core.ReasoningKindEncrypted, "Encrypted reasoning", "", 6},
		{"summary and replay", core.Part{ReasoningContent: "safe", ReasoningEncryptedContent: "SECRET"}, core.ReasoningKindSummary, "Thinking...", "safe", 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.part.Type, tt.part.Text = core.PartText, "answer"
			d := Project([]session.Message{inspectMessage(1, core.RoleAssistant, tt.part)}, Options{})
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
	excluded := []core.PartType{core.PartProviderReplay, core.PartDiscoveryCall, core.PartDiscoveryOutput, core.PartToolActivity, core.PartConversationStart, core.PartPlatformContext, core.PartDisplayText, core.PartAgentMention, core.PartDiffComment, core.PartGoalSteering, core.PartJobNotification, core.PartPathNote}
	for _, typ := range excluded {
		t.Run(string(typ), func(t *testing.T) {
			msg := inspectMessage(3, core.RoleAssistant, core.Part{Type: typ, Text: "SECRET", ReasoningContent: "SECRET", ReasoningEncryptedContent: "SECRET"})
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
	if !engine.IsInternalCompactionSummaryText(summary) {
		t.Fatal("invalid fixture summary")
	}
	messages := []session.Message{
		inspectMessage(1, core.RoleSystem, core.Part{Type: core.PartText, Text: "system"}),
		inspectMessage(2, core.RoleUser, core.Part{Type: core.PartText, Text: summary}),
		inspectMessage(3, core.RoleAssistant, core.Part{Type: core.PartText, Text: "tail"}),
		inspectMessage(4, core.RoleUser, core.Part{Type: core.PartText, Text: "active"}),
		inspectMessage(5, core.RoleUser, core.Part{Type: core.PartText, Text: summary}),
		inspectMessage(6, core.RoleAssistant, core.Part{Type: core.PartText, Text: "tail2"}),
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
	msg := inspectMessage(80, core.RoleAssistant, core.Part{Type: core.PartText, Text: "answer", CreatedAt: 123})
	first := Project([]session.Message{msg}, Options{}).Blocks[0]
	second := Project([]session.Message{inspectMessage(79, core.RoleUser), msg}, Options{}).Blocks[0]
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
	messages := []session.Message{inspectMessage(1, core.RoleSystem, core.Part{Type: core.PartText, Text: "persisted"})}
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
			tt.opts.Tools = []core.ToolSpec{{Name: "shell", Description: "Execute", Schema: map[string]any{"type": "object"}}}
			tt.opts.ToolDiscovery = &core.ToolDiscoveryDiagnostics{ResolvedMode: "on"}
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
	messages := []session.Message{inspectMessage(1, core.RoleTool, core.Part{Type: core.PartToolResult, ToolResult: &core.ToolResult{ID: "call1", Name: "shell", Content: "error details", IsError: true}})}
	d := Project(messages, Options{Session: meta})
	if d.Header.Provider != meta.Provider || d.Header.Model != meta.Model || d.Header.ReasoningEffort != meta.ReasoningEffort || d.Header.Agent != meta.Agent || d.Header.CompactionSeq != 20 || d.Header.CompactionCount != 2 {
		t.Fatalf("header = %+v", d.Header)
	}
	if got := d.Blocks[0].Tool; got == nil || got.CallID != "call1" || got.Name != "shell" || !got.IsError {
		t.Fatalf("tool = %+v", got)
	}
}

func TestInspectInvalidRuntimeSchema(t *testing.T) {
	d := Project(nil, Options{Runtime: RuntimeOptions{Loaded: true, Tools: []core.ToolSpec{{Name: "malformed", Schema: map[string]any{"invalid": make(chan int)}}}}})
	if len(d.Header.Runtime.Tools) != 0 || len(d.ResolveBodies([]string{"runtime.tool.0"}).Missing) != 1 {
		t.Fatal("malformed runtime schema advertised a JSON body")
	}
}
