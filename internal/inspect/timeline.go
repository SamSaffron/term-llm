package inspect

import (
	"fmt"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func normalizeModelInfo(info ModelInfo) ModelInfo {
	base, effort := llm.BaseModelAndEffortForProvider(info.Provider, info.Model)
	info.Model = base
	if info.Effort == "" {
		info.Effort = effort
	}
	return info
}

func modelInfo(provider, model, effort, mode, agent string) ModelInfo {
	return normalizeModelInfo(ModelInfo{Provider: provider, Model: model, Effort: effort, ReasoningMode: mode, Agent: agent})
}

func modelChange(marker llm.ModelSwapMarker) *ModelChange {
	return &ModelChange{
		From:     modelInfo(marker.FromProvider, marker.FromModel, marker.FromEffort, "", marker.FromAgent),
		To:       modelInfo(marker.ToProvider, marker.ToModel, marker.ToEffort, "", marker.ToAgent),
		Strategy: marker.Strategy, Status: marker.Status,
	}
}

// Legacy rows may contain only display text. Never fall back when structured
// parts exist: those may consist solely of excluded provider protocol data.
func messageParts(msg session.Message) []llm.Part {
	if len(msg.Parts) == 0 && msg.TextContent != "" {
		return []llm.Part{{Type: llm.PartText, Text: msg.TextContent}}
	}
	return msg.Parts
}

func (d *Document) setInitialModel(messages []session.Message) {
	for _, msg := range messages {
		if marker, ok := llm.ParseModelSwapMarker(llm.Message{Role: msg.Role, Parts: messageParts(msg)}); ok {
			d.Header.InitialModel = modelChange(marker).From
			return
		}
	}
}

func projectTimelineText(msg session.Message, prefix string, index int, text string, block Block, body string) (Block, string) {
	if marker, ok := llm.ParseModelSwapMarker(llm.Message{Role: msg.Role, Parts: []llm.Part{{Type: llm.PartText, Text: text}}}); ok {
		block = blockForMessage(msg, prefix, index, "model_change")
		block.ModelChange = modelChange(marker)
		block.Title = llm.FormatModelSwapMarker(marker)
		return block, block.Title
	}
	if msg.Role == llm.RoleUser {
		if source, target, document, ok := llm.ParseHandoverDocument(text); ok {
			block = blockForMessage(msg, prefix, index, "handover_document")
			block.Handover = &Handover{Source: source, Target: target}
			block.Title = fmt.Sprintf("Handover @%s → @%s", source, target)
			return block, document
		}
	}
	return block, body
}

func (d *Document) markHandoverAcknowledgement(previous, current session.Message, start int) {
	if previous.Role != llm.RoleUser || current.Role != llm.RoleAssistant {
		return
	}
	parts := messageParts(previous)
	if len(parts) == 0 {
		return
	}
	source, _, _, ok := llm.ParseHandoverDocument(parts[len(parts)-1].Text)
	if !ok {
		return
	}
	// Match the source as well as the format; an ordinary later model reply is
	// never labelled synthetic just because it happens to use the same wording.
	ack := llm.ReconstructHandoverHistory("", "", source, "")[1].Parts[0].Text
	for i := start; i < len(d.Blocks); i++ {
		block := &d.Blocks[i]
		body := d.bodies[block.ID]
		if block.Kind == "assistant_text" && llm.IsHandoverAcknowledgement(body.Text) && body.Text == ack {
			block.Synthetic = true
			block.Title = "Synthetic acknowledgement"
		}
	}
}
