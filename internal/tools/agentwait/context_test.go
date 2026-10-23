package agentwait

import (
	"context"
	"testing"

	"github.com/samsaffron/term-llm/internal/session"
)

func TestAgentWaitObserverRequiresActualOutstandingWait(t *testing.T) {
	for _, tc := range []struct {
		name, status            string
		waited, cancelled, want bool
	}{
		{"timed out wait", "running", true, false, true},
		{"detached", "running", false, false, false},
		{"already collected", "completed", true, false, false},
		{"child failed", "failed", true, false, false},
		{"parent stopped", "running", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []session.AgentRun
			ctx, cancel := context.WithCancel(WithObserver(context.Background(), func(row session.AgentRun) { got = append(got, row) }))
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			row := session.AgentRun{ID: "child", RunGeneration: 7, ParentResponseID: "response", Status: tc.status}
			Observe(ctx, row, tc.waited)
			if (len(got) == 1) != tc.want {
				t.Fatalf("observations=%v, want observed=%v", got, tc.want)
			}
			if tc.want && (got[0].RunGeneration != 7 || got[0].ParentResponseID != "response") {
				t.Fatalf("lost lineage: %+v", got[0])
			}
		})
	}
}
