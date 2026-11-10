package mcp

import (
	"context"
	"errors"
	"testing"
	"time"
)

// makeCatalogTool builds a minimal CatalogTool for use in CodeModeIdentity tests.
// server and originalName are the per-server identity; the flattened Name uses
// the same prefix convention that namespaceCatalogTool produces.
func makeCatalogTool(server, originalName, schemaHash string) CatalogTool {
	return CatalogTool{
		ID:           originalName,
		Server:       server,
		OriginalName: originalName,
		// Name stored in per-server snapshot is the original (bare) name before
		// aggregation; the aggregate produced by publishAggregateLocked prefixes it.
		Name:        originalName,
		Description: "test tool",
		SchemaHash:  schemaHash,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

// makePerServerSnapshot builds a minimal ToolSnapshot for a single tool.
func makePerServerSnapshot(ct CatalogTool) *ToolSnapshot {
	return &ToolSnapshot{
		Generation: 1,
		FetchedAt:  time.Now(),
		Tools:      []CatalogTool{ct},
		ByOriginal: map[string]int{ct.OriginalName: 0},
		Hash:       "testhash",
	}
}

// managerWithCatalogueTools returns a manager whose per-server catalogue and
// aggregate are populated directly (no live MCP process required). The aggregate
// contains the namespaced (server__original) name that real usage would produce.
func managerWithCatalogueTools(server string, tools []CatalogTool) *Manager {
	m := NewManager()
	m.mu.Lock()
	if m.catalogues == nil {
		m.catalogues = make(map[string]*ToolSnapshot)
	}
	snap := &ToolSnapshot{
		Generation: 1,
		FetchedAt:  time.Now(),
		Tools:      make([]CatalogTool, len(tools)),
		ByOriginal: make(map[string]int, len(tools)),
		Hash:       "testhash",
	}
	for i, ct := range tools {
		snap.Tools[i] = ct
		snap.ByOriginal[ct.OriginalName] = i
	}
	m.catalogues[server] = snap
	// Build aggregate with namespaced names (mirrors what publishAggregateLocked does).
	aggTools := make([]CatalogTool, len(tools))
	for i, ct := range tools {
		aggTools[i] = namespaceCatalogTool(server, "", ct)
	}
	aggTools = normalizeCatalogueIdentities(aggTools)
	agg := &CatalogueSnapshot{
		Generation: 1,
		FetchedAt:  time.Now(),
		Tools:      aggTools,
		Hash:       "agghash",
	}
	m.aggregate.Store(agg)
	m.mu.Unlock()
	return m
}

func TestCodeModeIdentity_CatalogueTool_Present(t *testing.T) {
	const server = "myserver"
	const original = "search"
	const wantHash = "abc123"

	ct := makeCatalogTool(server, original, wantHash)
	m := managerWithCatalogueTools(server, []CatalogTool{ct})

	// NewCatalogMCPTool expects a tool already in the namespaced form; build one
	// whose server and originalName fields are set for the catalogue path.
	tool := &MCPTool{
		manager:      m,
		server:       server,
		originalName: original,
		toolSpec:     ct.ToolSpec(),
	}

	spec, hash, err := tool.CodeModeIdentity(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hash != wantHash {
		t.Errorf("hash = %q, want %q", hash, wantHash)
	}
	if spec.Name != ct.Name {
		t.Errorf("spec.Name = %q, want %q", spec.Name, ct.Name)
	}
}

func TestCodeModeIdentity_CatalogueTool_Removed(t *testing.T) {
	const server = "myserver"
	const original = "search"

	ct := makeCatalogTool(server, original, "hash")
	m := managerWithCatalogueTools(server, []CatalogTool{ct})

	// Tool whose originalName is not in the per-server snapshot.
	removedTool := &MCPTool{
		manager:      m,
		server:       server,
		originalName: "gone_tool",
		toolSpec:     ct.ToolSpec(),
	}

	_, _, err := removedTool.CodeModeIdentity(context.Background())
	if !errors.Is(err, ErrToolRemovedFromCatalogue) {
		t.Fatalf("expected ErrToolRemovedFromCatalogue, got %v", err)
	}
}

func TestCodeModeIdentity_CatalogueTool_MissingSnapshot(t *testing.T) {
	const server = "nosnapshot"
	m := NewManager()
	// No catalogue entry for this server.

	tool := &MCPTool{
		manager:      m,
		server:       server,
		originalName: "some_tool",
	}

	_, _, err := tool.CodeModeIdentity(context.Background())
	if err == nil {
		t.Fatal("expected error for missing server catalogue, got nil")
	}
	// Should not be ErrToolRemovedFromCatalogue; it's a "not ready" error.
	if errors.Is(err, ErrToolRemovedFromCatalogue) {
		t.Errorf("missing snapshot should not return ErrToolRemovedFromCatalogue")
	}
}

func TestCodeModeIdentity_LegacyTool_Present(t *testing.T) {
	const server = "myserver"
	const original = "search"
	const wantHash = "hashXYZ"

	ct := makeCatalogTool(server, original, wantHash)
	m := managerWithCatalogueTools(server, []CatalogTool{ct})

	// The aggregate name after namespacing is "myserver__search".
	agg := m.CatalogueSnapshot()
	if agg == nil || len(agg.Tools) == 0 {
		t.Fatal("aggregate catalogue not populated")
	}
	aggName := agg.Tools[0].Name // e.g. "myserver__search"

	// Legacy tool: no server/originalName set, only the flattened projected name.
	legacyTool := NewMCPTool(m, ToolSpec{
		Name:        aggName,
		Description: "legacy",
		Schema:      ct.InputSchema,
	})

	spec, hash, err := legacyTool.CodeModeIdentity(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The aggregate SchemaHash is recomputed by finalizeCatalogTool from the
	// actual schema; verify it is non-empty rather than a fixed value.
	if hash == "" {
		t.Error("expected non-empty hash from aggregate tool")
	}
	_ = wantHash // wantHash applies to the per-server snapshot, not the aggregate
	if spec.Name != aggName {
		t.Errorf("spec.Name = %q, want %q", spec.Name, aggName)
	}
}

func TestCodeModeIdentity_LegacyTool_Removed(t *testing.T) {
	const server = "myserver"
	const original = "search"

	ct := makeCatalogTool(server, original, "hash")
	m := managerWithCatalogueTools(server, []CatalogTool{ct})

	// Legacy tool whose flattened name does not exist in the aggregate.
	legacyTool := NewMCPTool(m, ToolSpec{
		Name:   "myserver__nonexistent",
		Schema: map[string]any{},
	})

	_, _, err := legacyTool.CodeModeIdentity(context.Background())
	if !errors.Is(err, ErrToolRemovedFromCatalogue) {
		t.Fatalf("expected ErrToolRemovedFromCatalogue for missing legacy tool, got %v", err)
	}
}

func TestCodeModeIdentity_LegacyTool_NoCatalogue(t *testing.T) {
	m := NewManager()
	// aggregate is nil (no servers configured)
	legacyTool := NewMCPTool(m, ToolSpec{Name: "foo__bar", Schema: map[string]any{}})
	_, _, err := legacyTool.CodeModeIdentity(context.Background())
	// Either a "not ready" error or ErrToolRemovedFromCatalogue is acceptable when
	// the aggregate is empty; must not panic or return nil error.
	if err == nil {
		t.Fatal("expected non-nil error when catalogue empty")
	}
}
