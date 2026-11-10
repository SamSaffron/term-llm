package core

import "unicode/utf8"

// CodeModeEnvelope is model-visible status, separate from full private audit.
type CodeModeEnvelope struct {
	CellID          string                `json:"cell_id"`
	ToolCallID      string                `json:"tool_call_id,omitempty"`
	Status          string                `json:"status"`
	Error           string                `json:"error,omitempty"`
	ErrorTruncated  bool                  `json:"error_truncated,omitempty"`
	ErrorCode       string                `json:"error_code,omitempty"`
	StateCommitted  bool                  `json:"state_committed"`
	StateBytes      int                   `json:"state_bytes,omitempty"`
	MaxOutputTokens int                   `json:"max_output_tokens,omitempty"`
	OutputTruncated bool                  `json:"output_truncated,omitempty"`
	OutputLimited   bool                  `json:"output_limited,omitempty"`
	HasMoreOutput   bool                  `json:"has_more_output,omitempty"`
	OmittedBytes    int                   `json:"output_omitted_bytes,omitempty"`
	OmittedImages   int                   `json:"output_omitted_images,omitempty"`
	YieldMS         int64                 `json:"yield_time_ms,omitempty"`
	ElapsedMS       int64                 `json:"elapsed_ms,omitempty"`
	Calls           []CodeModeCallOutcome `json:"calls,omitempty"`
	UnsettledCalls  []CodeModeCallOutcome `json:"unsettled_calls,omitempty"`
	ActiveCellID    string                `json:"active_cell_id,omitempty"`
}

// CodeModeModelCalls removes argument bodies and Guardian command text from
// provider status while leaving the original ChildCalls audit untouched.
func CodeModeModelCalls(calls []CodeModeCallOutcome) []CodeModeCallOutcome {
	projected := append([]CodeModeCallOutcome(nil), calls...)
	for i := range projected {
		projected[i].Arguments = nil
		projected[i].ArgumentsOmitted = false
		projected[i].GuardianReviews = nil
	}
	return projected
}

// CodeModeEnvelopeFor is shared by pre-drain budgeting and final rendering.
func CodeModeEnvelopeFor(result CodeModeCallResult, toolCallID string) CodeModeEnvelope {
	envelope := CodeModeEnvelope{
		CellID:          result.CellID,
		ToolCallID:      toolCallID,
		Status:          result.Status,
		Error:           result.Error,
		ErrorCode:       result.ErrorCode,
		StateCommitted:  result.StateCommitted,
		StateBytes:      result.StateBytes,
		MaxOutputTokens: result.MaxOutputTokens,
		OutputTruncated: result.OutputTruncated,
		OutputLimited:   result.OutputLimited,
		HasMoreOutput:   result.HasMoreOutput,
		OmittedBytes:    result.OmittedBytes,
		OmittedImages:   result.OmittedImages,
		YieldMS:         result.Yield.Milliseconds(),
		ElapsedMS:       result.Elapsed.Milliseconds(),
		Calls:           CodeModeModelCalls(result.Calls),
		UnsettledCalls:  CodeModeModelCalls(result.UnsettledCalls),
		ActiveCellID:    result.ActiveCellID,
	}
	if len(envelope.Error) > 2048 {
		envelope.Error = envelope.Error[:2048]
		for !utf8.ValidString(envelope.Error) {
			envelope.Error = envelope.Error[:len(envelope.Error)-1]
		}
		envelope.ErrorTruncated = true
	}
	return envelope
}
