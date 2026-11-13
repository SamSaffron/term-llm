package testutil

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestMockToolScriptExposureRequiresExplicitOptIn(t *testing.T) {
	tool := NewMockTool("unknown", "ok")
	if tool.CodeModeExposure().Allowed {
		t.Fatal("generic mock silently opted into scripts")
	}
	tool.Exposure = core.CodeModeExposure{Allowed: true}
	if !tool.CodeModeExposure().Allowed {
		t.Fatal("fixture cannot opt into scripts explicitly")
	}
}
