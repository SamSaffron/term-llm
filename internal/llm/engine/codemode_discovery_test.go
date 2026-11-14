package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/term-llm/term-llm/internal/codemode"
	"github.com/term-llm/term-llm/internal/llm/core"
)

type describedCodeTool struct {
	codeTestTool
	description string
}

func (t describedCodeTool) Spec() core.ToolSpec {
	spec := t.codeTestTool.Spec()
	spec.Description = t.description
	return spec
}

func TestCodeModeDiscoveryPurposeAndCeiling(t *testing.T) {
	e := newCodeEngine()
	tool := describedCodeTool{codeTestTool: codeTestTool{name: "unfamiliar.exact-name", schema: map[string]interface{}{"type": "object"}}, description: strings.Repeat("é", 700)}
	e.RegisterTool(tool)
	e.RegisterTool(policyCodeTestTool{codeTestTool: codeTestTool{name: "ask_user"}, exposure: core.CodeModeExposure{Reason: "foreground_interaction"}})
	c := admitTestCell(t, e)
	raw, err := c.Describe(context.Background(), []string{tool.name})
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Name, Description string
		Hash              string         `json:"schema_hash"`
		OutputSchema      any            `json:"outputSchema"`
		InputSchema       map[string]any `json:"inputSchema"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != tool.name || entries[0].Description != strings.Repeat("é", 512) || !utf8.ValidString(entries[0].Description) || entries[0].Hash == "" || entries[0].OutputSchema != nil || entries[0].InputSchema["type"] != "object" {
		t.Fatalf("%s", raw)
	}
	for _, name := range []string{"ask_user", "missing"} {
		if _, err := c.Describe(context.Background(), []string{name}); err == nil {
			t.Fatalf("described %s", name)
		}
	}
	tool.description = "changed beyond captured identity"
	e.RegisterTool(tool)
	if _, err := c.Describe(context.Background(), []string{tool.name}); err == nil || !strings.Contains(err.Error(), "schema_changed") {
		t.Fatal(err)
	}
	e.SetAllowedToolsFilter([]string{})
	if _, err := c.Describe(context.Background(), []string{tool.name}); err == nil {
		t.Fatal("revoked tool described")
	}
}

func TestCodeModeDiscoveryBounds(t *testing.T) {
	e := newCodeEngine()
	e.RegisterTool(codeTestTool{name: "huge", schema: map[string]interface{}{"description": strings.Repeat("x", codemode.Limits.Arguments)}})
	c := admitTestCell(t, e)
	if _, err := c.Describe(context.Background(), []string{"huge"}); err == nil || !strings.Contains(err.Error(), "description_limit") {
		t.Fatalf("large schema: %v", err)
	}
	if _, err := c.Describe(context.Background(), make([]string, codemode.Limits.Calls+1)); err == nil || !strings.Contains(err.Error(), "description_limit") {
		t.Fatalf("large roster: %v", err)
	}
}
