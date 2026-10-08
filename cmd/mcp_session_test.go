package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestApplyInputSchemaToArgsKeepsStringsRaw(t *testing.T) {
	call := mcpToolCall{
		name: "browser_type",
		args: map[string]any{"text": int64(1815), "slowly": true, "count": int64(3), "ref": "e5"},
		raw:  map[string]string{"text": "1815", "slowly": "true", "count": "3", "ref": "e5"},
	}
	schema := map[string]any{"properties": map[string]any{
		"text":   map[string]any{"type": "string"},
		"slowly": map[string]any{"type": "boolean"},
		"count":  map[string]any{"type": []any{"integer", "string"}},
		"target": map[string]any{"type": "string"},
	}}
	var warn bytes.Buffer
	applyInputSchemaToArgs(&warn, &call, schema)
	if call.args["text"] != "1815" {
		t.Fatalf("string property should keep raw text, got %#v", call.args["text"])
	}
	if call.args["slowly"] != true || call.args["count"] != int64(3) {
		t.Fatalf("non-string properties must keep detected types: %#v", call.args)
	}
	if !strings.Contains(warn.String(), "ref") || !strings.Contains(warn.String(), "valid arguments: count, slowly, target, text") {
		t.Fatalf("expected unknown-argument hint, got %q", warn.String())
	}
}
