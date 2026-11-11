//go:build !js

package codemode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSettleInOrder(t *testing.T) {
	result := Run(context.Background(), Input{Code: `
 const resolvers=[];
 const p=[0,1,2].map(()=>new Promise(r=>resolvers.push(r)));
 const it=settleInOrder(p);
 resolvers[2]('c'); resolvers[0]('a'); resolvers[1]('b');
 for await(const r of it) text(r);
 for await(const r of settleInOrder([])) throw Error('empty');`}, nil, nil, nil)
	if result.Status != "completed" || len(result.Output) != 3 {
		t.Fatalf("%+v", result)
	}
	for i, want := range []string{`{"index":2,"status":"fulfilled","value":"c"}`, `{"index":0,"status":"fulfilled","value":"a"}`, `{"index":1,"status":"fulfilled","value":"b"}`} {
		if string(result.Output[i].Value) != want {
			t.Fatalf("%s != %s", result.Output[i].Value, want)
		}
	}
}

type settlementHost struct{ release, started chan struct{} }

func (h *settlementHost) Call(ctx context.Context, name string, _ json.RawMessage) (json.RawMessage, error) {
	if name == "slow" {
		close(h.started)
		select {
		case <-h.release:
			return json.RawMessage(`{"isError":true,"text":"resolved error"}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	<-h.started
	return nil, fmt.Errorf("bridge rejected")
}
func (*settlementHost) Describe(context.Context, []string) (json.RawMessage, error) {
	return json.RawMessage(`[]`), nil
}

func TestSettleInOrderAsyncEarlyExit(t *testing.T) {
	host := &settlementHost{release: make(chan struct{}), started: make(chan struct{})}
	output := make(chan string, 4)
	cell, err := StartCell(context.Background(), CellSpec{Names: []string{"slow", "reject"}, Host: host, Input: Input{Code: `
 const calls=[tools.slow({}),tools.reject({})];
 for await(const r of settleInOrder(calls)) {
   if(r.index!==1 || r.status!=='rejected' || !String(r.reason).includes('bridge rejected')) throw Error('wrong first result');
   text('rejected first'); break;
 }
 const [r]=await Promise.all([calls[0]]);
 if(!r.isError) throw Error('resolved error flag lost');
 store('finished',true); text('slow settled');`}, Sink: func(_ string, value json.RawMessage) error { output <- string(value); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer cell.Cancel()
	select {
	case got := <-output:
		if got != `"rejected first"` {
			t.Fatal(got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no early result")
	}
	select {
	case <-cell.Done():
		t.Fatal("iterator exit stopped dispatched work")
	default:
	}
	close(host.release)
	result := waitCellDone(t, cell)
	if result.Status != "completed" || !result.StateCommitted {
		t.Fatalf("%+v", result)
	}
	if got := <-output; got != `"slow settled"` {
		t.Fatal(got)
	}
}

func TestSettleInOrderKeepsCallLimit(t *testing.T) {
	result := Run(context.Background(), Input{Code: `const p=Array.from({length:65},()=>sleep(0)); for await(const r of settleInOrder(p)) { if(r.status==='rejected') throw r.reason; }`}, nil, nil, nil)
	if result.Status != "failed" || !strings.Contains(result.Error, "call_limit") {
		t.Fatalf("%+v", result)
	}
}

func TestSettleInOrderBreakStillRequiresAwait(t *testing.T) {
	host := &settlementHost{release: make(chan struct{}), started: make(chan struct{})}
	cell, err := StartCell(context.Background(), CellSpec{Names: []string{"slow", "reject"}, Host: host, Input: Input{Code: `
 const calls=[tools.slow({}),tools.reject({})];
 for await(const r of settleInOrder(calls)) break;
 store('notCommitted',true);`}})
	if err != nil {
		t.Fatal(err)
	}
	defer cell.Cancel()
	defer close(host.release)
	result := waitCellDone(t, cell)
	if result.Status != "failed" || result.StateCommitted || !strings.Contains(result.Error, "unawaited_calls") {
		t.Fatalf("%+v", result)
	}
}
