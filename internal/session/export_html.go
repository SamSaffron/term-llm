package session

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"path/filepath"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"github.com/samsaffron/term-llm/internal/llm"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
)

const maxHTMLExportInlineImageBytes = 2 << 20

//go:embed export_html.tmpl
var exportHTMLTemplateFS embed.FS

type htmlExportView struct {
	Title       string
	SessionID   string
	Status      string
	Agent       string
	Provider    string
	Model       string
	ModelLabel  string
	Mode        string
	Origin      string
	Reasoning   string
	Created     string
	Updated     string
	CWD         string
	Worktree    string
	Tools       string
	MCP         string
	Tags        string
	UserTurns   string
	LLMTurns    string
	ToolCalls   string
	Input       string
	Cached      string
	CacheWrite  string
	Output      string
	Messages    []htmlExportMessage
	DetailCount int
}

type htmlExportMessage struct {
	Role       string
	RoleLabel  string
	Time       string
	Duration   string
	Compaction bool
	ToolGroup  bool
	ToolCount  int
	Blocks     []htmlExportBlock
}

type htmlExportBlock struct {
	Kind  string
	HTML  template.HTML
	Text  string
	Title string
	Raw   bool
	Tool  *htmlExportTool
	Image *htmlExportImage
	File  *htmlExportFile
}

type htmlExportTool struct {
	Name       string
	ID         string
	Arguments  string
	HasCall    bool
	HasResult  bool
	Result     string
	IsError    bool
	Diffs      []htmlExportDiff
	Images     []htmlExportImage
	ExtraTexts []string
}

type htmlExportDiff struct {
	File string
	Old  string
	New  string
	Line int
}

type htmlExportImage struct {
	URL       template.URL
	MediaType string
	Omitted   bool
}

type htmlExportFile struct {
	Filename  string
	MediaType string
	Size      string
}

type pendingHTMLTool struct {
	call       *llm.ToolCall
	messageIdx int
}

// VisibleExportMessages returns messages intended for human-readable exports.
// Retained post-compaction context is already represented earlier in scrollback.
func VisibleExportMessages(messages []Message) []Message {
	visible := make([]Message, 0, len(messages))
	for i := range messages {
		msg := messages[i]
		if !msg.CompactionTail && !msg.IsGoalSteering() {
			visible = append(visible, msg)
		}
	}
	return visible
}

// ExportToHTML renders a self-contained, interactive transcript document.
func ExportToHTML(sess *Session, messages []Message, opts ExportOptions) (string, error) {
	if sess == nil {
		return "", fmt.Errorf("session is required")
	}

	view := buildHTMLExportView(sess, messages, opts)
	tmpl, err := template.New("export_html.tmpl").ParseFS(exportHTMLTemplateFS, "export_html.tmpl")
	if err != nil {
		return "", fmt.Errorf("parse HTML export template: %w", err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, view); err != nil {
		return "", fmt.Errorf("render HTML export: %w", err)
	}
	return out.String(), nil
}

func buildHTMLExportView(sess *Session, messages []Message, opts ExportOptions) htmlExportView {
	title := strings.TrimSpace(sess.PreferredLongTitle())
	if title == "" {
		title = strings.TrimSpace(sess.PreferredShortTitle())
	}
	if title == "" {
		title = ShortID(sess.ID)
	}
	if title == "" {
		title = "term-llm session"
	}
	status := string(sess.Status)
	if status == "" {
		status = "active"
	}
	mode := string(sess.Mode)
	if mode == "" {
		mode = "chat"
	}
	view := htmlExportView{
		Title: title, SessionID: sess.ID, Status: status, Agent: sess.Agent,
		Provider: sess.Provider, Model: sess.Model, ModelLabel: htmlProviderModelLabel(sess.Provider, sess.Model), Mode: mode, Origin: string(sess.Origin),
		Reasoning: strings.TrimSpace(strings.Join([]string{sess.ReasoningMode, sess.ReasoningEffort}, " ")),
		Created:   formatHTMLExportTime(sess.CreatedAt), Updated: formatHTMLExportTime(sess.UpdatedAt),
		CWD: sess.CWD, Worktree: sess.WorktreeDir, Tools: sess.Tools, MCP: sess.MCP, Tags: sess.Tags,
		UserTurns: formatHTMLCount(sess.UserTurns), LLMTurns: formatHTMLCount(sess.LLMTurns), ToolCalls: formatHTMLCount(sess.ToolCalls),
		Input: formatHTMLCount(sess.InputTokens), Cached: formatHTMLCount(sess.CachedInputTokens), CacheWrite: formatHTMLCount(sess.CacheWriteTokens), Output: formatHTMLCount(sess.OutputTokens),
	}
	view.Messages, view.DetailCount = buildHTMLExportMessages(VisibleExportMessages(messages), opts)
	return view
}

func htmlProviderModelLabel(provider, model string) string {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" {
		return model
	}
	if model == "" || strings.EqualFold(provider, model) {
		return provider
	}
	if strings.Contains(provider, "(") && strings.HasSuffix(provider, ")") {
		return provider
	}
	return provider + " · " + model
}

func buildHTMLExportMessages(messages []Message, opts ExportOptions) ([]htmlExportMessage, int) {
	markdown := goldmark.New(goldmark.WithExtensions(extension.GFM))
	views := make([]htmlExportMessage, 0, len(messages))
	pending := make(map[string]pendingHTMLTool)
	inlineBytes := 0
	detailCount := 0

	for _, msg := range messages {
		compaction := IsInternalCompactionSummaryMessage(msg)
		if msg.Role == llm.RoleSystem && !opts.IncludeSystem && !compaction {
			continue
		}
		view := htmlExportMessage{
			Role: string(msg.Role), RoleLabel: htmlRoleLabel(msg.Role),
			Time: formatHTMLExportTime(msg.CreatedAt), Duration: formatHTMLDuration(msg.DurationMs), Compaction: compaction,
		}
		messageIdx := len(views)
		parts := msg.Parts
		if len(parts) == 0 && msg.TextContent != "" {
			parts = []llm.Part{{Type: llm.PartText, Text: msg.TextContent}}
		}
		for _, part := range parts {
			switch part.Type {
			case llm.PartText:
				if reasoning, ok := htmlReasoningBlock(part, opts, markdown); ok {
					view.Blocks = append(view.Blocks, reasoning)
					detailCount++
				}
				if part.Text != "" {
					view.Blocks = append(view.Blocks, htmlExportBlock{Kind: "markdown", HTML: renderSafeMarkdown(markdown, part.Text)})
				}
			case llm.PartImage:
				image := buildHTMLImage(part.ImageData, &inlineBytes)
				view.Blocks = append(view.Blocks, htmlExportBlock{Kind: "image", Image: &image})
			case llm.PartFile:
				view.Blocks = append(view.Blocks, htmlExportBlock{Kind: "file", File: buildHTMLFile(part)})
			case llm.PartToolCall:
				if part.ToolCall == nil {
					continue
				}
				if part.ToolCall.ID == "" {
					view.Blocks = append(view.Blocks, htmlExportBlock{Kind: "tool", Tool: buildHTMLTool(part.ToolCall, nil, &inlineBytes)})
					detailCount++
				} else {
					pending[part.ToolCall.ID] = pendingHTMLTool{call: part.ToolCall, messageIdx: messageIdx}
				}
			case llm.PartToolResult:
				if part.ToolResult == nil {
					continue
				}
				var call *llm.ToolCall
				if match, ok := pending[part.ToolResult.ID]; ok {
					call = match.call
					delete(pending, part.ToolResult.ID)
				}
				view.Blocks = append(view.Blocks, htmlExportBlock{Kind: "tool", Tool: buildHTMLTool(call, part.ToolResult, &inlineBytes)})
				detailCount++
			}
		}
		if compaction && len(view.Blocks) == 0 && msg.TextContent != "" {
			view.Blocks = append(view.Blocks, htmlExportBlock{Kind: "markdown", HTML: renderSafeMarkdown(markdown, msg.TextContent)})
		}
		views = append(views, view)
	}

	for _, orphan := range pending {
		if orphan.messageIdx >= 0 && orphan.messageIdx < len(views) {
			views[orphan.messageIdx].Blocks = append(views[orphan.messageIdx].Blocks, htmlExportBlock{Kind: "tool", Tool: buildHTMLTool(orphan.call, nil, &inlineBytes)})
			detailCount++
		}
	}
	filtered := views[:0]
	for _, view := range views {
		if len(view.Blocks) > 0 {
			filtered = append(filtered, view)
		}
	}
	return groupHTMLExportTools(filtered), detailCount
}

func groupHTMLExportTools(messages []htmlExportMessage) []htmlExportMessage {
	grouped := make([]htmlExportMessage, 0, len(messages))
	for _, message := range messages {
		if !isHTMLExportToolMessage(message) {
			grouped = append(grouped, message)
			continue
		}
		if len(grouped) > 0 && isHTMLExportToolMessage(grouped[len(grouped)-1]) {
			last := &grouped[len(grouped)-1]
			last.Blocks = append(last.Blocks, message.Blocks...)
			continue
		}
		grouped = append(grouped, message)
	}
	for i := range grouped {
		if !isHTMLExportToolMessage(grouped[i]) {
			continue
		}
		grouped[i].ToolCount = len(grouped[i].Blocks)
		if grouped[i].ToolCount > 1 {
			grouped[i].ToolGroup = true
			grouped[i].RoleLabel = "Tools"
		}
	}
	return grouped
}

func isHTMLExportToolMessage(message htmlExportMessage) bool {
	if message.Role != string(llm.RoleTool) || len(message.Blocks) == 0 {
		return false
	}
	for _, block := range message.Blocks {
		if block.Kind != "tool" {
			return false
		}
	}
	return true
}

func htmlReasoningBlock(part llm.Part, opts ExportOptions, markdown goldmark.Markdown) (htmlExportBlock, bool) {
	hasReasoning := part.ReasoningContent != "" || len(part.ReasoningSummaryParts) > 0
	kind := llm.NormalizeStoredReasoningKind(part.ReasoningKind, hasReasoning)
	content := strings.TrimSpace(part.ReasoningContent)
	if content == "" {
		content = strings.TrimSpace(strings.Join(part.ReasoningSummaryParts, "\n\n"))
	}
	if content == "" {
		return htmlExportBlock{}, false
	}
	switch kind {
	case llm.ReasoningKindSummary:
		if !opts.IncludeReasoningSummaries {
			return htmlExportBlock{}, false
		}
		parsed := internalreasoning.ParseReasoningSummary(content)
		title := strings.TrimSpace(part.ReasoningSummaryTitle)
		if title == "" {
			title = parsed.Title
		}
		body := strings.TrimSpace(parsed.Body)
		if body == "" {
			body = content
		}
		return htmlExportBlock{Kind: "reasoning", Title: title, HTML: renderSafeMarkdown(markdown, body)}, true
	case llm.ReasoningKindRaw:
		if !opts.IncludeRawReasoning {
			return htmlExportBlock{}, false
		}
		return htmlExportBlock{Kind: "reasoning", Title: "Raw reasoning", Raw: true, HTML: renderSafeMarkdown(markdown, content)}, true
	default:
		return htmlExportBlock{}, false
	}
}

func renderSafeMarkdown(markdown goldmark.Markdown, source string) template.HTML {
	var out bytes.Buffer
	if err := markdown.Convert([]byte(source), &out); err != nil {
		return template.HTML(template.HTMLEscapeString(source))
	}
	// Goldmark's default renderer suppresses raw HTML and unsafe link schemes.
	return template.HTML(out.String()) // #nosec G203 -- output comes from Goldmark in safe mode.
}

func buildHTMLTool(call *llm.ToolCall, result *llm.ToolResult, inlineBytes *int) *htmlExportTool {
	tool := &htmlExportTool{}
	if call != nil {
		tool.HasCall = true
		tool.Name = call.Name
		tool.ID = call.ID
		tool.Arguments = prettyJSON(call.Arguments)
	}
	if result != nil {
		tool.HasResult = true
		tool.IsError = result.IsError
		tool.Result = result.Content
		if tool.Name == "" {
			tool.Name = result.Name
		}
		if tool.ID == "" {
			tool.ID = result.ID
		}
		for _, diff := range result.Diffs {
			tool.Diffs = append(tool.Diffs, htmlExportDiff{File: diff.File, Old: diff.Old, New: diff.New, Line: diff.Line})
		}
		for _, part := range result.ContentParts {
			switch part.Type {
			case llm.ToolContentPartText:
				if part.Text != "" && part.Text != result.Content {
					tool.ExtraTexts = append(tool.ExtraTexts, part.Text)
				}
			case llm.ToolContentPartImageData:
				tool.Images = append(tool.Images, buildHTMLImage(part.ImageData, inlineBytes))
			}
		}
	}
	if tool.Name == "" {
		tool.Name = "Tool"
	}
	return tool
}

func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw)
	}
	pretty, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(pretty)
}

func buildHTMLImage(data *llm.ToolImageData, inlineBytes *int) htmlExportImage {
	image := htmlExportImage{Omitted: true}
	if data == nil {
		return image
	}
	mediaType := strings.ToLower(strings.TrimSpace(data.MediaType))
	image.MediaType = mediaType
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return image
	}
	decoded, err := base64.StdEncoding.DecodeString(data.Base64)
	if err != nil || len(decoded) == 0 || *inlineBytes+len(decoded) > maxHTMLExportInlineImageBytes {
		return image
	}
	*inlineBytes += len(decoded)
	image.URL = template.URL("data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(decoded))
	image.Omitted = false
	return image
}

func buildHTMLFile(part llm.Part) *htmlExportFile {
	file := &htmlExportFile{Filename: "File attachment"}
	if part.FileData == nil {
		return file
	}
	if name := strings.TrimSpace(filepath.Base(part.FileData.Filename)); name != "" && name != "." {
		file.Filename = name
	}
	file.MediaType = strings.TrimSpace(part.FileData.MediaType)
	if part.FileData.SizeBytes > 0 {
		file.Size = fmt.Sprintf("%d bytes", part.FileData.SizeBytes)
	}
	return file
}

func htmlRoleLabel(role llm.Role) string {
	switch role {
	case llm.RoleUser:
		return "User"
	case llm.RoleAssistant:
		return "Assistant"
	case llm.RoleSystem:
		return "System"
	case llm.RoleTool:
		return "Tool"
	case llm.RoleDeveloper:
		return "Developer"
	case llm.RoleEvent:
		return "Event"
	default:
		if role == "" {
			return "Message"
		}
		return strings.ToUpper(string(role[:1])) + string(role[1:])
	}
}

func formatHTMLExportTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format("2006-01-02 15:04:05 UTC")
}

func formatHTMLDuration(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return (time.Duration(ms) * time.Millisecond).String()
}

func formatHTMLCount(value int) string {
	s := fmt.Sprintf("%d", value)
	start := 0
	if strings.HasPrefix(s, "-") {
		start = 1
	}
	for i := len(s) - 3; i > start; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
