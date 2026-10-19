package progressive

import (
	"encoding/json"
	"strings"

	runpkg "github.com/samsaffron/term-llm/internal/run"
)

// OutputText selects the stable textual output of a progressive run.
func OutputText(result RunResult) string {
	if strings.TrimSpace(result.FinalResponse) != "" {
		return result.FinalResponse
	}
	if strings.TrimSpace(result.FallbackText) != "" {
		return result.FallbackText
	}
	if len(result.Progress) == 0 {
		return ""
	}
	data, err := json.Marshal(result.Progress)
	if err != nil {
		return ""
	}
	return string(data)
}

// FromRunResult converts the transport-neutral runner result.
func FromRunResult(result *runpkg.ProgressiveResult) *RunResult {
	if result == nil {
		return nil
	}
	return &RunResult{
		ExitReason: result.ExitReason, Finalized: result.Finalized, SessionID: result.SessionID,
		Sequence: result.Sequence, Reason: result.Reason, Message: result.Message,
		Progress: result.Progress, FinalResponse: result.FinalResponse, FallbackText: result.FallbackText,
	}
}
