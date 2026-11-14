package engine

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/term-llm/term-llm/internal/codemode"
	"github.com/term-llm/term-llm/internal/llm/core"
	corestream "github.com/term-llm/term-llm/internal/llm/core/stream"
)

type codeModeLeasedRunner struct {
	lease *core.ClientToolLease
	run   func(context.Context, core.ToolCall) (core.ToolOutput, error)
}

func (r codeModeLeasedRunner) ClientToolLease() *core.ClientToolLease { return r.lease }
func (r codeModeLeasedRunner) OwnsClientTool(name string) bool {
	_, ok := r.lease.Spec(name)
	return ok
}
func (r codeModeLeasedRunner) RunClientTool(ctx context.Context, call core.ToolCall) (core.ToolOutput, error) {
	return r.run(ctx, call)
}

func codeModeClientsFixture(t *testing.T, name string) (context.Context, *core.ClientToolLease) {
	t.Helper()
	specs := []core.ToolSpec{{Name: name, Description: "original client schema", Schema: map[string]any{"type": "object", "properties": map[string]any{"message": map[string]any{"type": "string"}}}, OutputSchema: map[string]any{"type": "string"}}}
	lease, err := core.NewClientToolLease(core.ClientToolBinding{ResponseID: "resp_original", SessionID: "session_original", UserID: "user_original", ClientID: "client_original", Generation: "page_original"}, specs)
	if err != nil {
		t.Fatal(err)
	}
	runner := codeModeLeasedRunner{lease: lease, run: func(ctx context.Context, call core.ToolCall) (core.ToolOutput, error) {
		if err := core.BeginClientToolDispatch(ctx); err != nil {
			return core.ToolOutput{}, err
		}
		return core.TextOutput("phone:" + call.ID + ":" + string(call.Arguments)), nil
	}}
	return core.ScopeClientToolRunner(core.ContextWithClientToolRunner(context.Background(), runner), specs), lease
}

func TestCodeModeClientsForegroundSnapshotAndResult(t *testing.T) {
	ctx, lease := codeModeClientsFixture(t, "webmcp__phone")
	clients, err := captureCodeModeClients(ctx, nil)
	if err != nil || clients == nil {
		t.Fatalf("capture: %v", err)
	}
	id, handled, err := clients.identity("webmcp__phone", nil)
	if err != nil || !handled || id.spec.Description != "original client schema" || id.spec.OutputSchema["type"] != "string" {
		t.Fatalf("identity: %+v %v", id, err)
	}
	if clients.identities()["webmcp__phone"].hash != id.hash {
		t.Fatal("ceiling identity changed")
	}
	tool, ok := clients.tool("webmcp__phone")
	if !ok || !isCodeModeClientTool(tool) || !tool.(core.CodeModeExposureProvider).CodeModeExposure().Allowed {
		t.Fatal("client override absent or not exposed")
	}
	var paused, resumed bool
	callCtx := core.ContextWithCallID(context.Background(), "cell_call")
	callCtx = core.ContextWithCodeModeToolPause(callCtx, func() (func() error, error) {
		paused = true
		return func() error { resumed = true; return nil }, nil
	})
	out, err := tool.Execute(callCtx, json.RawMessage(`{"message":"hello"}`))
	if err != nil || out.Content != `phone:cell_call:{"message":"hello"}` || !paused || !resumed {
		t.Fatalf("foreground output=%+v err=%v paused=%v resumed=%v", out, err, paused, resumed)
	}
	lease.Revoke("response ended; cell survives")
	_, handled, err = clients.identity("webmcp__phone", nil)
	var failure *codemode.Error
	if !handled || !errors.As(err, &failure) || failure.Code != "client_binding_expired" || failure.Outcome != "not_dispatched" {
		t.Fatalf("expired identity: %v", err)
	}
	if _, ok := clients.tool("server_only"); ok {
		t.Fatal("client resolver fell back to unrelated server tool")
	}
	_, handled, err = clients.identity("server_only", nil)
	if handled || err != nil {
		t.Fatal("client expiry affected server tool resolution")
	}
	// A fresh response/runner in ambient context cannot renew this override.
	freshCtx, _ := codeModeClientsFixture(t, "webmcp__phone")
	_, err = tool.Execute(core.ContextWithCallID(freshCtx, "late_call"), json.RawMessage(`{}`))
	var clientErr *core.ClientToolError
	if !errors.As(err, &clientErr) || clientErr.Code != "client_binding_expired" || clientErr.Outcome != "not_dispatched" {
		t.Fatalf("fresh response rebound old cell: %v", err)
	}
}

func TestCodeModeClientCollisionsAndExactOrigin(t *testing.T) {
	for _, name := range []string{"shell", "webmcp__phone", codemode.ExecToolName} {
		t.Run(name, func(t *testing.T) {
			ctx, _ := codeModeClientsFixture(t, name)
			_, err := captureCodeModeClients(ctx, map[string]core.Tool{name: nil})
			var failure *codemode.Error
			if !errors.As(err, &failure) || failure.Code != "client_tool_collision" {
				t.Fatalf("collision admitted: %v", err)
			}
		})
	}
	ctx, _ := codeModeClientsFixture(t, "webmcp__phone")
	clients, _ := captureCodeModeClients(ctx, nil)
	_, handled, err := clients.identity("webmcp__phone", map[string]core.Tool{"webmcp__phone": nil})
	if !handled || err == nil {
		t.Fatal("later server replacement rerouted client capability")
	}
	// A different response/client generation must produce different identity.
	spec := clients.identities()["webmcp__phone"].spec
	other, err := core.NewClientToolLease(core.ClientToolBinding{ResponseID: "resp_other", SessionID: "session_original", ClientID: "other_device", Generation: "other_page"}, []core.ToolSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	runner := codeModeLeasedRunner{lease: other}
	otherCtx := core.ScopeClientToolRunner(core.ContextWithClientToolRunner(context.Background(), runner), []core.ToolSpec{spec})
	otherClients, _ := captureCodeModeClients(otherCtx, nil)
	if clients.identities()[spec.Name].hash == otherClients.identities()[spec.Name].hash {
		t.Fatal("identity omitted response/client provenance")
	}
}

func TestCodeModeClientUnsupportedAndChildFailClosed(t *testing.T) {
	plain := testClientToolRunner{run: map[string]func(context.Context, core.ToolCall) (core.ToolOutput, error){"phone": func(context.Context, core.ToolCall) (core.ToolOutput, error) {
		return core.TextOutput("direct only"), nil
	}}}
	ctx := core.ScopeClientToolRunner(core.ContextWithClientToolRunner(context.Background(), plain), []core.ToolSpec{{Name: "phone", Schema: map[string]any{"type": "object"}}})
	if clients, err := captureCodeModeClients(ctx, nil); err != nil || clients != nil {
		t.Fatalf("unsupported transport admitted: %v", err)
	}
	if core.ClientTool(ctx, "phone") == nil {
		t.Fatal("fail-closed script policy broke direct runner")
	}
	parent, _ := codeModeClientsFixture(t, "phone")
	clients, _ := captureCodeModeClients(parent, nil)
	child := core.ScopeClientToolRunner(parent, []core.ToolSpec{clients.identities()["phone"].spec})
	if clients, err := captureCodeModeClients(child, nil); clients != nil || err != nil {
		t.Fatal("child inherited parent's capability")
	}
}

func TestCodeModeClientActualCellCatalogueAndServerIndependence(t *testing.T) {
	ctx, lease := codeModeClientsFixture(t, "phone")
	e := newCodeEngine()
	e.RegisterTool(codeTestTool{name: "server", run: func(context.Context, json.RawMessage) (core.ToolOutput, error) {
		return core.TextOutput("server survives"), nil
	}})
	// Client offer is not server authority; exclude phone from the local list.
	e.SetAllowedToolsFilter([]string{codemode.LegacyToolName, "server"})
	cell, err := e.admitCodeCell(ctx, "outer", corestream.Sender{})
	if err != nil {
		t.Fatal(err)
	}
	defer cell.Close()
	if got := strings.Join(cell.Names(), ","); got != "phone,server" {
		t.Fatalf("catalogue: %s", got)
	}
	if _, registered := e.tools.Get("phone"); registered {
		t.Fatal("client was globally registered")
	}
	description, err := cell.Describe(context.Background(), []string{"phone"})
	if err != nil || !strings.Contains(string(description), "original client schema") || !strings.Contains(string(description), "outputSchema") {
		t.Fatalf("description=%s err=%v", description, err)
	}
	result := codemode.Run(context.Background(), codemode.Input{Code: `text((await tools.phone({message:'foreground'})).text)`}, cell.Names(), cell, cell)
	if result.Status != "completed" || len(result.Output) != 1 || !strings.Contains(string(result.Output[0].Value), "foreground") {
		t.Fatalf("foreground cell: %+v", result)
	}
	lease.Revoke("owner response ended")
	_, err = cell.Call(context.Background(), "phone", json.RawMessage(`{}`))
	var failure *codemode.Error
	if !errors.As(err, &failure) || failure.Code != "client_binding_expired" || failure.Outcome != "not_dispatched" {
		t.Fatalf("post-expiry client call: %v", err)
	}
	raw, err := cell.Call(context.Background(), "server", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(raw), "server survives") {
		t.Fatalf("client expiry cancelled server tool: %s %v", raw, err)
	}
	outcomes := cell.Outcomes()
	if len(outcomes) != 3 || outcomes[1].Outcome != "not_dispatched" || outcomes[2].Outcome != "completed" {
		t.Fatalf("audit: %+v", outcomes)
	}
}

func TestCodeModeClientActualCellPermitWaitExpiry(t *testing.T) {
	ctx, lease := codeModeClientsFixture(t, "phone")
	e := newCodeEngine()
	cell, err := e.admitCodeCell(ctx, "outer", corestream.Sender{})
	if err != nil {
		t.Fatal(err)
	}
	defer cell.Close()
	for range cap(cell.permits) {
		cell.permits <- struct{}{}
	}
	done := make(chan error, 1)
	go func() { _, err := cell.Call(context.Background(), "phone", json.RawMessage(`{}`)); done <- err }()
	for {
		cell.mu.Lock()
		waiting := cell.waiting
		cell.mu.Unlock()
		if waiting > 0 {
			break
		}
		runtime.Gosched()
	}
	lease.Revoke("response ended during permit wait")
	<-cell.permits
	select {
	case err := <-done:
		var failure *codemode.Error
		if !errors.As(err, &failure) || failure.Code != "client_binding_expired" || failure.Outcome != "not_dispatched" {
			t.Fatalf("permit expiry: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("permit leaked")
	}
	for len(cell.permits) > 0 {
		<-cell.permits
	}
}

func TestCodeModeClientAsyncObservationTimeoutAndResponseEnd(t *testing.T) {
	base, lease := codeModeClientsFixture(t, "phone")
	spec, _ := lease.Spec("phone")
	started := make(chan struct{})
	runner := codeModeLeasedRunner{lease: lease, run: func(ctx context.Context, _ core.ToolCall) (core.ToolOutput, error) {
		if err := core.BeginClientToolDispatch(ctx); err != nil {
			return core.ToolOutput{}, err
		}
		close(started)
		<-ctx.Done()
		return core.ToolOutput{}, ctx.Err()
	}}
	ctx := core.ScopeClientToolRunner(core.ContextWithClientToolRunner(context.Background(), runner), []core.ToolSpec{spec})
	ctx = core.ContextWithSessionID(ctx, core.SessionIDFromContext(base)+"async-client")
	e := newCodeEngine()
	defer e.CloseCodeMode()
	e.RegisterTool(codeTestTool{name: "server", run: func(context.Context, json.RawMessage) (core.ToolOutput, error) {
		return core.TextOutput("survived"), nil
	}})
	first, err := e.ExecCodeMode(ctx, `try { await tools.phone({}) } catch(e) { text('device ended') } text((await tools.server({})).text)`, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatalf("client did not dispatch: %+v", first)
	}
	yielded, err := e.WaitCodeMode(ctx, first.CellID, 0, 0)
	if err != nil || isCodeModeTerminal(yielded.Status) || lease.Check() != nil {
		t.Fatalf("observation timeout revoked client: %+v %v", yielded, err)
	}
	lease.Revoke("originating response completed while cell survives")
	final := awaitCodeModeTerminal(t, e, ctx, yielded)
	if final.Status != "completed" || !strings.Contains(partsText(final), "survived") {
		t.Fatalf("server did not survive foreground expiry: %+v", final)
	}
	var unknown bool
	for _, call := range final.Calls {
		if call.Name == "phone" && call.Outcome == "outcome_unknown" {
			unknown = true
		}
	}
	if !unknown {
		t.Fatalf("dispatched client failure lost unknown outcome: %+v", final.Calls)
	}
}
