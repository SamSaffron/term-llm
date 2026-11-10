package core

import (
	"encoding/json"
)

// projectCellOutput never slices JSON. If full status+state won't fit, publish a
// valid explicit limit failure rather than pretend state was returned intact.
// Engine-owned control envelopes keep their truthful status and metadata when
// only visible output exceeds the model budget.
func ProjectCodeModeOutput(output ToolOutput, maxChars int) ToolOutput {
	size := len(output.Content)
	if len(output.ContentParts) > 0 {
		size = 0
		for _, p := range output.ContentParts {
			if p.Type == ToolContentPartText {
				size += len(p.Text)
			}
		}
	}
	if maxChars <= 0 || (len(output.Content) <= maxChars && size <= maxChars) {
		return output
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(output.Content), &envelope) != nil {
		return cellProjectionFailure(output)
	}
	if _, control := envelope["status"]; control {
		if _, legacy := envelope["output"]; !legacy {
			return projectControlOutput(output, envelope, maxChars)
		}
	}
	delete(envelope, "output")
	envelope["omitted_output"] = json.RawMessage(`true`)
	raw, err := json.Marshal(envelope)
	if err != nil || len(raw) > maxChars {
		return cellProjectionFailure(output)
	}
	output.Content = string(raw)
	output.ContentParts = []ToolContentPart{{Type: ToolContentPartText, Text: output.Content}}
	return output
}

func cellProjectionFailure(output ToolOutput) ToolOutput {
	output.Content = `{"status":"failed","error":"model_output_limit","state_committed":false,"omitted_output":true}`
	output.ContentParts = []ToolContentPart{{Type: ToolContentPartText, Text: output.Content}}
	output.IsError = true
	return output
}
