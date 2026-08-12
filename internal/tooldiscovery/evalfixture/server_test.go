package evalfixture

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSimulatedToolReturnsOracleInStructuredAndTextOutput(t *testing.T) {
	root := t.TempDir()
	if err := InitializeWorkspace(root); err != nil {
		t.Fatal(err)
	}
	state := &fixtureState{root: root, server: "source_control"}
	definition, ok := FindDomain("source_control")
	if !ok {
		t.Fatal("source_control domain missing")
	}
	result, err := state.call(context.Background(), definition.Tools[4], json.RawMessage(`{"id":"42"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := OracleValue("source_control", "get_pull_request")
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok || structured["oracle"] != want {
		t.Fatalf("structured oracle = %#v, want %q", result.StructuredContent, want)
	}
	if len(result.Content) != 1 {
		t.Fatalf("content parts = %d, want 1", len(result.Content))
	}
	text, ok := result.Content[0].(*sdkmcp.TextContent)
	if !ok || !strings.Contains(text.Text, `"oracle":"`+want+`"`) {
		t.Fatalf("text-compatible output = %#v, want oracle %q", result.Content[0], want)
	}
	schema := realisticOutputSchema()
	properties := schema["properties"].(map[string]any)
	if _, ok := properties["oracle"]; !ok {
		t.Fatal("output schema is missing oracle")
	}
	required := schema["required"].([]string)
	if !containsString(required, "oracle") {
		t.Fatalf("required output fields = %v, want oracle", required)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
