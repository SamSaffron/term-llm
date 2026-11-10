package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/codemode"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

const (
	// ExecName starts a fresh engine-owned asynchronous cell.
	ExecName = codemode.ExecToolName
	// WaitName observes more output or completion without cancelling.
	WaitName = codemode.WaitToolName
	// CancelName is the only tool-initiated lifecycle stop.
	CancelName = codemode.CancelToolName
)

// ControlToolNames returns the async lifecycle surface in registration order.
func ControlToolNames() []string { return []string{ExecName, WaitName, CancelName} }

// IsControl reports whether name belongs to the async lifecycle surface.
func IsControl(name string) bool {
	return name == ExecName || name == WaitName || name == CancelName
}

// ExecTool starts a fresh QuickJS cell and returns its first observation.
type ExecTool struct{}

func NewExec() *ExecTool { return &ExecTool{} }

func (t *ExecTool) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name: ExecName,
		Description: `Start a fresh QuickJS cell for multi-step JavaScript that composes tools or activated skill modules. Prefer direct tools for one simple call.
Put an async function body in code (no Markdown fences). Fresh QuickJS per cell: no Node, filesystem or network except admitted tools. Native, skill-script and MCP tools keep normal permissions and approvals. Await every call; use Promise.all only for independent work.
- Output: text(value), image(handleOrBlock), or return value. Intermediates stay private. Each spawn_script/wait_script observation returns at most max_output_tokens (default 10000, 4 bytes per estimated token); withheld text stays queued for a later wait_script and permanently over-cap output is truncated and reported.
- State: load(key)/store(key,value) use engine/session memory, committed automatically only when the cell completes successfully. Limits: 1 MiB total JSON, 256 KiB per value. Failed or cancelled cells roll back staged state. This is process-local state, not durable session storage.
- Lifecycle: returns when the cell finishes, emits output, or the max_wait budget in seconds (default 60, maximum 600) expires. Set max_wait to 0 to poll without waiting. "yielded" or "running" means the cell is still active; call wait_script with the returned cell_id. Wait expiry and request cancellation do not stop the cell; use cancel_script explicitly.
- Limits: 256 MiB JS heap; 2 s active JS per cell (waiting for tools or sleep is excluded); 64 host calls, 4 concurrent; await sleep(ms) with ms<=1000.
Image handles are limited to 64 and 16 MiB per cell; use fresh cells for long screenshot workflows. Global output/compaction limits may lower an observation budget. No external-effect rollback or automatic replay; reconcile outcome_unknown actions.`,
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"code":              map[string]interface{}{"type": "string", "description": "Async function body; use dynamic import('skill:activated-name')"},
				"max_wait":          map[string]interface{}{"type": "integer", "minimum": 0, "maximum": 600, "description": "Wait budget in seconds; default 60; 0 polls without waiting"},
				"max_output_tokens": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": codemode.MaxOutputTokens, "description": "Visible text budget for this observation; default 10000"},
			},
			"required":             []string{"code"},
			"additionalProperties": false,
		},
	}
}

func (t *ExecTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	controller, err := controlController(ctx, ExecName)
	if err != nil {
		return core.ToolOutput{}, err
	}
	var input struct {
		Code            string `json:"code"`
		MaxWait         *int   `json:"max_wait"`
		MaxOutputTokens int    `json:"max_output_tokens"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return core.ToolOutput{}, fmt.Errorf("%s arguments: %w", ExecName, err)
	}
	yield, err := resolveYield(input.MaxWait)
	if err != nil {
		return core.ToolOutput{}, err
	}
	outputTokens, err := resolveOutputTokens(input.MaxOutputTokens)
	if err != nil {
		return core.ToolOutput{}, err
	}
	result, err := controller.ExecCodeMode(ctx, input.Code, yield, outputTokens)
	if err != nil {
		return core.ToolOutput{}, fmt.Errorf("%s: %w", ExecName, err)
	}
	return renderControl(ctx, result), nil
}

func (t *ExecTool) Preview(json.RawMessage) string { return "fresh async JavaScript cell" }

// WaitTool observes more output or the terminal state of an existing cell.
type WaitTool struct{}

func NewWait() *WaitTool { return &WaitTool{} }

func (t *WaitTool) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name: WaitName,
		Description: `Wait for a code cell created by spawn_script and return output not yet observed by a previous spawn_script/wait_script/cancel_script response.
A wait_script timeout (max_wait in seconds, default 60, maximum 600; 0 polls without waiting) is not cancellation: status "yielded" means the cell is still running and may be waited on again. Use cancel_script to stop it.
max_output_tokens caps only this observation's visible text (default 10000); withheld text stays queued for another wait_script. cell_id is the ID from the spawn_script receipt. A completed, failed or cancelled cell keeps its terminal status; an unknown or expired cell_id returns status "unknown".`,
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"cell_id":           map[string]interface{}{"type": "string", "description": "cell_id from a spawn_script receipt"},
				"max_wait":          map[string]interface{}{"type": "integer", "minimum": 0, "maximum": 600, "description": "Wait budget in seconds; default 60; 0 polls without waiting"},
				"max_output_tokens": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": codemode.MaxOutputTokens, "description": "Visible text budget for this observation; default 10000"},
			},
			"required":             []string{"cell_id"},
			"additionalProperties": false,
		},
	}
}

func (t *WaitTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	controller, err := controlController(ctx, WaitName)
	if err != nil {
		return core.ToolOutput{}, err
	}
	var input struct {
		CellID          string `json:"cell_id"`
		MaxWait         *int   `json:"max_wait"`
		MaxOutputTokens int    `json:"max_output_tokens"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return core.ToolOutput{}, fmt.Errorf("%s arguments: %w", WaitName, err)
	}
	if strings.TrimSpace(input.CellID) == "" {
		return core.ToolOutput{}, fmt.Errorf("%s: cell_id is required", WaitName)
	}
	yield, err := resolveYield(input.MaxWait)
	if err != nil {
		return core.ToolOutput{}, err
	}
	outputTokens, err := resolveOutputTokens(input.MaxOutputTokens)
	if err != nil {
		return core.ToolOutput{}, err
	}
	result, err := controller.WaitCodeMode(ctx, input.CellID, yield, outputTokens)
	if err != nil {
		return core.ToolOutput{}, fmt.Errorf("%s: %w", WaitName, err)
	}
	return renderControl(ctx, result), nil
}

func (t *WaitTool) Preview(json.RawMessage) string { return "wait for async JavaScript cell" }

// CancelTool explicitly stops a cell and reports any unresolved effects.
type CancelTool struct{}

func NewCancel() *CancelTool { return &CancelTool{} }

func (t *CancelTool) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name: CancelName,
		Description: `Explicitly cancel_script a code cell created by spawn_script. Request or wait_script cancellation does not do this.
Returns the terminal status and any output not yet observed, capped like an observation (default 10000 estimated tokens). In-flight external calls that have not settled are reported outcome_unknown and are never replayed; their actual-effect leases stay quarantined until they finish. Staged state is discarded. Cancelling an already completed, failed or cancelled cell returns its existing terminal status.`,
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"cell_id": map[string]interface{}{"type": "string", "description": "cell_id from a spawn_script receipt"},
			},
			"required":             []string{"cell_id"},
			"additionalProperties": false,
		},
	}
}

func (t *CancelTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	controller, err := controlController(ctx, CancelName)
	if err != nil {
		return core.ToolOutput{}, err
	}
	var input struct {
		CellID string `json:"cell_id"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return core.ToolOutput{}, fmt.Errorf("%s arguments: %w", CancelName, err)
	}
	if strings.TrimSpace(input.CellID) == "" {
		return core.ToolOutput{}, fmt.Errorf("%s: cell_id is required", CancelName)
	}
	result, err := controller.CancelCodeMode(ctx, input.CellID)
	if err != nil {
		return core.ToolOutput{}, fmt.Errorf("%s: %w", CancelName, err)
	}
	// A cancelled cell is the intended outcome of a successful cancel operation.
	if result.Status == "cancelled" {
		result.IsError = false
	}
	return renderControl(ctx, result), nil
}

func (t *CancelTool) Preview(json.RawMessage) string { return "cancel async JavaScript cell" }

func controlController(ctx context.Context, name string) (core.CodeModeController, error) {
	controller := core.CodeModeControllerFromContext(ctx)
	if controller == nil {
		return nil, fmt.Errorf("%s unavailable: engine-owned async cell controller required", name)
	}
	return controller, nil
}

func resolveYield(maxWait *int) (time.Duration, error) {
	seconds := 60
	if maxWait != nil {
		seconds = *maxWait
	}
	if seconds < 0 || seconds > 600 {
		return 0, fmt.Errorf("max_wait must be between 0 and 600")
	}
	return time.Duration(seconds) * time.Second, nil
}

func resolveOutputTokens(tokens int) (int, error) {
	if tokens < 0 || tokens > codemode.MaxOutputTokens {
		return 0, fmt.Errorf("max_output_tokens must be between 1 and %d", codemode.MaxOutputTokens)
	}
	if tokens == 0 {
		return codemode.DefaultOutputTokens, nil
	}
	return tokens, nil
}

// renderControl keeps visible output as ordered content parts and appends one
// machine-readable envelope. The envelope never contains session state values;
// state is observed by the next cell through load(). Media bytes live only in
// the ordered image parts.
func renderControl(ctx context.Context, result core.CodeModeCallResult) core.ToolOutput {
	parts := append([]core.ToolContentPart(nil), result.Parts...)
	envelope := core.CodeModeEnvelopeFor(result, core.CallIDFromContext(ctx))
	raw, err := core.MarshalCodeModeJSON(envelope)
	if err != nil {
		raw = []byte(`{"status":"failed","error":"result_encoding","state_committed":false}`)
	}
	parts = append(parts, core.ToolContentPart{Type: core.ToolContentPartText, Text: "\n" + string(raw)})
	return core.ToolOutput{
		Content:      string(raw),
		ContentParts: parts,
		ChildCalls:   result.Calls,
		PreserveJSON: true,
		IsError:      result.IsError,
	}
}
