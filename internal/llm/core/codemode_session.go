package core

import (
	"context"
	"sync"
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
	CellID               string
	Status               string
	Error                string
	ErrorCode            string
	StateCommitted       bool
	StateBytes           int
	Parts                []ToolContentPart
	MaxOutputTokens      int
	OutputTruncated      bool
	OutputLimited        bool
	HasMoreOutput        bool
	OmittedBytes         int
	OmittedImages        int
	Calls                []CodeModeCallOutcome
	UnsettledCalls       []CodeModeCallOutcome
	Yield                time.Duration
	Elapsed              time.Duration
	IsError              bool
	ActiveCellID         string
	LiveCellIDs          []string
	StateOverwrittenKeys []string
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

// CodeModeCellRef binds host prompts to the original cell, not a later turn.
type CodeModeCellRef struct {
	SessionID, CellID string
	Ctx               context.Context
}

// CodeModeAuthority supplies captured local tools; live catalogue checks still narrow admission.
type CodeModeAuthority interface {
	Tool(string) (Tool, bool)
	Close()
}
type CodeModeAuthorityCapture func(context.Context, CodeModeCellRef, []string) (CodeModeAuthority, error)

// CodeModeCellSummary is a bounded process-local lifecycle projection, without script or output bodies.
type CodeModeCellSummary struct {
	CellID         string `json:"cell_id"`
	Status         string `json:"status"`
	StartedAt      int64  `json:"started_at"`
	ElapsedMS      int64  `json:"elapsed_ms"`
	Live           bool   `json:"live"`
	HasMoreOutput  bool   `json:"has_more_output"`
	UnsettledCalls int    `json:"unsettled_calls"`
	WaitingCalls   int    `json:"waiting_calls,omitempty"`
	ErrorCode      string `json:"error_code,omitempty"`
}

// ContextWithCodeModeToolPause lets trusted approval transports park executing
// tool permits during a human wait. Resuming must succeed before dispatch.
func ContextWithCodeModeToolPause(ctx context.Context, pause func() (func() error, error)) context.Context {
	return context.WithValue(ctx, codeModePauseKey{}, pause)
}

type codeModePauseKey struct{}

func PauseCodeModeTool(ctx context.Context) (func() error, error) {
	if pause, _ := ctx.Value(codeModePauseKey{}).(func() (func() error, error)); pause != nil {
		return pause()
	}
	return func() error { return nil }, nil
}

// WithCodeModeApproval parks shared dispatch permits only during a human prompt.
func WithCodeModeApproval[T any](ctx context.Context, prompt func() (T, error)) (value T, err error) {
	resume, err := PauseCodeModeTool(ctx)
	if err != nil {
		return value, err
	}
	value, err = prompt()
	if resumed := resume(); err == nil {
		err = resumed
	}
	if err == nil {
		err = ctx.Err()
	}
	return
}

// LockCodeModeApproval parks shared execution permits while waiting for a
// prompt mutex, then reacquires them before any permission checks can succeed.
// The caller owns Unlock only after a nil result. A cancelled cell also cancels
// its lock holder's prompt, so queued cell calls cannot outlive an idle prompt.
func LockCodeModeApproval(ctx context.Context, lock *sync.Mutex) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	resume, err := PauseCodeModeTool(ctx)
	if err != nil {
		return err
	}
	lock.Lock()
	if err = resume(); err == nil {
		err = ctx.Err()
	}
	if err != nil {
		lock.Unlock()
	}
	return err
}
