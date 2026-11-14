package llm_test

import (
	"github.com/term-llm/term-llm/internal/llm"
	"github.com/term-llm/term-llm/internal/llm/core"
	"testing"
)

// This script-only gate fixture explicitly opts in; general mocks do not.
func (*turnGate) CodeModeExposure() core.CodeModeExposure {
	return core.CodeModeExposure{Allowed: true}
}

func TestWebToolCodeModeExposure(t *testing.T) {
	for _, tool := range []core.CodeModeExposureProvider{llm.NewWebSearchTool(nil), llm.NewReadURLTool()} {
		if exposure := tool.CodeModeExposure(); !exposure.Allowed || exposure.Reason != "" {
			t.Fatalf("%T exposure = %+v", tool, exposure)
		}
	}
}
