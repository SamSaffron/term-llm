package codemode

import (
	"context"
	"encoding/json"
	"time"
)

// Limits is the single resource policy for synchronous cells.
var Limits = struct {
	Code, State, Arguments, RawResult, Calls, Children, ProcessChildren, VMs int
	Heap, Linear, Stack, Text, Items, Images, ImageBytes                     int
	CPU, Wall, MaxWall                                                       time.Duration
}{64 << 10, 64 << 10, 256 << 10, 4 << 20, 64, 4, 16, 4, 64 << 20, 128 << 20, 512 << 10, 64 << 10, 128, 4, 16 << 20, 2 * time.Second, 120 * time.Second, 300 * time.Second}

// Host dispatches admitted tools. Implementations own authority and actual-call leases.
type Host interface {
	Call(context.Context, string, json.RawMessage) (json.RawMessage, error)
	Describe(context.Context, []string) (json.RawMessage, error)
}

// Modules resolves only immutable, admitted source snapshots.
type Modules interface {
	Normalize(base, specifier string) (string, error)
	Source(name string) (string, error)
}

// Input is one async-function body and explicit caller-carried JSON state.
type Input struct {
	Code      string          `json:"code"`
	State     json.RawMessage `json:"state,omitempty"`
	TimeoutMS int             `json:"timeout_ms,omitempty"`
}

// Emission is explicit ordered output; image values are validated by the host adapter.
type Emission struct {
	Kind  string          `json:"kind"`
	Value json.RawMessage `json:"value"`
}

// Result never publishes staged state on failure. External effects are not rolled back.
type Result struct {
	Status         string          `json:"status"`
	Error          string          `json:"error,omitempty"`
	StateCommitted bool            `json:"state_committed"`
	State          json.RawMessage `json:"state,omitempty"`
	Output         []Emission      `json:"output,omitempty"`
}

// Error is a stable bridge failure, distinct from a resolved tool isError envelope.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	CallID  string `json:"call_id,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }
