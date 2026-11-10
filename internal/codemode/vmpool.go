package codemode

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tetratelabs/wazero/experimental"
)

const (
	MaxLiveCells           = 8
	MaxTerminalCells       = 8
	SessionToolConcurrency = 4
	SessionInflightCalls   = 64
	SessionImageBytes      = 32 << 20
	VMFloor                = 4 << 20
)

// Budget accounts live WASM backing capacity, not Go GC/copy transients or native tools.
// Every child is charged atomically with its process parent.
type Budget struct {
	limit, used int64
	parent      *Budget
}

var budgetMu sync.Mutex
var processMemory = &Budget{limit: 2 << 30}
var compileSlots = make(chan struct{}, 1) // bound cold-cache compiler working sets
var vmSlots = make(chan struct{}, Limits.VMs)

// NewSessionBudget limits one session to half the process linear-memory budget.
func NewSessionBudget() *Budget { return &Budget{limit: 1 << 30, parent: processMemory} }
func (b *Budget) reserve(n int64) bool {
	budgetMu.Lock()
	defer budgetMu.Unlock()
	for p := b; p != nil; p = p.parent {
		if n > p.limit-p.used {
			return false
		}
	}
	for p := b; p != nil; p = p.parent {
		p.used += n
	}
	return true
}
func (b *Budget) release(n int64) {
	budgetMu.Lock()
	defer budgetMu.Unlock()
	for p := b; p != nil; p = p.parent {
		p.used -= n
	}
}

type budgetMemory struct {
	mu      sync.Mutex
	budget  *Budget
	buf     []byte
	charged uint64
	maximum uint64
	denied  atomic.Bool
	freed   bool
}

func newBudgetMemory(b *Budget) *budgetMemory {
	if b == nil {
		b = processMemory
	}
	if !b.reserve(VMFloor) {
		return nil
	}
	return &budgetMemory{budget: b, charged: VMFloor}
}
func (m *budgetMemory) Allocate(_, maximum uint64) experimental.LinearMemory {
	m.maximum = maximum
	return m
}
func (m *budgetMemory) Reallocate(size uint64) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.freed || size > m.maximum {
		return nil
	}
	if size <= uint64(cap(m.buf)) {
		m.buf = m.buf[:size]
		return m.buf
	}
	target := min(m.maximum, max(size, uint64(cap(m.buf))*2))
	charge := func(n uint64) bool { return n <= m.charged || m.budget.reserve(int64(n-m.charged)) }
	if !charge(target) {
		target = size
		if !charge(target) {
			m.denied.Store(true)
			return nil
		}
	}
	next := make([]byte, size, target)
	copy(next, m.buf)
	m.buf = next
	m.charged = max(m.charged, target)
	return m.buf
}
func (m *budgetMemory) Free() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.freed {
		m.freed = true
		m.buf = nil
		m.budget.release(int64(m.charged))
	}
}

func withCompilationSlot[T any](ctx context.Context, compile func() (T, error)) (value T, err error) {
	select {
	case compileSlots <- struct{}{}:
	case <-ctx.Done():
		return value, ctx.Err()
	}
	defer func() { <-compileSlots }()
	return compile()
}
func aggregateMemoryError(message string) string {
	if message == "" || strings.Contains(message, "out of memory") || strings.Contains(message, "memory_limit") || strings.Contains(message, "<unprintable exception>") {
		return "memory_limit: aggregate linear-memory budget exceeded; " + message
	}
	return message
}
