package llm

import "github.com/samsaffron/term-llm/internal/llm/core"

func collectTextParts(parts []Part) string { return core.CollectTextParts(parts) }

func collectToolResultText(parts []Part) string { return core.CollectToolResultText(parts) }

func flattenSystemUser(messages []Message) (string, string) {
	return core.FlattenSystemUser(messages)
}
