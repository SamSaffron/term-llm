package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type sessionSink struct {
	mu    sync.Mutex
	kinds []string
	texts []string
}

func (s *sessionSink) emit(kind string, value json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kinds = append(s.kinds, kind)
	if kind == "text" {
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			text = string(value)
		}
		s.texts = append(s.texts, text)
	}
	return nil
}

func (s *sessionSink) snapshot() ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.kinds...), append([]string(nil), s.texts...)
}

func waitCellDone(t *testing.T, cell *Cell) Result {
	t.Helper()
	select {
	case <-cell.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("cell never finished")
	}
	result, ok := cell.Result()
	if !ok {
		t.Fatal("done cell has no result")
	}
	return result
}

func TestSessionLimitsAreInternallyConsistent(t *testing.T) {
	if SessionLimits.Heap != 256<<20 {
		t.Fatalf("heap %d", SessionLimits.Heap)
	}
	if SessionLimits.Linear < SessionLimits.Heap {
		t.Fatalf("WASM linear cap %d cannot contain heap %d", SessionLimits.Linear, SessionLimits.Heap)
	}
	if SessionLimits.StateTotal != 1<<20 || SessionLimits.StateValue != 256<<10 {
		t.Fatalf("state limits: %+v", SessionLimits)
	}
}

func TestSessionCellYieldResumeAndFreshHeap(t *testing.T) {
	sink := &sessionSink{}
	cell, err := StartCell(context.Background(), CellSpec{
		Input: Input{Code: `globalThis.marker='first'; await sleep(60); text('a'); await sleep(60); text('b'); store('x',1);`},
		Sink:  sink.emit,
	})
	if err != nil {
		t.Fatal(err)
	}
	status, _ := cell.Wait(context.Background(), 10*time.Millisecond)
	if status != "running" {
		t.Fatalf("early status %q", status)
	}
	result := waitCellDone(t, cell)
	if result.Status != "completed" || !result.StateCommitted || string(result.State) != `{"set":{"x":1},"delete":[]}` {
		t.Fatalf("result: %+v", result)
	}
	kinds, texts := sink.snapshot()
	if strings.Join(kinds, ",") != "text,text" || strings.Join(texts, ",") != "a,b" {
		t.Fatalf("sink order: %v %v", kinds, texts)
	}

	fresh, err := StartCell(context.Background(), CellSpec{Input: Input{Code: `if(globalThis.marker) throw Error('leak'); text('fresh');`}, Sink: sink.emit})
	if err != nil {
		t.Fatal(err)
	}
	freshResult := waitCellDone(t, fresh)
	if freshResult.Status != "completed" {
		t.Fatalf("fresh cell: %+v", freshResult)
	}
}

func TestSessionCellRequestCancellationDoesNotStop(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	sink := &sessionSink{}
	cell, err := StartCell(parent, CellSpec{
		Input: Input{Code: `await sleep(80); text('alive'); store('x',1);`},
		Sink:  sink.emit,
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	result := waitCellDone(t, cell)
	if result.Status != "completed" || !result.StateCommitted {
		t.Fatalf("request cancellation stopped the cell: %+v", result)
	}
	_, texts := sink.snapshot()
	if strings.Join(texts, ",") != "alive" {
		t.Fatalf("output: %v", texts)
	}
}

func TestSessionCellExplicitCancel(t *testing.T) {
	cell, err := StartCell(context.Background(), CellSpec{Input: Input{Code: `while(true){}`}})
	if err != nil {
		t.Fatal(err)
	}
	status, _ := cell.Wait(context.Background(), 20*time.Millisecond)
	if status != "running" {
		t.Fatalf("status before cancel %q", status)
	}
	cell.Cancel()
	result := waitCellDone(t, cell)
	if result.Status != "cancelled" || result.StateCommitted {
		t.Fatalf("cancel: %+v", result)
	}
}

func TestSessionStateLimitsAndImageHandles(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
	}{
		{"per-value", `store('big','x'.repeat(300*1024)); text('ok')`},
		{"image-handle", `store('h','cell-image:forged'); text('ok')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &sessionSink{}
			cell, err := StartCell(context.Background(), CellSpec{Input: Input{Code: tc.code}, Sink: sink.emit})
			if err != nil {
				t.Fatal(err)
			}
			result := waitCellDone(t, cell)
			if result.Status != "failed" || result.StateCommitted {
				t.Fatalf("invalid state committed: %+v", result)
			}
			if tc.name == "per-value" && !strings.Contains(result.Error, "state_value_limit") {
				t.Fatalf("error: %q", result.Error)
			}
			if tc.name == "image-handle" && !strings.Contains(result.Error, "state_image_handle") {
				t.Fatalf("error: %q", result.Error)
			}
			_, texts := sink.snapshot()
			if strings.Join(texts, ",") != "ok" {
				t.Fatalf("output lost: %v", texts)
			}
		})
	}
}

func TestSessionSinkRejectionFailsBeforeCommit(t *testing.T) {
	cell, err := StartCell(context.Background(), CellSpec{
		Input: Input{Code: `store('x',1); image('forged'); text('after');`},
		Sink: func(kind string, _ json.RawMessage) error {
			if kind == "image" {
				return errors.New("image emission: invalid handle")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := waitCellDone(t, cell)
	if result.Status != "failed" || result.StateCommitted || !strings.Contains(result.Error, "image emission") {
		t.Fatalf("sink rejection: %+v", result)
	}
}
