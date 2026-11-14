package skills

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/term-llm/term-llm/internal/llm/core"
)

// ProjectScriptToolBinding exposes the wrapped script and its project trust
// predicate so a host can rebuild an equivalent snapshot-bound instance without
// weakening the gate. The returned predicate is the one registered with the
// wrapper; hosts that need frozen authority should supply their own equivalent.
func ProjectScriptToolBinding(tool core.Tool) (inner core.Tool, yoloEnabled func() bool, ok bool) {
	gated, ok := tool.(*projectScriptTool)
	if !ok || gated == nil {
		return nil, nil, false
	}
	return gated.Tool, gated.yoloEnabled, true
}

// The wrapper embeds core.Tool, which promotes only the base interface. Every
// optional contract that gates admission, ownership or finishing must be
// forwarded explicitly; otherwise the wrapper silently loses it.

func (t *projectScriptTool) CodeModeExposure() core.CodeModeExposure {
	if provider, ok := t.Tool.(core.CodeModeExposureProvider); ok {
		return provider.CodeModeExposure()
	}
	return core.CodeModeExposure{Reason: "undeclared_exposure"}
}

func (t *projectScriptTool) IsFinishingTool() bool {
	finishing, ok := t.Tool.(core.FinishingTool)
	return ok && finishing.IsFinishingTool()
}

func (t *projectScriptTool) IsCodeModeLifecycleTool() bool {
	control, ok := t.Tool.(core.CodeModeLifecycleTool)
	return ok && control.IsCodeModeLifecycleTool()
}

func (t *projectScriptTool) ToolOrigin() core.ToolOrigin {
	if provider, ok := t.Tool.(core.ToolOriginProvider); ok {
		return provider.ToolOrigin()
	}
	return core.ToolOrigin{}
}

func (t *projectScriptTool) ToolBindingID() string {
	if provider, ok := t.Tool.(core.ToolBindingIDProvider); ok {
		return provider.ToolBindingID()
	}
	return ""
}

// CodeModeIdentity preserves the wrapped script's provenance identity. It is
// the engine's optional identity hook rather than a core contract.
func (t *projectScriptTool) CodeModeIdentity(ctx context.Context) (core.ToolSpec, string, error) {
	if provider, ok := t.Tool.(interface {
		CodeModeIdentity(context.Context) (core.ToolSpec, string, error)
	}); ok {
		return provider.CodeModeIdentity(ctx)
	}
	spec := t.Tool.Spec()
	raw, err := json.Marshal(spec)
	if err != nil {
		return spec, "", err
	}
	return spec, fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}
