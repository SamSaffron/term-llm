package core

import (
	"context"
	"time"
)

// CodeModeController is the engine-owned lifecycle for asynchronous code
// cells. Tools must fail closed without it; a script cannot manufacture a
// controller or address another engine's cells.
type CodeModeController interface {
	ExecCodeMode(ctx context.Context, code string, yield time.Duration, maxOutputTokens int) (CodeModeCallResult, error)
	WaitCodeMode(ctx context.Context, cellID string, yield time.Duration, maxOutputTokens int) (CodeModeCallResult, error)
	CancelCodeMode(ctx context.Context, cellID string) (CodeModeCallResult, error)
}

// CodeModeCallResult is one bounded observation of an async cell. Parts are
// already ordered and materialized; Calls are incremental audit metadata.
type CodeModeCallResult struct {
	CellID          string
	Status          string
	Error           string
	ErrorCode       string
	StateCommitted  bool
	StateBytes      int
	Parts           []ToolContentPart
	MaxOutputTokens int
	OutputTruncated bool
	OutputLimited   bool
	HasMoreOutput   bool
	OmittedBytes    int
	OmittedImages   int
	Calls           []CodeModeCallOutcome
	UnsettledCalls  []CodeModeCallOutcome
	Yield           time.Duration
	Elapsed         time.Duration
	IsError         bool
	ActiveCellID    string
}

type codeModeControllerKey struct{}

// ContextWithCodeModeController injects engine-owned cell lifecycle authority.
func ContextWithCodeModeController(ctx context.Context, controller CodeModeController) context.Context {
	return context.WithValue(ctx, codeModeControllerKey{}, controller)
}

// CodeModeControllerFromContext returns the engine controller, if any.
func CodeModeControllerFromContext(ctx context.Context) CodeModeController {
	controller, _ := ctx.Value(codeModeControllerKey{}).(CodeModeController)
	return controller
}
