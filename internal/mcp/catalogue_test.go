package mcp

import (
	"reflect"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBuildToolSnapshotCanonicalHashAndMetadata(t *testing.T) {
	readOnly := true
	first, err := buildToolSnapshot("demo", 1, []*sdkmcp.Tool{{
		Name:        "find_issue",
		Description: "Find an issue",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"description": "Issue query", "type": "string"},
			},
		},
		OutputSchema: map[string]any{"properties": map[string]any{"id": map[string]any{"type": "string"}}, "type": "object"},
		Annotations:  &sdkmcp.ToolAnnotations{ReadOnlyHint: readOnly, Title: "Find issue"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildToolSnapshot("demo", 2, []*sdkmcp.Tool{{
		Name:        "find_issue",
		Description: "Find an issue",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Issue query"},
			},
			"type": "object",
		},
		OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}},
		Annotations:  &sdkmcp.ToolAnnotations{Title: "Find issue", ReadOnlyHint: readOnly},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash || first.Tools[0].SchemaHash != second.Tools[0].SchemaHash {
		t.Fatalf("canonical hashes differ: snapshot %q/%q tool %q/%q", first.Hash, second.Hash, first.Tools[0].SchemaHash, second.Tools[0].SchemaHash)
	}
	if first.Tools[0].EstimatedTokens <= 0 {
		t.Fatal("expected positive schema token estimate")
	}
	if first.Tools[0].ToolSpec().OutputSchema == nil {
		t.Fatal("output schema was not preserved")
	}

	changed, err := buildToolSnapshot("demo", 3, []*sdkmcp.Tool{{
		Name: "find_issue", Description: "Find and inspect an issue", InputSchema: first.Tools[0].InputSchema,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Tools[0].SchemaHash == first.Tools[0].SchemaHash {
		t.Fatal("description change did not alter schema hash")
	}
}

func TestBuildToolSnapshotRejectsDuplicateAndInvalidSchema(t *testing.T) {
	if _, err := buildToolSnapshot("demo", 1, []*sdkmcp.Tool{
		{Name: "same", InputSchema: map[string]any{}},
		{Name: "same", InputSchema: map[string]any{}},
	}); err == nil {
		t.Fatal("duplicate tool names were accepted")
	}
	if _, err := buildToolSnapshot("demo", 1, []*sdkmcp.Tool{{Name: "bad", InputSchema: []any{"not", "object"}}}); err == nil {
		t.Fatal("non-object input schema was accepted")
	}
}

func TestClientToolsReturnsCopySafeProjection(t *testing.T) {
	snapshot, err := buildToolSnapshot("demo", 1, []*sdkmcp.Tool{{
		Name: "safe", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient("demo", ServerConfig{})
	client.snapshot.Store(snapshot)
	first := client.Tools()
	first[0].Name = "mutated"
	first[0].Schema["type"] = "array"
	second := client.Tools()
	if second[0].Name != "safe" || !reflect.DeepEqual(second[0].Schema["type"], "object") {
		t.Fatalf("snapshot was mutated through Tools projection: %#v", second[0])
	}
}

func TestClientDoesNotPublishRefreshForStoppedSession(t *testing.T) {
	client := NewClient("demo", ServerConfig{})
	session := &sdkmcp.ClientSession{}
	client.mu.Lock()
	client.session = session
	client.running = false
	client.mu.Unlock()
	candidate := &ToolSnapshot{Generation: 2, Tools: []CatalogTool{{Name: "stale"}}}
	if client.publishRefresh(session, candidate) {
		t.Fatal("stopped session published a refresh")
	}
	if snapshot := client.snapshot.Load(); snapshot != nil {
		t.Fatalf("stopped session resurrected snapshot: %#v", snapshot)
	}
}
