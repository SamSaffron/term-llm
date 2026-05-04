package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

const ModelSwapEventType = "model_swap"

// ModelSwapMarker is a durable non-LLM transcript event describing a provider
// or model switch. Store/render layers may persist this, but provider request
// builders must filter RoleEvent messages before sending context to an LLM.
type ModelSwapMarker struct {
	Type         string `json:"type"`
	FromProvider string `json:"from_provider,omitempty"`
	FromModel    string `json:"from_model,omitempty"`
	FromEffort   string `json:"from_effort,omitempty"`
	ToProvider   string `json:"to_provider,omitempty"`
	ToModel      string `json:"to_model,omitempty"`
	ToEffort     string `json:"to_effort,omitempty"`
	Strategy     string `json:"strategy,omitempty"`
	Status       string `json:"status,omitempty"`
	DisplayText  string `json:"display_text,omitempty"`
}

// ModelSwapEventMessage returns a RoleEvent message suitable for durable
// transcript storage. It intentionally uses a text part containing structured
// JSON so existing session storage schemas can persist it without migration.
func ModelSwapEventMessage(marker ModelSwapMarker) Message {
	marker.Type = ModelSwapEventType
	if strings.TrimSpace(marker.DisplayText) == "" {
		marker.DisplayText = FormatModelSwapMarker(marker)
	}
	data, err := json.Marshal(marker)
	if err != nil {
		data = []byte(fmt.Sprintf(`{"type":"%s","display_text":"%s"}`, ModelSwapEventType, strings.ReplaceAll(marker.DisplayText, `"`, `'`)))
	}
	return Message{Role: RoleEvent, Parts: []Part{{Type: PartText, Text: string(data)}}}
}

// ParseModelSwapMarker extracts a model-swap marker from a durable event
// message.
func ParseModelSwapMarker(msg Message) (ModelSwapMarker, bool) {
	if msg.Role != RoleEvent {
		return ModelSwapMarker{}, false
	}
	for _, part := range msg.Parts {
		if part.Type != PartText || strings.TrimSpace(part.Text) == "" {
			continue
		}
		var marker ModelSwapMarker
		if err := json.Unmarshal([]byte(part.Text), &marker); err != nil {
			continue
		}
		if marker.Type != ModelSwapEventType {
			continue
		}
		if strings.TrimSpace(marker.DisplayText) == "" {
			marker.DisplayText = FormatModelSwapMarker(marker)
		}
		return marker, true
	}
	return ModelSwapMarker{}, false
}

func FormatModelSwapMarker(marker ModelSwapMarker) string {
	from := formatProviderModelEffort(marker.FromProvider, marker.FromModel, marker.FromEffort)
	to := formatProviderModelEffort(marker.ToProvider, marker.ToModel, marker.ToEffort)
	if from == "" {
		from = "previous model"
	}
	if to == "" {
		to = "target model"
	}
	strategy := strings.TrimSpace(marker.Strategy)
	status := strings.TrimSpace(marker.Status)
	switch status {
	case "failed":
		return fmt.Sprintf("↔ Model switch failed: %s → %s; restored %s", from, to, from)
	case "started":
		return fmt.Sprintf("↔ Model switch: %s → %s", from, to)
	default:
		if strategy == "handover" {
			return fmt.Sprintf("↔ Model switch: %s → %s (handover)", from, to)
		}
		return fmt.Sprintf("↔ Model switch: %s → %s", from, to)
	}
}

func formatProviderModelEffort(provider, model, effort string) string {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	effort = strings.TrimSpace(effort)
	base := ""
	switch {
	case provider != "" && model != "":
		base = provider + ":" + model
	case provider != "":
		base = provider
	case model != "":
		base = model
	}
	if effort != "" {
		if base == "" {
			return effort
		}
		return base + " / " + effort
	}
	return base
}
