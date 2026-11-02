package inspect

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestInspectOrderedReasoningBursts(t *testing.T) {
	for _, encrypted := range []string{"", "ENCRYPTED_REPLAY"} {
		t.Run("encrypted="+encrypted, func(t *testing.T) {
			msg := inspectMessage(41, core.RoleAssistant,
				core.Part{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "one", Name: "shell"}},
				core.Part{Type: core.PartText, ReasoningDisplay: &core.ReasoningDisplay{Content: "first thought", Kind: core.ReasoningKindRaw, SummaryTitle: "First"}},
				core.Part{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "two", Name: "read_file"}},
				core.Part{Type: core.PartText, ReasoningDisplay: &core.ReasoningDisplay{Content: "stale", SummaryParts: []string{"**Second**", "second thought"}, Kind: core.ReasoningKindSummary}},
				core.Part{Type: core.PartText, Text: "answer", ReasoningReplayOnly: true, ReasoningKind: core.ReasoningKindRaw, ReasoningContent: "COMBINED_CLEAR_TEXT", ReasoningSummaryParts: []string{"COMBINED_SUMMARY"}, ReasoningSummaryTitle: "COMBINED_TITLE", ReasoningEncryptedContent: encrypted},
			)
			opts := Options{SessionID: "session", TranscriptRev: 12}
			d := Project([]session.Message{msg}, opts)
			ids := []string{"m41.p0.tool_call", "m41.p1.reasoning", "m41.p2.tool_call", "m41.p3.reasoning"}
			if encrypted != "" {
				ids = append(ids, "m41.p4.reasoning")
			}
			ids = append(ids, "m41.p4.assistant_text")
			var gotIDs []string
			for _, block := range d.Blocks {
				gotIDs = append(gotIDs, block.ID)
			}
			if !reflect.DeepEqual(gotIDs, ids) {
				t.Fatalf("IDs = %v, want %v", gotIDs, ids)
			}
			if encrypted != "" {
				block := d.Blocks[4]
				if block.ReasoningKind != core.ReasoningKindEncrypted || block.Title != "Encrypted reasoning" || block.EncryptedBytes != len(encrypted) || block.HasBody || block.TotalBytes != 0 || block.Preview != "" {
					t.Fatalf("replay metadata = %+v", block)
				}
			}
			if d.Blocks[1].Title != "First" || d.Blocks[1].ReasoningKind != core.ReasoningKindRaw || d.Blocks[3].Title != "Second" || d.Blocks[3].ReasoningKind != core.ReasoningKindSummary {
				t.Fatalf("burst classification = %+v", d.Blocks)
			}
			bodies := d.ResolveBodies([]string{"m41.p1.reasoning", "m41.p3.reasoning", "m41.p4.assistant_text", "m41.p4.reasoning"})
			wantBodies := []Body{
				{ID: "m41.p1.reasoning", Format: "text", Text: "first thought", TotalBytes: len("first thought")},
				{ID: "m41.p3.reasoning", Format: "text", Text: "second thought", TotalBytes: len("second thought")},
				{ID: "m41.p4.assistant_text", Format: "text", Text: "answer", TotalBytes: len("answer")},
			}
			if !reflect.DeepEqual(bodies.Bodies, wantBodies) || !reflect.DeepEqual(bodies.Missing, []string{"m41.p4.reasoning"}) {
				t.Fatalf("bodies = %+v", bodies)
			}
			again := Project([]session.Message{msg}, opts)
			if !reflect.DeepEqual(d.Blocks, again.Blocks) || !reflect.DeepEqual(bodies, again.ResolveBodies([]string{"m41.p1.reasoning", "m41.p3.reasoning", "m41.p4.assistant_text", "m41.p4.reasoning"})) {
				t.Fatal("projection or body views are not deterministic")
			}
			encoded, err := json.Marshal([]any{d, bodies})
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"COMBINED_CLEAR_TEXT", "COMBINED_SUMMARY", "COMBINED_TITLE", "ENCRYPTED_REPLAY"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("projection leaked %q", secret)
				}
			}
		})
	}
}

func TestInspectReasoningEncryptedBytes(t *testing.T) {
	for _, tt := range []struct {
		name  string
		part  core.Part
		kind  core.ReasoningKind
		text  string
		bytes int
	}{
		{"replay clear text excluded", core.Part{ReasoningReplayOnly: true, ReasoningKind: core.ReasoningKindRaw, ReasoningContent: "COMBINED", ReasoningEncryptedContent: "SECRET"}, core.ReasoningKindEncrypted, "", 6},
		{"replay encrypted text counted", core.Part{ReasoningReplayOnly: true, ReasoningKind: core.ReasoningKindEncrypted, ReasoningContent: "CIPHER", ReasoningEncryptedContent: "SECRET"}, core.ReasoningKindEncrypted, "", 12},
		{"burst over encrypted aggregate", core.Part{ReasoningReplayOnly: true, ReasoningKind: core.ReasoningKindEncrypted, ReasoningContent: "CIPHER", ReasoningEncryptedContent: "SECRET", ReasoningDisplay: &core.ReasoningDisplay{Kind: core.ReasoningKindRaw, Content: "visible"}}, core.ReasoningKindRaw, "visible", 12},
		{"burst over clear aggregate", core.Part{ReasoningReplayOnly: true, ReasoningKind: core.ReasoningKindSummary, ReasoningContent: "COMBINED", ReasoningEncryptedContent: "SECRET", ReasoningDisplay: &core.ReasoningDisplay{Kind: core.ReasoningKindUnknown, Content: "visible"}}, core.ReasoningKindUnknown, "visible", 6},
		{"encrypted bytes not rune count", core.Part{ReasoningReplayOnly: true, ReasoningKind: core.ReasoningKindEncrypted, ReasoningContent: "密", ReasoningEncryptedContent: "文"}, core.ReasoningKindEncrypted, "", 6},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.part.Type = core.PartText
			d := Project([]session.Message{inspectMessage(7, core.RoleAssistant, tt.part)}, Options{})
			if len(d.Blocks) != 1 {
				t.Fatalf("blocks = %+v", d.Blocks)
			}
			block := d.Blocks[0]
			if block.ReasoningKind != tt.kind || block.EncryptedBytes != tt.bytes || block.HasBody != (tt.text != "") || block.TotalBytes != len(tt.text) {
				t.Fatalf("block = %+v", block)
			}
			bodies := d.ResolveBodies([]string{block.ID})
			if tt.text == "" {
				if len(bodies.Bodies) != 0 || !reflect.DeepEqual(bodies.Missing, []string{block.ID}) {
					t.Fatalf("encrypted body = %+v", bodies)
				}
			} else if len(bodies.Bodies) != 1 || bodies.Bodies[0].Text != tt.text {
				t.Fatalf("burst body = %+v", bodies)
			}
		})
	}
}

func TestInspectReasoningDisplayLegacyParity(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		part                 core.Part
		kind                 core.ReasoningKind
		title, body, summary string
	}{
		{"unknown", core.Part{ReasoningKind: core.ReasoningKindUnknown, ReasoningContent: " unknown text ", ReasoningSummaryTitle: "Explicit"}, core.ReasoningKindUnknown, "Explicit", "unknown text", " unknown text "},
		{"legacy summary", core.Part{ReasoningContent: "**Plan**\n\nDo this"}, core.ReasoningKindSummary, "Plan", "Do this", "**Plan**\n\nDo this"},
		{"summary parts", core.Part{ReasoningKind: core.ReasoningKindSummary, ReasoningContent: "old", ReasoningSummaryParts: []string{" **Plan** ", " first ", " ", " second "}}, core.ReasoningKindSummary, "Plan", "first\n\nsecond", "**Plan**\n\nfirst\n\nsecond"},
		{"blank parts fallback", core.Part{ReasoningContent: "fallback", ReasoningSummaryParts: []string{" "}}, core.ReasoningKindSummary, "Thinking...", "fallback", "fallback"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			legacy := tt.part
			burst := core.Part{ReasoningReplayOnly: true, ReasoningContent: "COMBINED", ReasoningSummaryParts: []string{"COMBINED"}, ReasoningKind: core.ReasoningKindEncrypted, ReasoningSummaryTitle: "WRONG", ReasoningDisplay: &core.ReasoningDisplay{Content: legacy.ReasoningContent, SummaryParts: legacy.ReasoningSummaryParts, Kind: legacy.ReasoningKind, SummaryTitle: legacy.ReasoningSummaryTitle}}
			for _, part := range []core.Part{legacy, burst} {
				kind, title, body := ReasoningDisplay(part)
				if kind != tt.kind || title != tt.title || body != tt.body || StoredReasoningKind(part) != tt.kind || ReasoningSummaryDisplayContent(part) != tt.summary {
					t.Fatalf("display = %q/%q/%q, stored kind = %q, summary = %q", kind, title, body, StoredReasoningKind(part), ReasoningSummaryDisplayContent(part))
				}
			}
		})
	}
	part := core.Part{ReasoningReplayOnly: true, ReasoningContent: "COMBINED", ReasoningSummaryParts: []string{"COMBINED"}}
	if got := ReasoningSummaryDisplayContent(part); got != "" {
		t.Fatalf("replay-only summary = %q", got)
	}
}
