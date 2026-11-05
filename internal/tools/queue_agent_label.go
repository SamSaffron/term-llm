package tools

import "github.com/samsaffron/term-llm/internal/formatutil"

// queueAgentLabelSchema advertises the optional presentation label.
func queueAgentLabelSchema() map[string]any {
	return map[string]any{"type": "string", "maxLength": formatutil.MaxDisplayLabelRunes,
		"description": "Optional short label shown as this job's title in conversation activity; presentation only, never job identity."}
}

func queueAgentLabel(label string) error { return formatutil.ValidDisplayLabel(label) }
