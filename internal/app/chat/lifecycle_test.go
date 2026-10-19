package chat

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/lifecycle"
	"github.com/samsaffron/term-llm/internal/termhost"
)

type forcedCleanupReporter struct {
	steps *[]string
}

func (r *forcedCleanupReporter) Report(lifecycle.Snapshot) termhost.Control {
	return termhost.Control{}
}
func (r *forcedCleanupReporter) RestoreOSC() string {
	*r.steps = append(*r.steps, "restore")
	return "clear"
}
func (r *forcedCleanupReporter) Close() {
	*r.steps = append(*r.steps, "close")
}

func TestForcedLifecycleCleanupRestoresOSCBeforeClosingAdapters(t *testing.T) {
	var steps []string
	reporter := &forcedCleanupReporter{steps: &steps}
	ForcedLifecycleCleanup(reporter, func(sequence string) (int, error) {
		steps = append(steps, "write:"+sequence)
		return len(sequence), nil
	})
	if got := strings.Join(steps, ","); got != "restore,write:clear,close" {
		t.Fatalf("forced cleanup order = %q", got)
	}
}
