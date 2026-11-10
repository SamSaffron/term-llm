//go:build !js

package codemode

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"sync"
	"testing"
	"time"
)

type probeHost struct {
	mu    sync.Mutex
	order []string
	slow  chan struct{}
}

func (h *probeHost) Call(ctx context.Context, n string, a json.RawMessage) (json.RawMessage, error) {
	if n == "slow" {
		select {
		case <-h.slow:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	} else if h.slow != nil {
		defer close(h.slow)
	}
	h.mu.Lock()
	h.order = append(h.order, n)
	h.mu.Unlock()
	return json.RawMessage(`{"n":7}`), nil
}
func (h *probeHost) Describe(context.Context, []string) (json.RawMessage, error) {
	return json.RawMessage(`[]`), nil
}

type probeModules map[string]string

func (m probeModules) Normalize(base, spec string) (string, error) {
	if spec == "skill:probe" {
		return "skill:probe/index.mjs", nil
	}
	if strings.HasPrefix(spec, "./") && !strings.Contains(spec, "..") {
		return path.Join(path.Dir(base), spec), nil
	}
	return "", fmt.Errorf("forbidden module: %s", spec)
}
func (m probeModules) Source(n string) (string, error) {
	s, ok := m[n]
	if !ok {
		return "", fmt.Errorf("missing module: %s", n)
	}
	return s, nil
}
func TestBindingAsyncAndModules(t *testing.T) {
	h := &probeHost{slow: make(chan struct{})}
	modules := probeModules{"skill:probe/index.mjs": `import {x} from './dep.mjs'; await Promise.resolve(); export const answer=x+1;`, "skill:probe/dep.mjs": `export const x=41;`}
	r := Run(context.Background(), Input{Code: `const [a,b]=await Promise.all([tools.slow({}),tools.fast({})]); const m=await import('skill:probe'); text([a.n,b.n,m.answer]); store('x',42);`}, []string{"slow", "fast"}, h, modules)
	if r.Status != "completed" || string(r.State) != `{"x":42}` {
		t.Fatalf("%+v", r)
	}
	if len(r.Output) != 1 || string(r.Output[0].Value) != `[7,7,42]` {
		t.Fatalf("%+v", r.Output)
	}
	if strings.Join(h.order, ",") != "fast,slow" {
		t.Fatal(h.order)
	}
}
func TestBindingLimitsAndFreshness(t *testing.T) {
	for _, code := range []string{`await import('node:fs')`, `await import('skill:missing')`, `function f(){return f()} f()`, `const a=[]; while(true) a.push(new Array(10000).fill('x'))`, `text('partial'); store('x',1); throw Error('no')`, `await new Promise(()=>{})`, `void tools.fast({});`, `store('bad', NaN);`} {
		t.Run(code[:min(24, len(code))], func(t *testing.T) {
			r := Run(context.Background(), Input{Code: code, TimeoutMS: 5000}, []string{"fast"}, &probeHost{}, nil)
			if r.Status == "completed" || r.StateCommitted {
				t.Fatalf("unexpected success: %+v", r)
			}
			t.Log(r.Error)
		})
	}
	for range 12 {
		r := Run(context.Background(), Input{Code: `if(globalThis.leak) throw Error('leak');globalThis.leak=1;return 2;`}, nil, nil, nil)
		if r.Status != "completed" {
			t.Fatalf("%+v", r)
		}
	}
}
func TestBindingCancellation(t *testing.T) {
	for _, code := range []string{`while(true){}`, `await new Promise(()=>{function spin(){Promise.resolve().then(spin)} spin()})`} {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		r := Run(ctx, Input{Code: code}, nil, nil, nil)
		cancel()
		if r.Status != "cancelled" {
			t.Fatalf("%+v", r)
		}
	}
}

func TestBindingModuleAwaitsHostAndNoRetry(t *testing.T) {
	h := &probeHost{}
	modules := probeModules{"skill:probe/index.mjs": `const r=await tools.fast({}); export const answer=r.n;`}
	r := Run(context.Background(), Input{Code: `const m=await import('skill:probe'); return m.answer;`}, []string{"fast"}, h, modules)
	if r.Status != "completed" || len(r.Output) != 1 || string(r.Output[0].Value) != `7` || len(h.order) != 1 {
		t.Fatalf("%+v %+v", r, h.order)
	}
}
func TestBindingJSONAndOutputLimits(t *testing.T) {
	for _, code := range []string{`const x={};x.x=x;store('x',x);`, `store('x',1n);`, `store('x',{f:()=>1});`, `store('x',[undefined]);`, `Number.isFinite=()=>true;store('x',NaN);`, `text('x'.repeat(65537));`, `for(let i=0;i<129;i++)text('x');`, `for(let i=0;i<65;i++)void sleep(1);`} {
		r := Run(context.Background(), Input{Code: code}, nil, nil, nil)
		if r.Status == "completed" || r.StateCommitted {
			t.Fatalf("%s: %+v", code, r)
		}
	}
	r := Run(context.Background(), Input{Code: `store('__proto__',{hello:7});text(load('missing')===undefined);`, State: json.RawMessage(`{}`)}, nil, nil, nil)
	if r.Status != "completed" || string(r.State) != `{"__proto__":{"hello":7}}` {
		t.Fatalf("%+v", r)
	}
}
func BenchmarkWarmCell(b *testing.B) {
	for b.Loop() {
		r := Run(context.Background(), Input{Code: `return 42;`}, nil, nil, nil)
		if r.Status != "completed" {
			b.Fatal(r.Error)
		}
	}
}
