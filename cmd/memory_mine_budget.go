package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/session"
)

const (
	defaultMemoryMinePromptMaxTokens   = 12000
	defaultMemoryMineTaxonomyMaxTokens = 1000
	defaultMemoryMineToolMaxTurns      = 6
	defaultMemoryMineMaxOutputTokens   = 2048
	memoryMineFragmentSearchLimit      = 8
	memoryMineFragmentListLimit        = 20
	memoryMineFragmentReadChars        = 6000
	memoryMineFragmentSnippetChars     = 320
	memoryMineTruncateMarker           = "... [truncated for memory mining budget]"
)

type taxonomyDirSummary struct {
	Path  string
	Count int
}

type transcriptFitResult struct {
	Messages             []session.Message
	TruncatedMessages    int
	AssistantMessagesCut int
	UserMessagesCut      int
	SingleMessageClipped bool
}

func buildTaxonomyMap(fragments []memorydb.Fragment, maxTokens int) string {
	if maxTokens <= 0 {
		return "(omitted)"
	}
	if len(fragments) == 0 {
		base := "Memory fragment map:\n- total_fragments: 0\n- no existing fragments"
		if llm.EstimateTokens(base) <= maxTokens {
			return base
		}
		return "fragment_count=0; use lookup tools"
	}

	byPath := make([]memorydb.Fragment, len(fragments))
	copy(byPath, fragments)
	sort.Slice(byPath, func(i, j int) bool {
		if byPath[i].UpdatedAt.Equal(byPath[j].UpdatedAt) {
			return byPath[i].Path < byPath[j].Path
		}
		return byPath[i].UpdatedAt.After(byPath[j].UpdatedAt)
	})

	dirCounts := map[string]int{}
	for _, frag := range byPath {
		dir := path.Dir(frag.Path)
		if dir == "." {
			dir = "(root)"
		}
		dirCounts[dir]++
	}
	dirs := make([]taxonomyDirSummary, 0, len(dirCounts))
	for dir, count := range dirCounts {
		dirs = append(dirs, taxonomyDirSummary{Path: dir, Count: count})
	}
	sort.Slice(dirs, func(i, j int) bool {
		if dirs[i].Count == dirs[j].Count {
			return dirs[i].Path < dirs[j].Path
		}
		return dirs[i].Count > dirs[j].Count
	})

	var b strings.Builder
	b.WriteString("Memory fragment map:\n")
	b.WriteString(fmt.Sprintf("- total_fragments: %d\n", len(byPath)))
	b.WriteString("- note: compact map only; use lookup tools for exact duplicate checks or content inspection\n")
	if llm.EstimateTokens(b.String()) > maxTokens {
		return fmt.Sprintf("fragment_count=%d; use lookup tools", len(byPath))
	}

	dirBudget := maxTokens / 3
	if dirBudget < 150 {
		dirBudget = 150
	}
	b.WriteString("Directories:\n")
	dirLines := 0
	for _, dir := range dirs {
		line := fmt.Sprintf("- %s (%d)\n", dir.Path, dir.Count)
		if llm.EstimateTokens(b.String()+line) > dirBudget {
			break
		}
		b.WriteString(line)
		dirLines++
	}
	if dirLines == 0 {
		b.WriteString("- (directory summary omitted for budget)\n")
	}

	b.WriteString("Known fragment paths (recent-first, partial if needed):\n")
	addedPaths := 0
	for _, frag := range byPath {
		line := "- " + frag.Path + "\n"
		if llm.EstimateTokens(b.String()+line) > maxTokens {
			break
		}
		b.WriteString(line)
		addedPaths++
	}
	if omitted := len(byPath) - addedPaths; omitted > 0 {
		line := fmt.Sprintf("- ... %d more path(s) omitted from map; use lookup tools\n", omitted)
		if llm.EstimateTokens(b.String()+line) <= maxTokens {
			b.WriteString(line)
		}
	}

	out := strings.TrimSpace(b.String())
	if out == "" {
		return "(omitted)"
	}
	return out
}

func estimateExtractionPromptTokens(candidate memoryMineCandidate, startOffset, endOffset int, messages []session.Message, taxonomyMap string) int {
	prompt := buildExtractionPrompt(candidate, startOffset, endOffset, messages, taxonomyMap)
	return llm.EstimateTokens(memoryExtractionSystemPrompt) + llm.EstimateTokens(prompt)
}

func truncateMessageForPromptBudget(candidate memoryMineCandidate, startOffset, nextOffset int, taxonomyMap string, msg session.Message) (session.Message, bool) {
	text := strings.TrimSpace(msg.TextContent)
	if text == "" {
		return msg, estimateExtractionPromptTokens(candidate, startOffset, nextOffset, []session.Message{msg}, taxonomyMap) <= memoryMinePromptMaxTokens
	}

	runes := []rune(text)
	best := -1
	lo, hi := 0, len(runes)
	for lo <= hi {
		mid := (lo + hi) / 2
		candidateText := string(runes[:mid])
		if mid < len(runes) {
			candidateText = strings.TrimSpace(candidateText) + memoryMineTruncateMarker
		}
		clone := msg
		clone.TextContent = candidateText
		if estimateExtractionPromptTokens(candidate, startOffset, nextOffset, []session.Message{clone}, taxonomyMap) <= memoryMinePromptMaxTokens {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if best < 0 {
		return session.Message{}, false
	}
	return cloneMessageWithText(msg, string(runes[:best]), best < len(runes)), true
}

func fitMessagesForPromptBudget(candidate memoryMineCandidate, startOffset, nextOffset int, messages []session.Message, taxonomyMap string) (transcriptFitResult, bool) {
	result := transcriptFitResult{Messages: cloneMessages(messages)}
	if estimateExtractionPromptTokens(candidate, startOffset, nextOffset, result.Messages, taxonomyMap) <= memoryMinePromptMaxTokens {
		return result, true
	}

	passes := []struct {
		High int
		Med  int
		Low  int
	}{
		{High: 1200, Med: 600, Low: 160},
		{High: 800, Med: 320, Low: 80},
		{High: 500, Med: 180, Low: 0},
	}

	for _, pass := range passes {
		for i := range result.Messages {
			msg := &result.Messages[i]
			if msg.Role != llm.RoleAssistant {
				continue
			}
			if strings.TrimSpace(msg.TextContent) == "" {
				continue
			}
			target := assistantTargetChars(msg.TextContent, pass.High, pass.Med, pass.Low)
			trimmed, changed := truncateTextToChars(msg.TextContent, target)
			if !changed {
				continue
			}
			msg.TextContent = trimmed
			result.TruncatedMessages++
			result.AssistantMessagesCut++
			if estimateExtractionPromptTokens(candidate, startOffset, nextOffset, result.Messages, taxonomyMap) <= memoryMinePromptMaxTokens {
				return result, true
			}
		}
	}

	for i := range result.Messages {
		msg := &result.Messages[i]
		if msg.Role != llm.RoleUser {
			continue
		}
		if strings.TrimSpace(msg.TextContent) == "" {
			continue
		}
		trimmed, changed := truncateTextToChars(msg.TextContent, 4000)
		if !changed {
			continue
		}
		msg.TextContent = trimmed
		result.TruncatedMessages++
		result.UserMessagesCut++
		result.SingleMessageClipped = true
		if estimateExtractionPromptTokens(candidate, startOffset, nextOffset, result.Messages, taxonomyMap) <= memoryMinePromptMaxTokens {
			return result, true
		}
	}

	if len(result.Messages) == 1 {
		clipped, ok := truncateMessageForPromptBudget(candidate, startOffset, nextOffset, taxonomyMap, result.Messages[0])
		if ok {
			result.Messages[0] = clipped
			result.TruncatedMessages++
			if result.Messages[0].Role == llm.RoleAssistant {
				result.AssistantMessagesCut++
			} else if result.Messages[0].Role == llm.RoleUser {
				result.UserMessagesCut++
			}
			result.SingleMessageClipped = true
			return result, true
		}
	}

	return result, false
}

func cloneMessages(messages []session.Message) []session.Message {
	out := make([]session.Message, len(messages))
	copy(out, messages)
	return out
}

func cloneMessageWithText(msg session.Message, text string, truncated bool) session.Message {
	clone := msg
	clone.TextContent = strings.TrimSpace(text)
	if truncated {
		clone.TextContent = strings.TrimSpace(clone.TextContent) + memoryMineTruncateMarker
	}
	return clone
}

func truncateTextToChars(text string, limit int) (string, bool) {
	text = strings.TrimSpace(text)
	if limit < 0 {
		limit = 0
	}
	if utf8.RuneCountInString(text) <= limit {
		return text, false
	}
	runes := []rune(text)
	base := string(runes[:limit])
	base = strings.TrimSpace(base)
	if base == "" {
		return memoryMineTruncateMarker, true
	}
	return base + memoryMineTruncateMarker, true
}

func assistantTargetChars(text string, high, med, low int) int {
	score := assistantMessagePriority(text)
	switch score {
	case 2:
		return high
	case 1:
		return med
	default:
		return low
	}
}

func assistantMessagePriority(text string) int {
	text = strings.TrimSpace(strings.ToLower(text))
	if text == "" {
		return 0
	}
	score := 0
	for _, needle := range []string{"changed", "updated", "configured", "deployed", "restart", "merged", "commit", "pr ", "issue", "path", "url", "http", "https", "provider", "model", "token", "budget", "cache", "fragment", "memory", "version", "tool", "agent", "prompt"} {
		if strings.Contains(text, needle) {
			score += 2
		}
	}
	for _, needle := range []string{"- ", "* ", "1. ", "2. ", "##", "```", "/", ".md", ".go", ".rb", "postgres", "redis", "docker", "system", "config"} {
		if strings.Contains(text, needle) {
			score++
		}
	}
	if strings.Count(text, "\n") >= 3 {
		score += 2
	}
	if utf8.RuneCountInString(text) >= 1000 {
		score++
	}
	if score >= 5 {
		return 2
	}
	if score >= 2 {
		return 1
	}
	return 0
}

func registerMemoryExtractionTools(engine *llm.Engine, store *memorydb.Store, agent string) ([]llm.ToolSpec, func()) {
	tools := []llm.Tool{
		&memorySearchFragmentsTool{store: store, agent: agent},
		&memoryListFragmentsTool{store: store, agent: agent},
		&memoryGetFragmentTool{store: store, agent: agent},
	}
	specs := make([]llm.ToolSpec, 0, len(tools))
	for _, tool := range tools {
		engine.RegisterTool(tool)
		specs = append(specs, tool.Spec())
	}
	cleanup := func() {
		for _, tool := range tools {
			engine.UnregisterTool(tool.Spec().Name)
		}
	}
	return specs, cleanup
}

type memorySearchFragmentsTool struct {
	store *memorydb.Store
	agent string
}

func (t *memorySearchFragmentsTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "memory_search_fragments",
		Description: "Search existing memory fragments by content and path. Use this before creating a new fragment when duplicate or overlapping memory might already exist.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{"type": "string", "description": "Search query describing the memory you want to inspect"},
				"limit": map[string]interface{}{"type": "integer", "description": "Maximum number of hits to return (default 5, max 8)", "default": 5},
			},
			"required":             []string{"query"},
			"additionalProperties": false,
		},
	}
}

func (t *memorySearchFragmentsTool) Preview(args json.RawMessage) string {
	var payload struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Query)
}

func (t *memorySearchFragmentsTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var payload struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return llm.ToolOutput{}, fmt.Errorf("parse memory_search_fragments args: %w", err)
	}
	payload.Query = strings.TrimSpace(payload.Query)
	if payload.Query == "" {
		return llm.ToolOutput{}, fmt.Errorf("query is required")
	}
	if payload.Limit <= 0 || payload.Limit > memoryMineFragmentSearchLimit {
		payload.Limit = 5
	}

	results, err := t.store.SearchFragments(ctx, payload.Query, payload.Limit, t.agent)
	if err != nil {
		return llm.ToolOutput{}, err
	}
	for i := range results {
		snippetLimit := memoryMineFragmentSnippetChars
		if memoryMineReadBytes > 0 && memoryMineReadBytes < snippetLimit {
			snippetLimit = memoryMineReadBytes
		}
		if len(results[i].Snippet) > snippetLimit {
			results[i].Snippet = results[i].Snippet[:snippetLimit] + "..."
		}
	}
	response := map[string]any{
		"query":   payload.Query,
		"agent":   t.agent,
		"results": results,
	}
	data, _ := json.MarshalIndent(response, "", "  ")
	return llm.TextOutput(string(data)), nil
}

type memoryListFragmentsTool struct {
	store *memorydb.Store
	agent string
}

func (t *memoryListFragmentsTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "memory_list_fragments",
		Description: "List existing memory fragment paths, optionally narrowed by prefix. Use this when you know the area of memory you want to inspect but not the exact path.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"prefix": map[string]interface{}{"type": "string", "description": "Optional path prefix such as fragments/preferences or images/2026-03"},
				"limit":  map[string]interface{}{"type": "integer", "description": "Maximum number of paths to return (default 20)", "default": 20},
			},
			"additionalProperties": false,
		},
	}
}

func (t *memoryListFragmentsTool) Preview(args json.RawMessage) string {
	var payload struct {
		Prefix string `json:"prefix"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Prefix)
}

func (t *memoryListFragmentsTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var payload struct {
		Prefix string `json:"prefix"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return llm.ToolOutput{}, fmt.Errorf("parse memory_list_fragments args: %w", err)
	}
	prefix := strings.TrimSpace(payload.Prefix)
	limit := payload.Limit
	if limit <= 0 || limit > memoryMineFragmentListLimit {
		limit = memoryMineFragmentListLimit
	}

	fragments, err := t.store.ListFragments(ctx, memorydb.ListOptions{Agent: t.agent})
	if err != nil {
		return llm.ToolOutput{}, err
	}
	paths := make([]string, 0, limit)
	for _, frag := range fragments {
		if prefix != "" && !strings.HasPrefix(frag.Path, prefix) {
			continue
		}
		paths = append(paths, frag.Path)
		if len(paths) >= limit {
			break
		}
	}
	response := map[string]any{
		"agent":  t.agent,
		"prefix": prefix,
		"paths":  paths,
	}
	data, _ := json.MarshalIndent(response, "", "  ")
	return llm.TextOutput(string(data)), nil
}

type memoryGetFragmentTool struct {
	store *memorydb.Store
	agent string
}

func (t *memoryGetFragmentTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "memory_get_fragment",
		Description: "Read one existing memory fragment by exact path. Use this before updating a fragment so you can merge instead of overwrite blindly.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string", "description": "Exact fragment path"},
			},
			"required":             []string{"path"},
			"additionalProperties": false,
		},
	}
}

func (t *memoryGetFragmentTool) Preview(args json.RawMessage) string {
	var payload struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Path)
}

func (t *memoryGetFragmentTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var payload struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return llm.ToolOutput{}, fmt.Errorf("parse memory_get_fragment args: %w", err)
	}
	fragPath := strings.TrimSpace(payload.Path)
	if fragPath == "" {
		return llm.ToolOutput{}, fmt.Errorf("path is required")
	}
	frag, err := t.store.GetFragment(ctx, t.agent, fragPath)
	if err != nil {
		return llm.ToolOutput{}, err
	}
	if frag == nil {
		return llm.TextOutput(fmt.Sprintf("{\n  \"agent\": %q,\n  \"path\": %q,\n  \"found\": false\n}", t.agent, fragPath)), nil
	}
	content := frag.Content
	readLimit := memoryMineFragmentReadChars
	if memoryMineReadBytes > 0 && memoryMineReadBytes < readLimit {
		readLimit = memoryMineReadBytes
	}
	if len(content) > readLimit {
		content = content[:readLimit] + "\n... [truncated]"
	}
	response := map[string]any{
		"agent":   t.agent,
		"path":    frag.Path,
		"found":   true,
		"content": content,
	}
	data, _ := json.MarshalIndent(response, "", "  ")
	return llm.TextOutput(string(data)), nil
}
