package inspect

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/session"
)

// Project builds an explicit allowlist projection. In particular it never
// marshals llm.Part or session.Message, whose replay fields contain secrets.
func Project(messages []session.Message, opts Options) *Document {
	d := &Document{SchemaVersion: 1, SessionID: opts.SessionID, TranscriptRev: opts.TranscriptRev, Scope: opts.Scope, Blocks: []Block{}, bodies: make(map[string]Body)}
	if d.Scope == "" {
		d.Scope = "persisted_history"
	}
	d.Header = headerForSession(opts.Session)
	d.setInitialModel(messages)
	d.addRuntime(messages, opts.Runtime)
	boundary := -1
	if session.HasCompactionBoundary(opts.Session) {
		boundary = CompactionBoundaryIndex(messages, true, opts.Session.CompactionSeq, -1)
	}
	ordinal, tailEnd, summaryID := 0, 0, ""
	for i, msg := range messages {
		prefix := messagePrefix(msg.ID, i)
		var compaction *Compaction
		if IsCompactionSummary(msg) {
			ordinal++
			tailEnd = CompactionTailEnd(messages, i)
			summaryID = fmt.Sprintf("%s.p%d.compaction_summary", prefix, summaryPartIndex(msg))
			compaction = &Compaction{Ordinal: ordinal, ActiveBoundary: boundary == i, TailCount: tailEnd - i - 1}
		} else if i == boundary {
			marker := blockForMessage(msg, prefix, -1, "compaction_boundary")
			marker.Compaction = &Compaction{ActiveBoundary: true}
			d.addBlock(marker, "", "text")
		}
		start := len(d.Blocks)
		d.addMessage(msg, prefix, compaction, i < tailEnd && compaction == nil, summaryID, opts.ChildSessionIDs)
		if i > 0 {
			d.markHandoverAcknowledgement(messages[i-1], msg, start)
		}
	}
	return d
}

func headerForSession(meta *session.Session) Header {
	h := Header{CompactionSeq: -1}
	if meta == nil {
		return h
	}
	h.Provider, h.Model = meta.Provider, meta.Model
	if h.Provider == "" {
		h.Provider = meta.ProviderKey
	}
	h.ReasoningEffort, h.Agent = meta.ReasoningEffort, meta.Agent
	h.CompactionSeq, h.CompactionCount = meta.CompactionSeq, meta.CompactionCount
	provider := meta.ProviderKey
	if provider == "" {
		provider = meta.Provider
	}
	h.CurrentModel = modelInfo(provider, meta.Model, meta.ReasoningEffort, meta.ReasoningMode, meta.Agent)
	h.InitialModel = h.CurrentModel
	return h
}

func messagePrefix(id int64, index int) string {
	if id != 0 {
		return "m" + strconv.FormatInt(id, 10)
	}
	return "i" + strconv.Itoa(index)
}

func summaryPartIndex(msg session.Message) int {
	for i, part := range msg.Parts {
		if part.Type == core.PartText && engine.IsInternalCompactionSummaryText(part.Text) {
			return i
		}
	}
	return 0
}

func blockForMessage(msg session.Message, prefix string, partIndex int, kind string) Block {
	id := prefix + "." + kind
	if partIndex >= 0 {
		id = fmt.Sprintf("%s.p%d.%s", prefix, partIndex, kind)
	}
	created := int64(0)
	if !msg.CreatedAt.IsZero() {
		created = msg.CreatedAt.UnixMilli()
	}
	if partIndex >= 0 && partIndex < len(msg.Parts) && msg.Parts[partIndex].CreatedAt != 0 {
		created = msg.Parts[partIndex].CreatedAt
	}
	return Block{ID: id, Kind: kind, Role: msg.Role, MessageID: strconv.FormatInt(msg.ID, 10), Sequence: msg.Sequence, PartIndex: partIndex, CreatedAt: created, Title: kind}
}

func textKind(msg session.Message) string {
	switch msg.Role {
	case core.RoleSystem:
		return "system"
	case core.RoleDeveloper:
		for _, part := range msg.Parts {
			if part.Type == core.PartPathNote {
				return "path_note"
			}
		}
		return "developer"
	case core.RoleUser:
		for _, part := range msg.Parts {
			if part.Type == core.PartGoalSteering {
				return "goal_steering"
			}
			if part.Type == core.PartJobNotification {
				return "job_notification"
			}
		}
		return "user"
	case core.RoleAssistant:
		return "assistant_text"
	case core.RoleEvent:
		return "event"
	default:
		return ""
	}
}

func (d *Document) addMessage(msg session.Message, prefix string, compaction *Compaction, tail bool, summaryID string, children func(string, string) []string) {
	parts := messageParts(msg)
	for i, part := range parts {
		if part.Type == core.PartText {
			d.addReasoning(msg, prefix, i, part, tail, summaryID)
		}
		block, text, format := projectPart(msg, prefix, i, part, compaction)
		if block.Kind == "" {
			continue
		}
		block.CompactionTail = msg.CompactionTail || tail
		if tail {
			block.CompactionSummaryID = summaryID
		}
		if part.Type == core.PartToolResult && part.ToolResult != nil && children != nil {
			block.ChildSessionIDs = children(part.ToolResult.Name, part.ToolResult.Content)
		}
		d.addBlock(block, text, format)
	}
}

func projectPart(msg session.Message, prefix string, index int, part core.Part, compaction *Compaction) (Block, string, string) {
	kind, text, format := "", "", "text"
	switch part.Type {
	case core.PartText:
		kind, text = textKind(msg), part.Text
		if text == "" {
			return Block{}, "", ""
		}
		if compaction != nil {
			kind = "compaction_summary"
		}
	case core.PartImage:
		kind, text = "image", imageMetadata(part)
	case core.PartFile:
		kind, text = "file", fileMetadata(part)
	case core.PartSkillActivation:
		if part.SkillActivation != nil {
			kind, text = "skill_activation", FormatSkillActivationProvenance(part.SkillActivation)
		}
	case core.PartToolCall:
		if part.ToolCall != nil {
			kind, text, format = "tool_call", FormatJSON(part.ToolCall.Arguments), "json"
		}
	case core.PartToolResult:
		if part.ToolResult != nil {
			kind, text = "tool_result", toolResultText(part.ToolResult)
		}
	}
	if kind == "" {
		return Block{}, "", ""
	}
	block := blockForMessage(msg, prefix, index, kind)
	if kind == "compaction_summary" {
		block.Compaction = compaction
	}
	decoratePart(&block, part)
	if part.Type == core.PartText {
		block, text = projectTimelineText(msg, prefix, index, part.Text, block, text)
	}
	return block, text, format
}

func decoratePart(block *Block, part core.Part) {
	switch part.Type {
	case core.PartImage:
		block.Title = "image"
		if part.ImageData != nil && part.ImageData.MediaType != "" {
			block.Title = part.ImageData.MediaType
		}
	case core.PartFile:
		if part.FileData != nil {
			block.Title = part.FileData.Filename
		}
	case core.PartSkillActivation:
		block.Title = part.SkillActivation.Name
	case core.PartToolCall:
		tc := part.ToolCall
		block.Title, block.Tool = tc.Name, &Tool{CallID: tc.ID, Name: tc.Name}
	case core.PartToolResult:
		tr := part.ToolResult
		block.Title, block.Tool = tr.Name, &Tool{CallID: tr.ID, Name: tr.Name, IsError: tr.IsError}
	}
}

func (d *Document) addReasoning(msg session.Message, prefix string, index int, part core.Part, tail bool, summaryID string) {
	if part.ReasoningContent == "" && len(part.ReasoningSummaryParts) == 0 && part.ReasoningEncryptedContent == "" {
		return
	}
	kind, title, text := ReasoningDisplay(part)
	block := blockForMessage(msg, prefix, index, "reasoning")
	block.Title, block.ReasoningKind = title, kind
	block.EncryptedBytes = len(part.ReasoningEncryptedContent)
	if kind == core.ReasoningKindEncrypted {
		block.EncryptedBytes += len(part.ReasoningContent)
	}
	block.CompactionTail = msg.CompactionTail || tail
	if tail {
		block.CompactionSummaryID = summaryID
	}
	d.addBlock(block, text, "text")
}

func (d *Document) addBlock(block Block, text, format string) {
	block.TotalBytes = len(text)
	if text != "" {
		block.TotalLines = strings.Count(text, "\n") + 1
		block.HasBody = true
		block.Preview = preview(text)
		d.bodies[block.ID] = Body{ID: block.ID, Format: format, Text: text, TotalBytes: len(text)}
	}
	d.Blocks = append(d.Blocks, block)
}

func imageMetadata(part core.Part) string {
	if part.ImageData == nil {
		return "[Image]"
	}
	return fmt.Sprintf("[Image: %s, %d bytes]", part.ImageData.MediaType, encodedBytes(part.ImageData.Base64))
}

func encodedBytes(encoded string) int {
	// Computing decoded length does not allocate or expose the underlying bytes.
	return base64.StdEncoding.DecodedLen(len(encoded)) - min(2, len(encoded)-len(strings.TrimRight(encoded, "=")))
}

func fileMetadata(part core.Part) string {
	if part.Text != "" {
		return part.Text
	}
	if part.FileData == nil {
		return "[File]"
	}
	file := part.FileData
	return fmt.Sprintf("[File: %s, %s, %d bytes]", file.Filename, file.MediaType, file.SizeBytes)
}

func toolResultText(result *core.ToolResult) string {
	text := result.Content
	n := len(result.Images) + len(result.Media)
	for _, part := range result.ContentParts {
		if part.ImageData != nil {
			n++
		}
		if result.Content == "" && part.Type == core.ToolContentPartText {
			text += part.Text
		}
	}
	if n > 0 {
		if text != "" {
			text += "\n"
		}
		text += fmt.Sprintf("[%d image(s) attached]", n)
	}
	return text
}

func (d *Document) addRuntime(messages []session.Message, opts RuntimeOptions) {
	rt := Runtime{Loaded: opts.Loaded, UnavailableReason: opts.UnavailableReason}
	if !opts.Loaded {
		if rt.UnavailableReason == "" {
			rt.UnavailableReason = "runtime_not_loaded"
		}
		d.Header.Runtime = rt
		return
	}
	if opts.UnavailableReason != "" {
		d.Header.Runtime = rt
		return
	}
	rt.ToolDiscovery = opts.ToolDiscovery
	if opts.SystemPrompt != "" {
		rt.SystemPromptBlockID = "runtime.system_prompt"
		matches := persistedSystemPrompt(messages) == opts.SystemPrompt
		rt.SystemPromptMatchesPersisted = &matches
		// Header-only body: the transcript block list stays persisted history.
		d.bodies[rt.SystemPromptBlockID] = Body{ID: rt.SystemPromptBlockID, Format: "text", Text: opts.SystemPrompt, TotalBytes: len(opts.SystemPrompt)}
	}
	for i, tool := range opts.Tools {
		id := fmt.Sprintf("runtime.tool.%d", i)
		schema, err := json.Marshal(tool.Schema)
		if err != nil {
			// A malformed plugin schema cannot be represented as a JSON body.
			continue
		}
		text, err := json.MarshalIndent(struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Schema      any    `json:"schema"`
		}{tool.Name, tool.Description, tool.Schema}, "", "  ")
		if err != nil {
			continue
		}
		rt.Tools = append(rt.Tools, RuntimeTool{BlockID: id, Name: tool.Name, Description: tool.Description, SchemaBytes: len(schema)})
		d.bodies[id] = Body{ID: id, Format: "json", Text: string(text), TotalBytes: len(text)}
	}
	d.Header.Runtime = rt
}

func persistedSystemPrompt(messages []session.Message) string {
	for _, msg := range messages {
		if msg.Role != core.RoleSystem {
			continue
		}
		var texts []string
		for _, part := range msg.Parts {
			if part.Type == core.PartText && part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
		if len(msg.Parts) == 0 {
			return msg.TextContent
		}
		return strings.Join(texts, "\n\n")
	}
	return ""
}
