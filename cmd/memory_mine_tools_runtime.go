package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/llm"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
)

type memoryExtractionResult struct {
	Created       int
	Updated       int
	Skipped       int
	AffectedPaths []string
	FinalText     string
}

type memoryExtractionCollector struct {
	mu       sync.Mutex
	created  int
	updated  int
	skipped  int
	affected []string
	seen     map[string]struct{}
}

func newMemoryExtractionCollector() *memoryExtractionCollector {
	return &memoryExtractionCollector{seen: map[string]struct{}{}}
}

func (c *memoryExtractionCollector) noteCreated(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created++
	c.addPath(path)
}

func (c *memoryExtractionCollector) noteUpdated(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updated++
	c.addPath(path)
}

func (c *memoryExtractionCollector) noteSkipped() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.skipped++
}

func (c *memoryExtractionCollector) addPath(path string) {
	if _, ok := c.seen[path]; ok {
		return
	}
	c.seen[path] = struct{}{}
	c.affected = append(c.affected, path)
}

func (c *memoryExtractionCollector) result(finalText string) memoryExtractionResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	affected := make([]string, len(c.affected))
	copy(affected, c.affected)
	sort.Strings(affected)
	res := memoryExtractionResult{
		Created:       c.created,
		Updated:       c.updated,
		Skipped:       c.skipped,
		AffectedPaths: affected,
		FinalText:     strings.TrimSpace(finalText),
	}
	if res.Created == 0 && res.Updated == 0 && res.Skipped == 0 {
		res.Skipped = 1
	}
	return res
}

type memoryCreateFragmentTool struct {
	store     *memorydb.Store
	agent     string
	collector *memoryExtractionCollector
}

func (t *memoryCreateFragmentTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "memory_create_fragment",
		Description: "Create a new durable memory fragment. Use this only when memory does not already exist under a suitable path.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":    map[string]interface{}{"type": "string", "description": "Relative fragment path"},
				"content": map[string]interface{}{"type": "string", "description": "Full fragment markdown/text content"},
				"reason":  map[string]interface{}{"type": "string", "description": "Why this new fragment is warranted"},
			},
			"required":             []string{"path", "content"},
			"additionalProperties": false,
		},
	}
}

func (t *memoryCreateFragmentTool) Preview(args json.RawMessage) string {
	var payload struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(args, &payload)
	return strings.TrimSpace(payload.Path)
}

func (t *memoryCreateFragmentTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var payload struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return llm.ToolOutput{}, fmt.Errorf("parse memory_create_fragment args: %w", err)
	}
	fragPath, err := validateFragmentPath(payload.Path)
	if err != nil {
		return llm.ToolOutput{}, err
	}
	content := strings.TrimSpace(payload.Content)
	if content == "" {
		return llm.ToolOutput{}, fmt.Errorf("content cannot be empty")
	}
	if len([]byte(content)) > 8192 {
		return llm.ToolOutput{}, fmt.Errorf("content exceeds 8192 bytes")
	}
	if !memoryDryRun {
		if err := t.store.CreateFragment(ctx, &memorydb.Fragment{Agent: t.agent, Path: fragPath, Content: content, Source: memorydb.DefaultSourceMine}); err != nil {
			return llm.ToolOutput{}, err
		}
	}
	t.collector.noteCreated(fragPath)
	return llm.TextOutput(fmt.Sprintf("created %s", fragPath)), nil
}

type memoryUpdateFragmentTool struct {
	store     *memorydb.Store
	agent     string
	collector *memoryExtractionCollector
}

func (t *memoryUpdateFragmentTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "memory_update_fragment",
		Description: "Update an existing durable memory fragment after inspecting it. Use this when the memory already exists and should be revised or expanded.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":    map[string]interface{}{"type": "string", "description": "Relative fragment path"},
				"content": map[string]interface{}{"type": "string", "description": "Replacement fragment content"},
				"reason":  map[string]interface{}{"type": "string", "description": "Why this fragment should be updated"},
			},
			"required":             []string{"path", "content"},
			"additionalProperties": false,
		},
	}
}

func (t *memoryUpdateFragmentTool) Preview(args json.RawMessage) string {
	var payload struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(args, &payload)
	return strings.TrimSpace(payload.Path)
}

func (t *memoryUpdateFragmentTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var payload struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return llm.ToolOutput{}, fmt.Errorf("parse memory_update_fragment args: %w", err)
	}
	fragPath, err := validateFragmentPath(payload.Path)
	if err != nil {
		return llm.ToolOutput{}, err
	}
	content := strings.TrimSpace(payload.Content)
	if content == "" {
		return llm.ToolOutput{}, fmt.Errorf("content cannot be empty")
	}
	if len([]byte(content)) > 8192 {
		return llm.ToolOutput{}, fmt.Errorf("content exceeds 8192 bytes")
	}
	if !memoryDryRun {
		ok, err := t.store.UpdateFragment(ctx, t.agent, fragPath, content)
		if err != nil {
			return llm.ToolOutput{}, err
		}
		if !ok {
			return llm.ToolOutput{}, fmt.Errorf("fragment %s does not exist", fragPath)
		}
	}
	t.collector.noteUpdated(fragPath)
	return llm.TextOutput(fmt.Sprintf("updated %s", fragPath)), nil
}

func registerMemoryExtractionTools(engine *llm.Engine, store *memorydb.Store, agent string, collector *memoryExtractionCollector) ([]llm.ToolSpec, func()) {
	tools := []llm.Tool{
		&memorySearchFragmentsTool{store: store, agent: agent},
		&memoryListFragmentsTool{store: store, agent: agent},
		&memoryGetFragmentTool{store: store, agent: agent},
		&memoryCreateFragmentTool{store: store, agent: agent, collector: collector},
		&memoryUpdateFragmentTool{store: store, agent: agent, collector: collector},
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
