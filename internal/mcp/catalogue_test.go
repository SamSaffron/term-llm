package mcp

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

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
	if !first.Tools[0].Annotations.Present || !first.Tools[0].Annotations.ReadOnly {
		t.Fatalf("annotations were not preserved: %#v", first.Tools[0].Annotations)
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
	if changed.Tools[0].Annotations.Present {
		t.Fatalf("missing SDK annotations were marked present: %#v", changed.Tools[0].Annotations)
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

func TestNormalizeCatalogueIdentitiesPreservesCommonFlatteningAndSeparatesCollisions(t *testing.T) {
	tools := normalizeCatalogueIdentities([]CatalogTool{
		{Server: "alpha", OriginalName: "lookup", Name: "alpha__lookup", InputSchema: map[string]any{}},
		{Server: "beta", OriginalName: "lookup", Name: "beta__lookup", InputSchema: map[string]any{}},
		{Server: "a", OriginalName: "b__c", Name: "a__b__c", InputSchema: map[string]any{}},
		{Server: "a__b", OriginalName: "c", Name: "a__b__c", InputSchema: map[string]any{}},
	})
	byIdentity := make(map[string]CatalogTool, len(tools))
	seenNames := make(map[string]bool, len(tools))
	for _, tool := range tools {
		byIdentity[catalogIdentity(tool)] = tool
		if seenNames[tool.Name] {
			t.Fatalf("duplicate executable name %q", tool.Name)
		}
		seenNames[tool.Name] = true
	}
	alpha := byIdentity["alpha\x00lookup"]
	beta := byIdentity["beta\x00lookup"]
	if alpha.Name != "alpha__lookup" || beta.Name != "beta__lookup" {
		t.Fatalf("common flattened names changed: alpha=%q beta=%q", alpha.Name, beta.Name)
	}
	if alpha.ChildName != "lookup" || beta.ChildName != "lookup" || alpha.Namespace == beta.Namespace {
		t.Fatalf("same child across namespaces not represented explicitly: alpha=%+v beta=%+v", alpha, beta)
	}
	first := byIdentity["a\x00b__c"]
	second := byIdentity["a__b\x00c"]
	if first.Name == second.Name || first.Name == "a__b__c" || second.Name == "a__b__c" {
		t.Fatalf("delimiter collision was not resolved symmetrically: %q %q", first.Name, second.Name)
	}
}

func TestNormalizeCatalogueIdentitiesResolvesSanitizationCollisionsDeterministically(t *testing.T) {
	input := []CatalogTool{
		{Server: "sales force", OriginalName: "find.issue", Name: "sales force__find.issue", InputSchema: map[string]any{}},
		{Server: "sales@force", OriginalName: "find@issue", Name: "sales@force__find@issue", InputSchema: map[string]any{}},
		{Server: "sales@force", OriginalName: "find.issue", Name: "sales@force__find.issue", InputSchema: map[string]any{}},
	}
	first := normalizeCatalogueIdentities(append([]CatalogTool(nil), input...))
	second := normalizeCatalogueIdentities([]CatalogTool{input[2], input[0], input[1]})
	project := func(tools []CatalogTool) map[string]string {
		out := make(map[string]string, len(tools))
		for _, tool := range tools {
			if len(tool.Namespace) > maxNativeIdentityBytes || len(tool.ChildName) > maxNativeIdentityBytes {
				t.Fatalf("native identity exceeds cap: %+v", tool)
			}
			out[catalogIdentity(tool)] = tool.Namespace + "/" + tool.ChildName
		}
		return out
	}
	if !reflect.DeepEqual(project(first), project(second)) {
		t.Fatalf("identity normalization depends on input order: first=%v second=%v", project(first), project(second))
	}
	if first[0].Namespace == first[1].Namespace {
		t.Fatalf("sanitized namespace collision survived: %+v", first)
	}
	if first[1].ChildName == first[2].ChildName {
		t.Fatalf("sanitized child collision survived: %+v", first)
	}
}

func TestNamespaceDescriptionUsesMetadataAndSmallUTF8SafeCap(t *testing.T) {
	description := strings.Repeat("é", MaxNamespaceDescriptionBytes)
	snapshot, err := buildToolSnapshotWithDescription("demo", 1, description, []*sdkmcp.Tool{{Name: "lookup", InputSchema: map[string]any{}}})
	if err != nil {
		t.Fatal(err)
	}
	tool := normalizeCatalogueIdentities([]CatalogTool{namespaceCatalogTool("demo", snapshot.NamespaceDescription, snapshot.Tools[0])})[0]
	if len(tool.NamespaceDescription) != MaxNamespaceDescriptionBytes || !utf8.ValidString(tool.NamespaceDescription) {
		t.Fatalf("description bytes=%d valid=%v", len(tool.NamespaceDescription), utf8.ValidString(tool.NamespaceDescription))
	}
	if tool.ToolSpec().Namespace == nil || tool.ToolSpec().Namespace.Description != tool.NamespaceDescription {
		t.Fatalf("namespace description missing from provider-neutral spec: %#v", tool.ToolSpec())
	}
}

func TestMCPNamespaceDescriptionPrefersInstructionsThenServerMetadata(t *testing.T) {
	if got := mcpNamespaceDescription(&sdkmcp.InitializeResult{Instructions: "  Use issue keys.  ", ServerInfo: &sdkmcp.Implementation{Name: "fallback"}}); got != "Use issue keys." {
		t.Fatalf("instructions description = %q", got)
	}
	if got := mcpNamespaceDescription(&sdkmcp.InitializeResult{ServerInfo: &sdkmcp.Implementation{Name: "machine", Title: "Issue Tracker"}}); got != "Tools provided by Issue Tracker." {
		t.Fatalf("metadata description = %q", got)
	}
}
