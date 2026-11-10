package core

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// projectControlOutput is a last-resort projection (including historical
// compaction). Fresh observations are budgeted before draining the live queue.
// Preserve ordered media and as much text as fits, never replace everything
// with status or reinterpret the success of an explicit cancel operation.
func projectControlOutput(output ToolOutput, envelope map[string]json.RawMessage, maxChars int) ToolOutput {
	envelope["output_truncated"] = json.RawMessage(`true`)
	envelope["omitted_output"] = json.RawMessage(`true`)
	raw, err := MarshalCodeModeJSON(envelope)
	if err != nil || len(raw) >= maxChars {
		minimal := map[string]json.RawMessage{}
		for _, key := range []string{"cell_id", "tool_call_id", "status", "state_committed", "error_code", "has_more_output", "output_limited"} {
			if value, ok := envelope[key]; ok && !((key == "has_more_output" || key == "output_limited") && string(value) == "false") {
				minimal[key] = value
			}
		}
		if calls, ok := envelope["calls"]; ok && len(calls) > 2 {
			minimal["audit_omitted"] = json.RawMessage(`true`)
		}
		var unsettled []json.RawMessage
		if json.Unmarshal(envelope["unsettled_calls"], &unsettled) == nil && len(unsettled) > 0 {
			minimal["unsettled_call_count"], _ = json.Marshal(len(unsettled))
			minimal["outcome_unknown"] = json.RawMessage(`true`)
		}
		minimal["output_truncated"] = json.RawMessage(`true`)
		minimal["omitted_output"] = json.RawMessage(`true`)
		raw, err = MarshalCodeModeJSON(minimal)
		if err != nil || len(raw) > maxChars {
			return cellProjectionFailure(output)
		}
	}
	remaining := max(0, maxChars-len(raw)-1)
	parts := make([]ToolContentPart, 0, len(output.ContentParts)+1)
	for i, part := range output.ContentParts {
		if i == len(output.ContentParts)-1 && part.Type == ToolContentPartText && strings.TrimSpace(part.Text) == output.Content {
			continue // only the trailing status envelope, not deliberately emitted JSON
		}
		if part.Type == ToolContentPartText {
			if len(part.Text) > remaining {
				part.Text = part.Text[:remaining]
				for !utf8.ValidString(part.Text) {
					part.Text = part.Text[:len(part.Text)-1]
				}
			}
			remaining -= len(part.Text)
			if part.Text == "" {
				continue
			}
		}
		parts = append(parts, part)
	}
	output.Content = string(raw)
	output.ContentParts = append(parts, ToolContentPart{Type: ToolContentPartText, Text: "\n" + output.Content})
	return output
}
