package core

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestReasoningForDisplay(t *testing.T) {
	burst := &ReasoningDisplay{Content: "after tool", Kind: ReasoningKindRaw}
	for _, tc := range []struct {
		name string
		part Part
		want *ReasoningDisplay
	}{
		{"burst over aggregate", Part{ReasoningDisplay: burst, ReasoningReplayOnly: true, ReasoningContent: "beforeafter"}, burst},
		{"filtered burst cannot expose aggregate", Part{ReasoningReplayOnly: true, ReasoningContent: "private"}, nil},
		{"legacy summary", Part{ReasoningContent: "old summary"}, &ReasoningDisplay{Content: "old summary", Kind: ReasoningKindSummary}},
		{"explicit raw", Part{ReasoningContent: "raw", ReasoningKind: ReasoningKindRaw}, &ReasoningDisplay{Content: "raw", Kind: ReasoningKindRaw}},
		{"structured summary", Part{ReasoningSummaryParts: []string{"a", "b"}}, &ReasoningDisplay{SummaryParts: []string{"a", "b"}, Kind: ReasoningKindSummary}},
		{"encrypted only", Part{ReasoningEncryptedContent: "secret", ReasoningKind: ReasoningKindEncrypted}, nil},
		{"no reasoning", Part{Text: "answer"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReasoningForDisplay(tc.part); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ReasoningForDisplay = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestReasoningDisplayRoundTripAndClone(t *testing.T) {
	original := Part{Type: PartText, CreatedAt: 123, ReasoningDisplay: &ReasoningDisplay{
		Content: "burst", SummaryParts: []string{"burst"}, Kind: ReasoningKindSummary, SummaryTitle: "title",
	}, ReasoningReplayOnly: true, ReasoningContent: "aggregate", ReasoningEncryptedContent: "secret"}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var restored Part
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, original) {
		t.Fatalf("round trip = %#v, want %#v", restored, original)
	}
	cloned := CloneParts([]Part{original})
	cloned[0].ReasoningDisplay.Content = "changed"
	cloned[0].ReasoningDisplay.SummaryParts[0] = "changed"
	if original.ReasoningDisplay.Content != "burst" || original.ReasoningDisplay.SummaryParts[0] != "burst" {
		t.Fatalf("clone aliases original: %#v", original.ReasoningDisplay)
	}
}
