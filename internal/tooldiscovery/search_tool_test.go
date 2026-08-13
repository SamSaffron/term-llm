package tooldiscovery

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeSearchInputSelectorHandling(t *testing.T) {
	tests := []struct {
		name      string
		args      string
		wantQuery string
		wantNames []string
		wantErr   string
	}{
		{
			name:      "query only",
			args:      `{"query":" list workflows "}`,
			wantQuery: "list workflows",
		},
		{
			name:      "exact names only",
			args:      `{"tool_names":[" discourse_list_workflows ",""]}`,
			wantNames: []string{"discourse_list_workflows"},
		},
		{
			name:      "exact names take precedence over redundant query",
			args:      `{"query":"list workflows","tool_names":["discourse_list_workflows"]}`,
			wantNames: []string{"discourse_list_workflows"},
		},
		{
			name:    "neither selector",
			args:    `{"query":" ","tool_names":[""]}`,
			wantErr: "provide a non-empty query or non-empty tool_names",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeSearchInput(json.RawMessage(tt.args))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("decodeSearchInput() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeSearchInput() error = %v", err)
			}
			if got.Query != tt.wantQuery {
				t.Errorf("Query = %q, want %q", got.Query, tt.wantQuery)
			}
			if strings.Join(got.ToolNames, ",") != strings.Join(tt.wantNames, ",") {
				t.Errorf("ToolNames = %q, want %q", got.ToolNames, tt.wantNames)
			}
		})
	}
}

func TestSearchToolSpecExplainsSelectorChoice(t *testing.T) {
	spec := (&SearchTool{}).Spec()
	if !strings.Contains(spec.Description, "If both are supplied") || !strings.Contains(spec.Description, "tool_names takes precedence") {
		t.Fatalf("tool description does not explain selector precedence: %q", spec.Description)
	}

	properties, ok := spec.Schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties schema type = %T", spec.Schema["properties"])
	}
	for _, property := range []string{"query", "tool_names"} {
		propertySchema, ok := properties[property].(map[string]any)
		if !ok {
			t.Fatalf("%s schema type = %T", property, properties[property])
		}
		description, _ := propertySchema["description"].(string)
		if !strings.Contains(description, "Do not supply both") {
			t.Errorf("%s description does not explain selector choice: %q", property, description)
		}
	}
}

func TestSearchToolPreviewPrefersExactNames(t *testing.T) {
	preview := (&SearchTool{}).Preview(json.RawMessage(`{"query":"list workflows","tool_names":["discourse_list_workflows"]}`))
	if want := "Loading MCP tools: discourse_list_workflows"; preview != want {
		t.Fatalf("Preview() = %q, want %q", preview, want)
	}
}
