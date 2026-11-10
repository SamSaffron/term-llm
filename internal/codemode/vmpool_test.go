//go:build !js

package codemode

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

type measuredHost struct{ started chan struct{} }

func (h *measuredHost) Call(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	h.started <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (*measuredHost) Describe(context.Context, []string) (json.RawMessage, error) {
	return json.RawMessage(`[]`), nil
}
func TestManySessionLightweightMemory(t *testing.T) {
	// Eight sessions, eight real awaiting VMs each. No reservation of their max heaps.
	sessions, cellsPerSession := 8, 8
	if n, err := strconv.Atoi(os.Getenv("TERM_LLM_TEST_CODEMODE_SESSIONS")); err == nil && n > 0 && n <= 32 {
		sessions = n
	}
	warm := Run(context.Background(), Input{Code: `return 1`}, nil, nil, nil)
	if warm.Status != "completed" {
		t.Fatal(warm)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	baselineRSS, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(baselineRSS), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			t.Log("baseline " + l)
		}
	}
	h := &measuredHost{started: make(chan struct{}, sessions*cellsPerSession)}
	var cells []*Cell
	for range sessions {
		budget := NewSessionBudget()
		for range cellsPerSession {
			c, err := StartCell(context.Background(), CellSpec{Input: Input{Code: `await tools.hold({})`}, Names: []string{"hold"}, Host: h, Memory: budget})
			if err != nil {
				t.Fatal(err)
			}
			cells = append(cells, c)
		}
	}
	defer func() {
		for _, c := range cells {
			c.Cancel()
		}
		for _, c := range cells {
			waitCellDone(t, c)
		}
		budgetMu.Lock()
		remaining := processMemory.used
		budgetMu.Unlock()
		if remaining != 0 {
			t.Errorf("VM memory not reclaimed: %d", remaining)
		}
	}()
	for range sessions * cellsPerSession {
		select {
		case <-h.started:
		case <-time.After(20 * time.Second):
			t.Fatal("many-session admission failed")
		}
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	budgetMu.Lock()
	used := processMemory.used
	budgetMu.Unlock()
	rss, _ := os.ReadFile("/proc/self/status")
	var resident string
	for _, l := range strings.Split(string(rss), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			resident = l
		}
	}
	t.Logf("%d sessions x %d awaiting cells: charged linear=%d bytes (%.2f MiB/cell), Go heap delta=%d bytes, process %s", sessions, cellsPerSession, used, float64(used)/float64(sessions*cellsPerSession*(1<<20)), int64(after.HeapAlloc)-int64(before.HeapAlloc), resident)
	if used > int64(sessions*cellsPerSession*8)<<20 {
		t.Fatalf("unexpected lightweight charge: %d", used)
	}
}
func TestBudgetMemoryGrowthAndReclaim(t *testing.T) {
	parent := &Budget{limit: 12 << 20}
	b := &Budget{limit: 8 << 20, parent: parent}
	m := newBudgetMemory(b)
	if m == nil {
		t.Fatal("floor")
	}
	m.Allocate(0, 64<<20)
	buf := m.Reallocate(2 << 20)
	buf[len(buf)-1] = 42
	buf = m.Reallocate(3 << 20)
	buf[len(buf)-1] = 43
	buf = m.Reallocate(5 << 20)
	if buf[(2<<20)-1] != 42 || buf[(3<<20)-1] != 43 {
		t.Fatal("growth lost bytes")
	}
	if m.Reallocate(9<<20) != nil || !m.denied.Load() {
		t.Fatal("aggregate budget bypass")
	}
	m.Free()
	m.Free()
	if b.used != 0 || parent.used != 0 {
		t.Fatal("charge leaked")
	}
	tiny := &Budget{limit: VMFloor - 1}
	if newBudgetMemory(tiny) != nil {
		t.Fatal("floor must fail before instantiation")
	}
}

func TestAggregateMemoryFailureOnlyAffectsOwningCell(t *testing.T) {
	small := &Budget{limit: 8 << 20, parent: processMemory}
	cell, err := StartCell(context.Background(), CellSpec{Memory: small, Input: Input{Code: `const a=[];for(let i=0;i<100;i++)a.push('x'.repeat(1024*1024));store('bad',true)`}})
	if err != nil {
		t.Fatal(err)
	}
	result := waitCellDone(t, cell)
	if result.Status != "failed" || !strings.Contains(result.Error, "memory_limit: aggregate") || result.StateCommitted {
		t.Fatalf("aggregate OOM: %+v", result)
	}
	if small.used != 0 {
		t.Fatalf("OOM leaked %d bytes", small.used)
	}
	other, err := StartCell(context.Background(), CellSpec{Memory: NewSessionBudget(), Input: Input{Code: `return 'unaffected'`}})
	if err != nil {
		t.Fatal(err)
	}
	if result := waitCellDone(t, other); result.Status != "completed" {
		t.Fatalf("OOM crossed session: %+v", result)
	}
}
