package codemode

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Tool names for the asynchronous lifecycle surface. The legacy Name value in
// package tool remains a separate explicit compatibility path.
const (
	LegacyToolName = "code_mode"
	ExecToolName   = "spawn_script"
	WaitToolName   = "wait_script"
	CancelToolName = "cancel_script"
)

// DefaultOutputTokens is the default visible-output budget for one exec or
// wait observation. The engine applies the same 4-bytes-per-token estimate
// used elsewhere, so the byte conversion is explicit rather than pretending to
// be an exact token count. Images are budgeted separately by count and decoded
// bytes. Retained undelivered output has its own bounded collector cap.
const DefaultOutputTokens = 10000

// MaxOutputTokens bounds one observation's text budget.
const MaxOutputTokens = 100000

// SessionLimits is the resource policy for engine-owned asynchronous cells.
// The heap and the WASM linear memory are deliberately larger than the legacy
// synchronous policy: a 256 MiB QuickJS heap cannot live inside a 128 MiB
// linear memory cap.
var SessionLimits = struct {
	Code, Text, Items, Images, ImageBytes       int
	StateTotal, StateValue, Heap, Linear, Stack int
	RetainBytes                                 int
	CPU, Yield, MaxYield                        time.Duration
}{
	Code:        64 << 10,
	Text:        4 << 20,
	Items:       1024,
	Images:      16,
	ImageBytes:  16 << 20,
	StateTotal:  1 << 20,
	StateValue:  256 << 10,
	Heap:        256 << 20,
	Linear:      512 << 20,
	Stack:       512 << 10,
	RetainBytes: 1 << 20,
	CPU:         2 * time.Second,
	Yield:       60 * time.Second,
	MaxYield:    600 * time.Second,
}

// CellSpec starts one fresh QuickJS cell. The engine owns the context and
// cancels it explicitly; the spec callback runs on the machine goroutine and
// must only copy data.
type CellSpec struct {
	Input   Input
	Names   []string
	Host    Host
	Modules Modules
	// Sink receives explicit text/image emissions in order. It may copy or
	// reject an emission (for example an invalid image handle) by returning an
	// error, which fails the cell before any state commit.
	Sink func(kind string, value json.RawMessage) error
	// OnFinish runs exactly once after the machine exits and before waiters see
	// the terminal state. It must not call back into the cell's WASM instance.
	OnFinish func(Result)
}

// Cell is the engine-independent handle for one async QuickJS machine. A cell
// runs in its own goroutine; Cancel is the only path that stops it.
type Cell struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	changed chan struct{}
	status  string
	err     string
	state   json.RawMessage
	okState bool
	done    bool
	doneCh  chan struct{}
}

func newCell(ctx context.Context, cancel context.CancelFunc) *Cell {
	return &Cell{ctx: ctx, cancel: cancel, changed: make(chan struct{}), status: "running", doneCh: make(chan struct{})}
}

func (c *Cell) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// Context is the cell-owned context passed to all host work.
func (c *Cell) Context() context.Context { return c.ctx }

// Cancel requests cell cancellation. It never waits for a host tool that
// ignores its context; the engine keeps that call's actual-effect lease.
func (c *Cell) Cancel() { c.cancel() }

// Done is closed after OnFinish has observed the terminal machine result.
func (c *Cell) Done() <-chan struct{} { return c.doneCh }

func (c *Cell) complete(result Result) {
	c.mu.Lock()
	c.status = result.Status
	c.err = result.Error
	c.state = append(json.RawMessage(nil), result.State...)
	c.okState = result.StateCommitted
	c.done = true
	c.notifyLocked()
	c.mu.Unlock()
	close(c.doneCh)
}

// Result returns the machine result once the cell is done.
func (c *Cell) Result() (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.done {
		return Result{}, false
	}
	state := append(json.RawMessage(nil), c.state...)
	return Result{Status: c.status, Error: c.err, StateCommitted: c.okState, State: state}, true
}

// Wait blocks until the cell finishes or the yield budget elapses. A yield is
// never a cancellation; Status stays "running" and callers may wait again.
func (c *Cell) Wait(ctx context.Context, yield time.Duration) (status string, err string) {
	if yield <= 0 {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.status, c.err
	}
	timer := time.NewTimer(yield)
	defer timer.Stop()
	for {
		c.mu.Lock()
		if c.done {
			status, err = c.status, c.err
			c.mu.Unlock()
			return status, err
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			c.mu.Lock()
			status, err = c.status, c.err
			c.mu.Unlock()
			return status, err
		case <-ctx.Done():
			c.mu.Lock()
			status, err = c.status, c.err
			c.mu.Unlock()
			return status, err
		}
	}
}
