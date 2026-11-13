package docsgen

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/tools"
)

func TestToolTableIncludesDeclaredScriptExposure(t *testing.T) {
	table := toolTable()
	if !strings.Contains(table, "| Script exposure |") {
		t.Fatal("tool reference omits the independent script exposure policy")
	}
	for _, descriptor := range tools.BuiltinToolDescriptors() {
		var row string
		for _, line := range strings.Split(table, "\n") {
			if strings.HasPrefix(line, "| `"+descriptor.Name+"` |") {
				row = line
				break
			}
		}
		if row == "" {
			t.Errorf("missing tool %s", descriptor.Name)
			continue
		}
		if descriptor.CodeModeExposure.Allowed {
			if !strings.Contains(row, "Allowed when bound") {
				t.Errorf("%s lost allowed exposure", descriptor.Name)
			}
		} else if descriptor.CodeModeExposure.Reason == "" || !strings.Contains(row, "Direct only: `"+descriptor.CodeModeExposure.Reason+"`") {
			t.Errorf("%s lost direct-only reason: %s", descriptor.Name, row)
		}
	}
}
