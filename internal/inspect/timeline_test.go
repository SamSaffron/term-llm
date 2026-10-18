package inspect

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func swapMessage(id int64, marker llm.ModelSwapMarker) session.Message {
	msg := llm.ModelSwapEventMessage(marker)
	return inspectMessage(id, msg.Role, msg.Parts...)
}

func TestInspectModelTimelineHeader(t *testing.T) {
	meta := &session.Session{Provider: "OpenAI label", ProviderKey: "openai", Model: "gpt-5-high", ReasoningMode: "pro", Agent: "dev"}
	current := ModelInfo{Provider: "openai", Model: "gpt-5", Effort: "high", ReasoningMode: "pro", Agent: "dev"}
	first := llm.ModelSwapMarker{FromProvider: "claude-bin", FromModel: "opus-max", FromAgent: "source", ToProvider: "openai", ToModel: "gpt-5-high"}
	for _, tt := range []struct {
		name     string
		messages []session.Message
		initial  ModelInfo
	}{
		{"no marker", nil, current},
		{"one marker", []session.Message{swapMessage(1, first)}, ModelInfo{Provider: "claude-bin", Model: "opus", Effort: "max", Agent: "source"}},
		{"first of multiple", []session.Message{swapMessage(1, first), swapMessage(2, llm.ModelSwapMarker{FromModel: "later"})}, ModelInfo{Provider: "claude-bin", Model: "opus", Effort: "max", Agent: "source"}},
		{"missing fields stay unknown", []session.Message{swapMessage(1, llm.ModelSwapMarker{FromModel: "old"})}, ModelInfo{Model: "old"}},
		{"empty from stays unknown", []session.Message{swapMessage(1, llm.ModelSwapMarker{})}, ModelInfo{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := Project(tt.messages, Options{Session: meta})
			if d.Header.InitialModel != tt.initial || d.Header.CurrentModel != current {
				t.Fatalf("models = %+v / %+v, want %+v / %+v", d.Header.InitialModel, d.Header.CurrentModel, tt.initial, current)
			}
			if d.Header.Provider != meta.Provider || d.Header.Model != meta.Model {
				t.Fatal("legacy header changed")
			}
		})
	}
}

func TestInspectCurrentModelFallback(t *testing.T) {
	for _, tt := range []struct {
		name string
		meta *session.Session
		want ModelInfo
	}{
		{"no metadata", nil, ModelInfo{}},
		{"provider label fallback", &session.Session{Provider: "openai", Model: "gpt-5-high"}, ModelInfo{Provider: "openai", Model: "gpt-5", Effort: "high"}},
		{"explicit effort wins", &session.Session{ProviderKey: "openai", Model: "gpt-5-high", ReasoningEffort: "low"}, ModelInfo{Provider: "openai", Model: "gpt-5", Effort: "low"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := Project(nil, Options{Session: tt.meta}).Header
			if h.CurrentModel != tt.want || h.InitialModel != tt.want {
				t.Fatalf("header = %+v", h)
			}
		})
	}
}

func TestInspectModelChangeBlocks(t *testing.T) {
	for _, tt := range []struct{ strategy, status string }{
		{"naive", "succeeded"}, {"handover", "succeeded"}, {"handover", "failed"}, {"naive", "started"},
	} {
		t.Run(tt.strategy+"/"+tt.status, func(t *testing.T) {
			marker := llm.ModelSwapMarker{FromProvider: "openai", FromModel: "gpt-5-high", FromAgent: "dev", ToProvider: "claude-bin", ToModel: "opus-max", ToAgent: "reviewer", Strategy: tt.strategy, Status: tt.status, DisplayText: "stale display"}
			d := Project([]session.Message{swapMessage(12, marker)}, Options{})
			block := d.Blocks[0]
			want := llm.FormatModelSwapMarker(marker)
			if block.Kind != "model_change" || block.ID != "m12.p0.model_change" || block.Title != want || !block.HasBody {
				t.Fatalf("block = %+v", block)
			}
			change := block.ModelChange
			if change == nil || change.Strategy != tt.strategy || change.Status != tt.status || change.From.Model != "gpt-5" || change.From.Effort != "high" || change.From.Agent != "dev" || change.To.Model != "opus" || change.To.Effort != "max" || change.To.Agent != "reviewer" {
				t.Fatalf("change = %+v", change)
			}
			body := d.ResolveBodies([]string{block.ID}).Bodies[0]
			if body.Text != want || body.TotalBytes != len(want) || strings.Contains(body.Text, `"type"`) {
				t.Fatalf("body = %+v", body)
			}
			if tt.status == "failed" && !strings.Contains(body.Text, "; restored @dev openai:gpt-5-high") {
				t.Fatal("failure lacks restoration")
			}
		})
	}
}

func TestInspectHandoverDocumentAndAcknowledgement(t *testing.T) {
	history := llm.ReconstructHandoverHistory("system", "briefing\n\nexact body\n", "dev", "reviewer")
	messages := make([]session.Message, len(history))
	for i, msg := range history {
		messages[i] = inspectMessage(int64(i+1), msg.Role, msg.Parts...)
	}
	messages = append(messages, inspectMessage(4, history[2].Role, history[2].Parts...))
	d := Project(messages, Options{})
	doc, ack, later := d.Blocks[1], d.Blocks[2], d.Blocks[3]
	if doc.Kind != "handover_document" || doc.Title != "Handover @dev → @reviewer" || doc.Handover == nil || *doc.Handover != (Handover{Source: "dev", Target: "reviewer"}) {
		t.Fatalf("document = %+v", doc)
	}
	if body := d.ResolveBodies([]string{doc.ID}).Bodies[0].Text; body != "briefing\n\nexact body\n" {
		t.Fatalf("document body = %q", body)
	}
	if ack.Kind != "assistant_text" || !ack.Synthetic || ack.Title != "Synthetic acknowledgement" || later.Synthetic {
		t.Fatalf("ack = %+v, later = %+v", ack, later)
	}
	// Text with this wording but without a preceding handover is model output.
	plain := Project(messages[2:3], Options{})
	if plain.Blocks[0].Synthetic {
		t.Fatal("standalone acknowledgement labelled synthetic")
	}
}

func TestInspectLegacyModelMarker(t *testing.T) {
	msg := swapMessage(1, llm.ModelSwapMarker{FromProvider: "openai", FromModel: "gpt-5-high"})
	msg.TextContent, msg.Parts = msg.Parts[0].Text, nil
	d := Project([]session.Message{msg}, Options{})
	if d.Header.InitialModel.Model != "gpt-5" || d.Blocks[0].Kind != "model_change" {
		t.Fatalf("legacy projection = %+v", d)
	}
}
