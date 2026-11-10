// Package tool adapts fresh QuickJS cells to the model-facing tool contract.
// The original synchronous code_mode remains an explicit compatibility path;
// ExecTool/WaitTool/CancelTool expose the engine-owned async lifecycle.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/codemode"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

const Name = codemode.LegacyToolName

// Tool runs a bounded async body, using only engine-admitted capabilities.
type Tool struct{}

func New() *Tool { return &Tool{} }
func (t *Tool) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name: Name,
		Description: `Run JavaScript to compose tools, filter results or use activated skill modules. Prefer direct tools for simple calls, discovery, skill activation and controls.
Put an async function body in code (no Markdown fences); await/return work. Fresh QuickJS per call: no Node, filesystem or network except through admitted tools.
- Tools: await tools[exactName](args); inspect unfamiliar schemas with text(await describeTools([names])). Native, skill-script and MCP tools retain normal permissions; MCP is optional.
- Results: native {text,isError,timedOut,content?}; MCP {content,structuredContent?,isError?}. Check error flags; bridge failures throw. Await every call; use Promise.all only for independent work.
- Output: text(value), image(imageBlockOrHandle), or return value. Intermediates stay private; return emits output, not state.
- State: store(key,value) stages JSON; load(key) reads it. Pass the successful result's state as the next call's state; nothing persists automatically. Globals and image handles expire.
- Skills: const m = await import('skill:activated-name'); use its documented exports. Host tools perform filesystem/browser work.
Example (read_file admitted): const r = await tools.read_file({path:'README.md'}); if (r.isError || r.timedOut) throw Error(r.text); text(r.text);
Limits: 120s wall default, 300s max via timeout_ms (includes approvals); 2s active JS; 64 host calls, 4 concurrent; await sleep(ms) with ms<=1000.
No external-effect rollback. Reconcile outcome_unknown before retrying actions.`,
		Schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"code": map[string]interface{}{"type": "string", "description": "Async function body; use dynamic import('skill:activated-name')"}, "state": map[string]interface{}{"type": "object", "additionalProperties": true, "description": "Explicit prior returned state, or empty object"}, "timeout_ms": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 300000}}, "required": []string{"code"}, "additionalProperties": false},
	}
}
func (t *Tool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	cell := core.CodeModeCellFromContext(ctx)
	if cell == nil {
		return core.ToolOutput{}, fmt.Errorf("code_mode unavailable: engine-owned environment required")
	}
	defer cell.Close()
	if !codemode.Available() {
		return core.ToolOutput{}, fmt.Errorf("code_mode unavailable on browser host")
	}
	var input codemode.Input
	if err := json.Unmarshal(args, &input); err != nil {
		return core.ToolOutput{}, fmt.Errorf("code_mode arguments: %w", err)
	}
	result := codemode.Run(ctx, input, cell.Names(), cell, cell)
	parts := make([]core.ToolContentPart, 0, len(result.Output)+1)
	images := 0
	imageBytes := 0
	for _, emission := range result.Output {
		if emission.Kind == "text" {
			var text string
			if json.Unmarshal(emission.Value, &text) != nil {
				text = string(emission.Value)
			}
			parts = append(parts, core.ToolContentPart{Type: core.ToolContentPartText, Text: text})
			continue
		}
		part, err := cell.Image(emission.Value)
		if err == nil {
			images++
			imageBytes += len(part.ImageData.Base64) * 3 / 4
			if images > codemode.Limits.Images || imageBytes > codemode.Limits.ImageBytes {
				err = fmt.Errorf("emitted image limit")
			}
		}
		if err != nil {
			result.Status = "failed"
			result.Error = err.Error()
			result.StateCommitted = false
			result.State = nil
			break
		}
		parts = append(parts, part)
	}
	if strings.Contains(string(result.State), "cell-image:") {
		result.Status = "failed"
		result.Error = "image handles are cell-scoped and cannot be stored in state"
		result.StateCommitted = false
		result.State = nil
	}
	// Media bytes are carried only by ordered content parts, never inside status JSON.
	for i := range result.Output {
		if result.Output[i].Kind == "image" {
			result.Output[i].Value = json.RawMessage(`"[explicit image]"`)
		}
	}
	audit := cell.Outcomes()
	projected := core.CodeModeModelCalls(audit)
	envelope := struct {
		codemode.Result
		CellID string                     `json:"cell_id"`
		Calls  []core.CodeModeCallOutcome `json:"calls,omitempty"`
	}{result, core.CallIDFromContext(ctx), projected}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return core.ToolOutput{}, fmt.Errorf("code_mode result: %w", err)
	}
	parts = append(parts, core.ToolContentPart{Type: core.ToolContentPartText, Text: "\n" + string(raw)})
	return core.ToolOutput{Content: string(raw), ContentParts: parts, ChildCalls: audit, PreserveJSON: true, IsError: result.Status != "completed"}, nil
}
func (t *Tool) Preview(json.RawMessage) string { return "bounded JavaScript cell" }
