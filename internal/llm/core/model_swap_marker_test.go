package core

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestFormatModelSwapMarker(t *testing.T) {
	for _, tt := range []struct {
		name   string
		marker ModelSwapMarker
		want   string
	}{
		{"empty", ModelSwapMarker{}, "↔ Model switch: previous model → target model"},
		{"normal", ModelSwapMarker{FromProvider: "openai", FromModel: "gpt-5", FromEffort: "high", ToProvider: "anthropic", ToModel: "claude", ToEffort: "max"}, "↔ Model switch: openai:gpt-5 / high → anthropic:claude / max"},
		{"handover", ModelSwapMarker{FromModel: "old", ToModel: "new", Strategy: "handover", Status: "succeeded"}, "↔ Model switch: old → new (handover)"},
		{"failed", ModelSwapMarker{FromModel: "old", ToModel: "new", Strategy: "handover", Status: "failed"}, "↔ Model switch failed: old → new; restored old"},
		{"started", ModelSwapMarker{FromModel: "old", ToModel: "new", Strategy: "handover", Status: "started"}, "↔ Model switch: old → new"},
		{"agents", ModelSwapMarker{FromProvider: "openai", FromModel: "gpt-5", FromEffort: "high", FromAgent: "dev", ToProvider: "anthropic", ToModel: "claude", ToEffort: "max", ToAgent: "reviewer", Strategy: "handover"}, "↔ Model switch: @dev openai:gpt-5 / high → @reviewer anthropic:claude / max (handover)"},
		{"failed agents", ModelSwapMarker{FromModel: "old", FromAgent: "dev", ToModel: "new", ToAgent: "reviewer", Status: "failed"}, "↔ Model switch failed: @dev old → @reviewer new; restored @dev old"},
		{"one agent", ModelSwapMarker{FromModel: "old", ToModel: "new", ToAgent: " @reviewer "}, "↔ Model switch: old → @reviewer new"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatModelSwapMarker(tt.marker); got != tt.want {
				t.Fatalf("format = %q, want %q", got, tt.want)
			}
			msg := ModelSwapEventMessage(tt.marker)
			parsed, ok := ParseModelSwapMarker(msg)
			if !ok || parsed.FromAgent != tt.marker.FromAgent || parsed.ToAgent != tt.marker.ToAgent || parsed.DisplayText != tt.want {
				t.Fatalf("round trip = %+v, parsed %v", parsed, ok)
			}
		})
	}
}

func TestModelSwapMarkerEmptyAgentsOmitted(t *testing.T) {
	data, err := json.Marshal(ModelSwapMarker{Type: ModelSwapEventType})
	if err != nil || string(data) != `{"type":"model_swap"}` {
		t.Fatalf("marker JSON = %s, err %v", data, err)
	}
}

func TestModelSwapMarkerExcludedFromProviderHistory(t *testing.T) {
	marker := ModelSwapEventMessage(ModelSwapMarker{FromAgent: "source", ToAgent: "target", Strategy: "handover", Status: "succeeded"})
	user := UserText("briefing")
	user.CacheAnchor = true
	messages := []Message{SystemText("system"), marker, user, AssistantText("ack")}
	want := []Message{messages[0], messages[2], messages[3]}
	for _, tt := range []struct {
		name   string
		filter func([]Message) []Message
	}{
		{"conversation filter", FilterConversationMessages},
		{"provider history sanitizer", SanitizeToolHistory},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.filter(messages); !reflect.DeepEqual(got, want) {
				t.Fatalf("provider history = %+v, want %+v", got, want)
			}
		})
	}
}
