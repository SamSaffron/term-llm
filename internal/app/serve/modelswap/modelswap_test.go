package modelswap

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestRespondedCountsReasoningButVisibleOutputDoesNot(t *testing.T) {
	for _, tc := range []struct {
		name               string
		ev                 core.Event
		responded, visible bool
	}{
		{"reasoning", core.Event{Type: core.EventReasoningDelta, Text: "thinking"}, true, false},
		{"text", core.Event{Type: core.EventTextDelta, Text: "answer"}, true, true},
		{"empty text", core.Event{Type: core.EventTextDelta}, false, false},
		{"tool call", core.Event{Type: core.EventToolCall}, true, true},
		{"usage", core.Event{Type: core.EventUsage}, false, false},
	} {
		if got := Responded(tc.ev); got != tc.responded {
			t.Errorf("%s: Responded = %v, want %v", tc.name, got, tc.responded)
		}
		// Reasoning must not block a handover fallback after a later failure.
		if got := VisibleOutput(tc.ev); got != tc.visible {
			t.Errorf("%s: VisibleOutput = %v, want %v", tc.name, got, tc.visible)
		}
	}
}

func TestStoppedIsCancellationOrDeadlineOnly(t *testing.T) {
	if !Stopped(fmt.Errorf("run: %w", context.Canceled)) || !Stopped(context.DeadlineExceeded) {
		t.Fatal("wrapped cancellation and deadlines must count as a stop")
	}
	if Stopped(errors.New("400 invalid_request")) || Stopped(nil) {
		t.Fatal("provider failures are not stops")
	}
}

func TestInsertMarkerRowKeepsStoredRowFields(t *testing.T) {
	created := time.Date(2026, 11, 2, 18, 0, 0, 0, time.UTC)
	rows := []session.Message{
		{ID: 1, Role: core.RoleUser, TextContent: "kept tail", TurnIndex: 3, CompactionTail: true, CreatedAt: created},
		{ID: 2, Role: core.RoleUser, TextContent: "trigger", TurnIndex: 4, CreatedAt: created},
		{ID: 3, Role: core.RoleAssistant, TextContent: "partial", TurnIndex: 4, DurationMs: 900, CreatedAt: created,
			Parts: []core.Part{{Type: core.PartText, Text: "partial"}, {Type: core.PartPathNote}}},
	}
	marker := session.Message{Role: core.RoleEvent, TextContent: "marker"}

	updated := InsertMarkerRow(rows, marker)

	if len(updated) != 4 || updated[2].Role != core.RoleEvent || updated[2].TurnIndex != 4 {
		t.Fatalf("marker must follow the trigger, in its turn: %#v", updated)
	}
	if updated[0].ID != 1 || !updated[0].CompactionTail || updated[0].TurnIndex != 3 {
		t.Fatalf("compaction tail row changed: %#v", updated[0])
	}
	if got := updated[3]; got.ID != 3 || got.TurnIndex != 4 || got.DurationMs != 900 || !got.CreatedAt.Equal(created) || len(got.Parts) != 2 {
		t.Fatalf("reply row lost stored fields: %#v", got)
	}
}

func TestInsertMarkerFallsBackToLatestUser(t *testing.T) {
	history := []core.Message{
		core.UserText("original trigger"),
		core.AssistantText("old reply"),
		core.UserText("normalized trigger"),
		core.AssistantText("current reply"),
	}
	updated := InsertMarker(history, core.ModelSwapEventMessage(core.ModelSwapMarker{Status: "succeeded"}))

	if len(updated) != 5 || updated[2].Role != core.RoleUser || updated[3].Role != core.RoleEvent || updated[4].Role != core.RoleAssistant {
		t.Fatalf("marker matched an older repeated prompt instead of the latest user: %#v", updated)
	}
}

func TestInsertMarkerImageOnlyTriggerUsesLatestUser(t *testing.T) {
	imageOnly := core.Message{Role: core.RoleUser, Parts: []core.Part{{Type: core.PartImage, ImagePath: "image.png"}}}
	history := []core.Message{
		core.UserText("handover summary"),
		imageOnly,
		core.AssistantText("reply"),
	}
	updated := InsertMarker(history, core.ModelSwapEventMessage(core.ModelSwapMarker{Status: "succeeded"}))

	if len(updated) != 4 || updated[1].Role != core.RoleUser || updated[2].Role != core.RoleEvent || updated[3].Role != core.RoleAssistant {
		t.Fatalf("image-only trigger anchored marker to handover context: %#v", updated)
	}
}
