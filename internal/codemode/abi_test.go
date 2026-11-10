//go:build !js

package codemode

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/tetratelabs/wazero"
)

func TestPinnedABI(t *testing.T) {
	if got := fmt.Sprintf("%x", sha256.Sum256(wasm)); got != "d4c9375f2b1ca4dc95f72c8aa2982a7a9951ac8011490d79c6582df732b4bbd9" {
		t.Fatal(got)
	}
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)
	mod, err := r.CompileModule(ctx, wasm)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range mod.ImportedFunctions() {
		ns, n, _ := f.Import()
		t.Logf("import %s.%s %v -> %v", ns, n, f.ParamTypes(), f.ResultTypes())
		if ns != "env" && ns != "wasi_snapshot_preview1" {
			t.Fatal("unexpected import", ns)
		}
	}
	for n, f := range mod.ExportedFunctions() {
		t.Logf("export %s %v -> %v", n, f.ParamTypes(), f.ResultTypes())
	}
}

type lateHost struct{ started, release, finished chan struct{} }

func (h *lateHost) Call(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	close(h.started)
	<-h.release
	defer close(h.finished)
	return json.RawMessage(`null`), nil
}
func (h *lateHost) Describe(context.Context, []string) (json.RawMessage, error) {
	return json.RawMessage(`[]`), nil
}
func TestBindingLateCompletion(t *testing.T) {
	h := &lateHost{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan Result, 1)
	go func() {
		result <- Run(ctx, Input{Code: `await tools.late({}); text('not emitted');`}, []string{"late"}, h, nil)
	}()
	select {
	case <-h.started:
	case early := <-result:
		cancel()
		t.Fatalf("cell failed before host start: %+v", early)
	}
	cancel()
	r := <-result
	if r.Status != "cancelled" || len(r.Output) != 0 {
		t.Fatalf("%+v", r)
	}
	close(h.release)
	<-h.finished
	r = Run(context.Background(), Input{Code: `return 1;`}, nil, nil, nil)
	if r.Status != "completed" {
		t.Fatalf("%+v", r)
	}
}
