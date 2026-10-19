package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestSetOutputToolLegacyParameter(t *testing.T) {
	tool := NewSetOutputTool("submit_result", "", "Submit the result", nil)
	wantSchema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"content": map[string]interface{}{
				"type":        "string",
				"description": "The output content",
			},
		},
		"required":             []string{"content"},
		"additionalProperties": false,
	}
	if got := tool.Spec().Schema; !reflect.DeepEqual(got, wantSchema) {
		t.Fatalf("Spec().Schema = %#v, want %#v", got, wantSchema)
	}

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"content":"done"}`)); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := tool.Value(); got != "done" {
		t.Fatalf("Value() = %q, want %q", got, "done")
	}
	if !tool.Captured() {
		t.Fatal("Captured() = false, want true")
	}
}

func TestSetOutputToolTypedSchemaCapturesCompleteObject(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"summary": map[string]interface{}{"type": "string"},
			"score":   map[string]interface{}{"type": "number"},
			"tags": map[string]interface{}{
				"type":  "array",
				"items": map[string]interface{}{"type": "string"},
			},
		},
		"required":             []string{"summary", "score"},
		"additionalProperties": false,
	}
	tool := NewSetOutputTool("submit_result", "", "Submit the result", schema)
	if got := tool.Spec().Schema; !reflect.DeepEqual(got, schema) {
		t.Fatalf("Spec().Schema = %#v, want %#v", got, schema)
	}

	args := json.RawMessage(`{
		"summary": "done",
		"score": 0.75,
		"tags": ["go", "tests"]
	}`)
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	want := `{"summary":"done","score":0.75,"tags":["go","tests"]}`
	if got := tool.Value(); got != want {
		t.Fatalf("Value() = %q, want %q", got, want)
	}
	if !tool.Captured() {
		t.Fatal("Captured() = false, want true")
	}
}

func TestSetOutputToolTypedSchemaRejectsNonObjects(t *testing.T) {
	for _, args := range []string{`null`, `[]`, `"text"`} {
		t.Run(args, func(t *testing.T) {
			tool := NewSetOutputTool("submit_result", "", "Submit the result", map[string]interface{}{"type": "object"})
			if _, err := tool.Execute(context.Background(), json.RawMessage(args)); err == nil {
				t.Fatal("Execute() error = nil, want error")
			}
			if tool.Captured() {
				t.Fatal("Captured() = true after invalid arguments")
			}
		})
	}
}

func TestSetOutputToolReset(t *testing.T) {
	for _, typed := range []bool{false, true} {
		t.Run(fmt.Sprint("typed=", typed), func(t *testing.T) {
			var schema map[string]interface{}
			if typed {
				schema = map[string]interface{}{"type": "object"}
			}
			tool := NewSetOutputTool("submit_result", "", "Submit the result", schema)
			if _, err := tool.Execute(context.Background(), json.RawMessage(`{"content":"prior turn"}`)); err != nil {
				t.Fatal(err)
			}
			if !tool.Captured() || tool.Value() == "" {
				t.Fatal("output was not captured")
			}
			tool.Reset()
			if tool.Captured() || tool.Value() != "" {
				t.Fatalf("stale output after reset: captured=%v value=%q", tool.Captured(), tool.Value())
			}
			if _, err := tool.Execute(context.Background(), json.RawMessage(`{"content":"next turn"}`)); err != nil {
				t.Fatal(err)
			}
			if !tool.Captured() || !strings.Contains(tool.Value(), "next turn") {
				t.Fatalf("capture after reset: %q", tool.Value())
			}
		})
	}
}
