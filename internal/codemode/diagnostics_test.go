//go:build !js

package codemode

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"
)

func TestBindingScriptDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, code, message, location string
	}{
		{"runtime", "const a = 1;\nawait Promise.resolve();\nthrow Error('bad \"quote\"');", `Error: bad "quote"`, "cell.js:3:7"},
		{"runtime-first-line", `throw Error('first');`, "Error: first", "cell.js:1:"},
		{"syntax", "const a = 1;\nconst b = 2;\nconst = 3;", "SyntaxError:", "cell.js:3:"},
		{"string", `throw 'plain "quote"';`, `plain "quote"`, ""},
		{"multiline-string", `throw 'first\nsecond\\literal';`, "first\nsecond\\literal", ""},
		{"null", `throw null;`, "null", ""},
		{"undefined", `throw undefined;`, "undefined", ""},
		{"number", `throw 42;`, "42", ""},
		{"throwing-stack", `throw {toString(){return 'original'},get stack(){throw Error('secondary')}};`, "original", ""},
		{"throwing-string", `throw {toString(){throw Error('secondary')},get stack(){throw Error('again')}};`, "<unprintable exception>", ""},
		{"mutated-string", `globalThis.String=()=>{throw Error('secondary')}; throw 'original';`, "original", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Run(context.Background(), Input{Code: tc.code}, nil, nil, nil)
			if r.Status != "failed" || r.StateCommitted {
				t.Fatalf("unexpected success: %+v", r)
			}
			want := "script_error: " + tc.message
			if tc.location == "" {
				if r.Error != want {
					t.Fatalf("diagnostic = %q, want %q", r.Error, want)
				}
			} else if !strings.HasPrefix(r.Error, want) || !strings.Contains(r.Error, "\n") || !strings.Contains(r.Error, tc.location) {
				t.Fatalf("diagnostic = %q, want prefix %q and location %q on an actual stack line", r.Error, want, tc.location)
			}
		})
	}
}

// Exercise the host exception path as well as the prelude's promise rejection
// path: primitive exceptions must not be probed for stack, and a throwing stack
// getter must not recursively replace the original diagnostic.
func TestBindingHostExceptionDiagnostics(t *testing.T) {
	ctx := context.Background()
	r := wazero.NewRuntimeWithConfig(ctx, runtimeConfig(runtime.GOOS, runtime.GOARCH).WithCompilationCache(compilationCache))
	defer r.Close(ctx)
	module, err := r.CompileModule(ctx, wasm)
	if err != nil {
		t.Fatal(err)
	}
	m := &machine{ctx: ctx, policy: legacyMachinePolicy()}
	if err := m.instantiate(r, module); err != nil {
		t.Fatal(err)
	}
	defer m.call("qjs_destroy")
	for _, tc := range []struct{ name, code, want string }{
		{"string", `throw 'original';`, "script_error: original"},
		{"null", `throw null;`, "script_error: null"},
		{"undefined", `throw undefined;`, "script_error: undefined"},
		{"throwing-stack", `throw {toString(){return 'original'},get stack(){throw Error('secondary')}};`, "script_error: original"},
		{"throwing-string", `throw {toString(){throw Error('secondary')},get stack(){throw Error('again')}};`, "script_error: <unprintable exception>"},
		{"oversized-stack", `throw {toString(){return 'original'},stack:'x'.repeat(65537)};`, "script_error: original"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.eval(tc.code, "cell.js")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("diagnostic = %v, want %q", err, tc.want)
			}
			if _, err := m.evalDiscard("1"); err != nil {
				t.Fatalf("secondary exception left VM unusable: %v", err)
			}
		})
	}
}

func TestBindingUnknownToolDiagnostics(t *testing.T) {
	h := &probeHost{}
	r := Run(context.Background(), Input{Code: `await tools.missing_tool({});`}, []string{"fast"}, h, nil)
	if r.Status != "failed" || r.StateCommitted {
		t.Fatalf("unexpected success: %+v", r)
	}
	for _, want := range []string{"script_error: Error: unknown_tool:", "missing_tool", "Object.keys(tools)", "describeTools"} {
		if !strings.Contains(r.Error, want) {
			t.Fatalf("diagnostic = %q, missing %q", r.Error, want)
		}
	}
	if len(h.order) != 0 {
		t.Fatalf("unknown tool dispatched: %v", h.order)
	}

	r = Run(context.Background(), Input{Code: `
const missing = tools.missing_tool;
if (typeof missing !== 'function') throw Error('lookup threw or failed');
if (Object.getPrototypeOf(tools) !== null || !Object.isFrozen(tools)) throw Error('registry changed');
for (const name of Reflect.ownKeys(Object.prototype)) {
  if (tools[name] !== undefined) throw Error('prototype callable: ' + name);
}
if (tools.then !== undefined || 'missing_tool' in tools) throw Error('phantom entry');
try { missing(); throw Error('missing did not throw'); }
catch (e) { if (e.code !== 'unknown_tool') throw e; }
text(Object.keys(tools));
text(await Promise.resolve(tools) === tools);
text((await tools.fast({})).n);
`}, []string{"fast"}, h, nil)
	if r.Status != "completed" || len(r.Output) != 3 || string(r.Output[0].Value) != `["fast"]` || string(r.Output[1].Value) != `true` || string(r.Output[2].Value) != `7` {
		t.Fatalf("registry invariants: %+v", r)
	}
	if strings.Join(h.order, ",") != "fast" {
		t.Fatalf("dispatches: %v", h.order)
	}
}

func TestBindingDiagnosticRollbackAndPartialOutput(t *testing.T) {
	for _, failure := range []string{`throw Error('original');`, `await tools.missing_tool({});`} {
		t.Run(failure, func(t *testing.T) {
			in := Input{Code: `text('partial'); store('x',2); ` + failure, State: json.RawMessage(`{"x":1}`)}
			r := Run(context.Background(), in, nil, nil, nil)
			if r.Status != "failed" || r.StateCommitted || len(r.State) != 0 || len(r.Output) != 1 || string(r.Output[0].Value) != `"partial"` {
				t.Fatalf("failure lost output or committed state: %+v", r)
			}
			if string(in.State) != `{"x":1}` {
				t.Fatalf("initial state mutated: %s", in.State)
			}

			sink := &sessionSink{}
			cell, err := StartCell(context.Background(), CellSpec{Input: in, Sink: sink.emit})
			if err != nil {
				t.Fatal(err)
			}
			r = waitCellDone(t, cell)
			_, texts := sink.snapshot()
			if r.Status != "failed" || r.StateCommitted || len(r.State) != 0 || strings.Join(texts, ",") != "partial" {
				t.Fatalf("session failure lost output or committed state: %+v, output %v", r, texts)
			}
		})
	}
}
