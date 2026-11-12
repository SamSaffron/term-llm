package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
)

type memoryExtractionResult struct {
	Created       int
	Updated       int
	Skipped       int
	Rejected      int
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
	rejected int
	pending  map[string]bool
}

func newMemoryExtractionCollector() *memoryExtractionCollector {
	return &memoryExtractionCollector{seen: map[string]struct{}{}, pending: map[string]bool{}}
}

func writeKey(p string) string {
	return path.Clean(strings.TrimSpace(strings.ReplaceAll(p, "\\", "/")))
}
func (c *memoryExtractionCollector) noteFailure(p string, secret bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil {
		c.pending = map[string]bool{}
	}
	c.pending[writeKey(p)] = c.pending[writeKey(p)] || secret
	if secret {
		c.rejected++
	}
}
func (c *memoryExtractionCollector) pendingError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return nil
	}
	for _, secret := range c.pending {
		if secret {
			return fmt.Errorf("%w: %d unresolved write(s); batch cursor retained; retry a corrected write to the same path", errMemorySafety, len(c.pending))
		}
	}
	return fmt.Errorf("%d unresolved memory write(s); batch cursor retained", len(c.pending))
}

func (c *memoryExtractionCollector) noteCreated(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, writeKey(path))
	c.created++
	c.addPath(path)
}

func (c *memoryExtractionCollector) noteUpdated(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, writeKey(path))
	c.updated++
	c.addPath(path)
}

// An exact already-stored correction is an accepted examined no-op, not a
// missing fragment or a fictitious update. Still attribute the inspected range.
func (c *memoryExtractionCollector) noteUnchanged(p string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, writeKey(p))
	c.skipped++
	c.addPath(p)
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
		Rejected:      c.rejected,
		AffectedPaths: affected,
		FinalText:     strings.TrimSpace(finalText),
	}
	if res.Created == 0 && res.Updated == 0 && res.Skipped == 0 && len(c.pending) == 0 {
		res.Skipped = 1
	}
	return res
}

type memoryCreateFragmentTool struct {
	store      *memorydb.Store
	agent      string
	dryRun     bool
	collector  *memoryExtractionCollector
	protection *memoryProtection
}

func (t *memoryCreateFragmentTool) Spec() core.ToolSpec {
	return core.ToolSpec{
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

func (t *memoryCreateFragmentTool) Execute(ctx context.Context, args json.RawMessage) (out core.ToolOutput, err error) {
	var payload struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Reason  string `json:"reason"`
	}
	defer func() {
		if err != nil {
			t.collector.noteFailure(payload.Path, errors.Is(err, errMemorySafety))
		}
	}()

	if err := json.Unmarshal(args, &payload); err != nil {
		return core.ToolOutput{}, fmt.Errorf("parse memory_create_fragment args: %w", err)
	}
	fragPath, err := validateFragmentPath(payload.Path)
	if err != nil {
		return core.ToolOutput{}, err
	}
	content := strings.TrimSpace(payload.Content)
	if content == "" {
		return core.ToolOutput{}, fmt.Errorf("content cannot be empty")
	}
	if len([]byte(content)) > 8192 {
		return core.ToolOutput{}, fmt.Errorf("content exceeds 8192 bytes")
	}
	if protectionOrDefault(t.protection).excluded(fragPath) {
		t.collector.noteSkipped()
		return core.TextOutput("skipped: path excluded by owner memory.exclude_paths; do not relocate excluded material"), nil
	}
	if err := protectionOrDefault(t.protection).fragment(payload.Path, payload.Content); err != nil {
		return core.ToolOutput{}, err
	}
	if !t.dryRun {
		if err := t.store.CreateFragment(ctx, &memorydb.Fragment{Agent: t.agent, Path: fragPath, Content: content, Source: memorydb.DefaultSourceMine}); err != nil {
			return core.ToolOutput{}, err
		}
	}
	t.collector.noteCreated(fragPath)
	return core.TextOutput(fmt.Sprintf("created %s", fragPath)), nil
}

type memoryUpdateFragmentTool struct {
	store      *memorydb.Store
	agent      string
	dryRun     bool
	collector  *memoryExtractionCollector
	protection *memoryProtection
}

func (t *memoryUpdateFragmentTool) Spec() core.ToolSpec {
	return core.ToolSpec{
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

func (t *memoryUpdateFragmentTool) Execute(ctx context.Context, args json.RawMessage) (out core.ToolOutput, err error) {
	var payload struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Reason  string `json:"reason"`
	}
	defer func() {
		if err != nil {
			t.collector.noteFailure(payload.Path, errors.Is(err, errMemorySafety))
		}
	}()

	if err := json.Unmarshal(args, &payload); err != nil {
		return core.ToolOutput{}, fmt.Errorf("parse memory_update_fragment args: %w", err)
	}
	fragPath, err := validateFragmentPath(payload.Path)
	if err != nil {
		return core.ToolOutput{}, err
	}
	content := strings.TrimSpace(payload.Content)
	if content == "" {
		return core.ToolOutput{}, fmt.Errorf("content cannot be empty")
	}
	if len([]byte(content)) > 8192 {
		return core.ToolOutput{}, fmt.Errorf("content exceeds 8192 bytes")
	}
	if protectionOrDefault(t.protection).excluded(fragPath) {
		t.collector.noteSkipped()
		return core.TextOutput("skipped: path excluded by owner memory.exclude_paths; do not relocate excluded material"), nil
	}
	if err := protectionOrDefault(t.protection).fragment(payload.Path, payload.Content); err != nil {
		return core.ToolOutput{}, err
	}
	if !t.dryRun {
		ok, err := t.store.UpdateFragment(ctx, t.agent, fragPath, content)
		if err != nil {
			return core.ToolOutput{}, err
		}
		if !ok {
			existing, err := t.store.GetFragment(ctx, t.agent, fragPath)
			if err != nil {
				return core.ToolOutput{}, err
			}
			if existing == nil {
				return core.ToolOutput{}, fmt.Errorf("fragment %s does not exist", fragPath)
			}
			if existing.Content != content {
				return core.ToolOutput{}, fmt.Errorf("fragment changed concurrently; inspect and retry")
			}
			t.collector.noteUnchanged(fragPath)
			return core.TextOutput(fmt.Sprintf("unchanged %s; corrected proposal already matches stored content", fragPath)), nil
		}
	}
	t.collector.noteUpdated(fragPath)
	return core.TextOutput(fmt.Sprintf("updated %s", fragPath)), nil
}

func registerMemoryExtractionTools(engine *engine.Engine, store *memorydb.Store, agent string, collector *memoryExtractionCollector, readBytes int, dryRun bool, protection *memoryProtection) ([]core.ToolSpec, func()) {
	tools := []core.Tool{
		&memorySearchFragmentsTool{store: store, agent: agent, readBytes: readBytes, protection: protection},
		&memoryListFragmentsTool{store: store, agent: agent, protection: protection},
		&memoryGetFragmentTool{store: store, agent: agent, readBytes: readBytes, protection: protection},
		&memoryCreateFragmentTool{store: store, agent: agent, dryRun: dryRun, collector: collector, protection: protection},
		&memoryUpdateFragmentTool{store: store, agent: agent, dryRun: dryRun, collector: collector, protection: protection},
	}
	specs := make([]core.ToolSpec, 0, len(tools))
	for _, tool := range tools {
		tool = &memoryProtectedTool{Tool: tool, protection: protection, collector: collector}
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

// Policy applies to every provider-bound tool result, not just the first turn.
type memoryProtectedTool struct {
	core.Tool
	protection *memoryProtection
	collector  *memoryExtractionCollector
}

func (t *memoryProtectedTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	out, err := t.Tool.Execute(ctx, args)

	if err != nil {
		return core.ToolOutput{}, err
	}
	out.Content = protectionOrDefault(t.protection).sanitize(out.Content)
	return out, nil
}
