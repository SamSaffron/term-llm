package serve

import "testing"

func TestServeRunResultTextSurvivesValueHandoff(t *testing.T) {
	var result serveRunResult
	result.Text.WriteString("first")
	// Finalization, response callbacks, and child lifecycle turns all hand run
	// results around by value. The text accumulator must not retain a hidden
	// self-pointer into the original result's stack frame.
	forwarded := func(completed serveRunResult) serveRunResult { return completed }(result)
	forwarded.Text.WriteString(" second")
	if got := forwarded.Text.String(); got != "first second" {
		t.Fatalf("forwarded text = %q", got)
	}
	if got := result.Text.String(); got != "first" {
		t.Fatalf("source text changed during handoff: %q", got)
	}
}
