//go:build !js

package codemode

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tetratelabs/wazero"
)

func TestRuntimeConfigPlatform(t *testing.T) {
	for _, tc := range []struct {
		os, arch    string
		interpreter bool
	}{
		{"darwin", "arm64", true},
		{"darwin", "amd64", false},
		{"linux", "arm64", false},
		{"linux", "amd64", false},
		{"windows", "arm64", false},
		{"windows", "amd64", false},
	} {
		t.Run(tc.os+"/"+tc.arch, func(t *testing.T) {
			want := wazero.NewRuntimeConfig()
			if tc.interpreter {
				want = wazero.NewRuntimeConfigInterpreter()
			}
			// Compare the public constructor results, not wazero's private fields.
			// The arm64 Darwin branch must not silently return the compiler default.
			if got := runtimeConfig(tc.os, tc.arch); !reflect.DeepEqual(got, want) {
				t.Fatalf("runtime config for %s/%s: interpreter=%v required", tc.os, tc.arch, tc.interpreter)
			}
		})
	}
}

// Run this from a hardened-signed native test binary, not only `go test`:
// an unsigned test binary cannot detect executable-memory signing violations.
func TestScriptHello(t *testing.T) {
	const code = `text("hello");`
	t.Run("synchronous", func(t *testing.T) {
		r := Run(context.Background(), Input{Code: code}, nil, nil, nil)
		if r.Status != "completed" || !r.StateCommitted || len(r.Output) != 1 || r.Output[0].Kind != "text" || string(r.Output[0].Value) != `"hello"` {
			t.Fatalf("unexpected script result: %+v", r)
		}
	})
	t.Run("asynchronous", func(t *testing.T) {
		sink := &sessionSink{}
		cell, err := StartCell(context.Background(), CellSpec{
			Input: Input{Code: code},
			Sink:  sink.emit,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer cell.Cancel()
		r := waitCellDone(t, cell)
		if r.Status != "completed" || !r.StateCommitted || !json.Valid(r.State) {
			t.Fatalf("unexpected cell result: %+v", r)
		}
		kinds, texts := sink.snapshot()
		if !reflect.DeepEqual(kinds, []string{"text"}) || !reflect.DeepEqual(texts, []string{"hello"}) {
			t.Fatalf("unexpected emissions: %v %v", kinds, texts)
		}
	})
}
