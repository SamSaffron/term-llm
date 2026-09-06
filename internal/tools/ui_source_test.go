package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestUIGetSourceBoundedCompiledRead(t *testing.T) {
	tool := &UIGetSourceTool{}
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"target":"compiled","operation":"read","path":"compiled/index.html","start_line":1,"end_line":4}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), "compiled-only") || !strings.Contains(string(b), "total_lines") {
		t.Fatalf("%s", b)
	}
	if strings.Contains(string(b), "TERM_LLM_UI_PREFIX =") {
		t.Fatal("injected config exported")
	}
}
func TestUIGetSourceExtractionRequiresApproval(t *testing.T) {
	tool := &UIGetSourceTool{}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"target":"compiled","operation":"extract","path":"new-reference"}`))
	if err == nil || !strings.Contains(err.Error(), "approval") {
		t.Fatalf("%v", err)
	}
}
func TestUIExtensionsContextOnly(t *testing.T) {
	tool := &UIExtensionsTool{}
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"reload"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), "only in term-llm serve web") {
		t.Fatal(string(b))
	}
	called := ""
	tool.control = func(_ context.Context, op string) (any, error) {
		called = op
		return map[string]string{"source": "config"}, nil
	}
	if _, err = tool.Execute(context.Background(), json.RawMessage(`{"operation":"reload"}`)); err != nil || called != "reload" {
		t.Fatalf("%s %v", called, err)
	}
}

func TestUIActivateExtensionsUsesBoundControl(t *testing.T) {
	tool := &UIExtensionsTool{activate: true}
	if tool.Spec().Name != UIActivateToolName {
		t.Fatal("incorrect tool name")
	}
	called := ""
	tool.control = func(_ context.Context, op string) (any, error) {
		called = op
		return map[string]string{"activation": "requested"}, nil
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{}`)); err != nil || called != "activate" {
		t.Fatalf("%s %v", called, err)
	}
	ordinary := &UIExtensionsTool{}
	if _, err := ordinary.Execute(context.Background(), json.RawMessage(`{"operation":"activate"}`)); err == nil {
		t.Fatal("ordinary rescan accepted activation")
	}
}
