package progressive

import (
	"reflect"
	"testing"

	runpkg "github.com/samsaffron/term-llm/internal/run"
)

func TestOutputTextPreservesSelectionOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   RunResult
		want string
	}{
		{name: "final response", in: RunResult{FinalResponse: " final ", FallbackText: "fallback", Progress: map[string]any{"step": 1}}, want: " final "},
		{name: "fallback", in: RunResult{FinalResponse: " ", FallbackText: "fallback", Progress: map[string]any{"step": 1}}, want: "fallback"},
		{name: "progress JSON", in: RunResult{Progress: map[string]any{"step": float64(1)}}, want: `{"step":1}`},
		{name: "empty", in: RunResult{}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := OutputText(tc.in); got != tc.want {
				t.Fatalf("OutputText() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFromRunResultCopiesEveryField(t *testing.T) {
	if FromRunResult(nil) != nil {
		t.Fatal("FromRunResult(nil) must remain nil")
	}
	in := &runpkg.ProgressiveResult{
		ExitReason: "timeout", Finalized: true, SessionID: "session", Sequence: 4,
		Reason: "reason", Message: "message", Progress: map[string]any{"step": "four"},
		FinalResponse: "final", FallbackText: "fallback",
	}
	want := &RunResult{
		ExitReason: "timeout", Finalized: true, SessionID: "session", Sequence: 4,
		Reason: "reason", Message: "message", Progress: map[string]any{"step": "four"},
		FinalResponse: "final", FallbackText: "fallback",
	}
	if got := FromRunResult(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("FromRunResult() = %#v, want %#v", got, want)
	}
}
