package engine

import (
	"context"
	"encoding/json"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/term-llm/term-llm/internal/llm/core"
	corestream "github.com/term-llm/term-llm/internal/llm/core/stream"
)

// Embedding just Tool intentionally hides optional exposure declarations.
type undeclaredCodeTestTool struct{ core.Tool }

type finishingCodeTestTool struct{ codeTestTool }

func (finishingCodeTestTool) IsFinishingTool() bool { return true }

type lifecycleCodeTestTool struct{ codeTestTool }

func (lifecycleCodeTestTool) IsCodeModeLifecycleTool() bool { return true }

func TestCodeModeExposureDefaultDenyAndDirectUsability(t *testing.T) {
	e := newCodeEngine()
	var calls atomic.Int32
	leaf := codeTestTool{name: "undeclared", run: func(context.Context, json.RawMessage) (core.ToolOutput, error) {
		calls.Add(1)
		return core.TextOutput("direct result"), nil
	}}
	e.RegisterTool(undeclaredCodeTestTool{leaf})
	declaredDirect := leaf
	declaredDirect.name = "declared_direct"
	e.RegisterTool(policyCodeTestTool{codeTestTool: declaredDirect, exposure: core.CodeModeExposure{Reason: "host_scoped"}})
	e.RegisterTool(policyCodeTestTool{codeTestTool: codeTestTool{name: "ui_activate_extensions"}, exposure: core.CodeModeExposure{Reason: "extension_lifecycle"}})
	e.RegisterTool(finishingCodeTestTool{codeTestTool{name: "custom_output"}})
	e.RegisterTool(lifecycleCodeTestTool{codeTestTool{name: "renamed_lifecycle"}})
	c := admitTestCell(t, e)
	for _, name := range []string{"undeclared", "declared_direct", "ui_activate_extensions", "custom_output", "renamed_lifecycle"} {
		if slices.Contains(c.Names(), name) {
			t.Errorf("catalogue exposes %s", name)
		}
		if _, err := c.Describe(context.Background(), []string{name}); err == nil {
			t.Errorf("described direct-only %s", name)
		}
		if _, err := c.Call(context.Background(), name, json.RawMessage(`{}`)); err == nil {
			t.Errorf("called direct-only %s", name)
		}
		if err := e.AdmitCodeModule("denied", nil, []string{name}, func() bool { return true }); err == nil {
			t.Errorf("admitted direct-only dependency %s", name)
		}
	}
	for _, name := range []string{leaf.name, declaredDirect.name} {
		outcome := e.executeSingleToolCallOutcome(context.Background(), core.ToolCall{ID: "direct", Name: name, Arguments: json.RawMessage(`{}`)}, corestream.Sender{}, false, false)
		if outcome.err != nil || outcome.output.Content != "direct result" {
			t.Fatalf("direct call for %s changed: %+v", name, outcome)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("expected only two direct executions, got %d", calls.Load())
	}
}

func TestCodeModeExposureReplacementNarrowsAndWideningNeedsNewCell(t *testing.T) {
	e := newCodeEngine()
	leaf := codeTestTool{name: "leaf", run: func(context.Context, json.RawMessage) (core.ToolOutput, error) {
		return core.TextOutput("ok"), nil
	}}
	e.RegisterTool(leaf)
	e.RegisterTool(policyCodeTestTool{codeTestTool: codeTestTool{name: "initially_direct"}, exposure: core.CodeModeExposure{Reason: "host_scoped"}})
	if err := e.AdmitCodeModule("dependent", nil, []string{"leaf"}, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	c := admitTestCell(t, e)
	e.RegisterTool(policyCodeTestTool{codeTestTool: leaf, exposure: core.CodeModeExposure{Reason: "binding_unavailable"}})
	if _, err := c.Describe(context.Background(), []string{"leaf"}); err == nil {
		t.Fatal("same-schema direct-only replacement described")
	}
	if _, err := c.Call(context.Background(), "leaf", json.RawMessage(`{}`)); err == nil {
		t.Fatal("same-schema direct-only replacement executed")
	}
	if _, err := c.module("skill:dependent"); err == nil {
		t.Fatal("module retained a direct-only dependency")
	}
	wider := leaf
	wider.name = "initially_direct"
	e.RegisterTool(wider)
	if _, err := c.Call(context.Background(), wider.name, json.RawMessage(`{}`)); err == nil {
		t.Fatal("exposure widening expanded an admitted cell")
	}
	c.Close()
	next := admitTestCell(t, e)
	if !slices.Contains(next.Names(), wider.name) {
		t.Fatal("new cell did not see explicitly allowed replacement")
	}
}

func TestCodeModeDirectOnlyCompatibilityIsNarrowingAndLive(t *testing.T) {
	e := newCodeEngine()
	leaf := codeTestTool{name: "leaf"}
	e.RegisterTool(leaf)
	e.RegisterTool(undeclaredCodeTestTool{codeTestTool{name: "unknown"}})
	e.RegisterTool(finishingCodeTestTool{codeTestTool{name: "output"}})
	c := admitTestCell(t, e)
	e.SetCodeModeDirectOnly([]string{"leaf"})
	if _, err := c.Describe(context.Background(), []string{leaf.name}); err == nil {
		t.Fatal("compatibility exclusion did not narrow a live cell")
	}
	if err := e.AdmitCodeModule("blocked", nil, []string{leaf.name}, func() bool { return true }); err == nil {
		t.Fatal("compatibility exclusion bypassed by module dependency")
	}
	c.Close()
	excluded := admitTestCell(t, e)
	e.SetCodeModeDirectOnly(nil)
	if _, err := excluded.Describe(context.Background(), []string{leaf.name}); err == nil {
		t.Fatal("clearing exclusions widened an existing ceiling")
	}
	excluded.Close()
	next := admitTestCell(t, e)
	if got := next.Names(); !slices.Equal(got, []string{"leaf"}) {
		t.Fatalf("clearing exclusions granted undeclared/finishing exposure: %v", got)
	}
}

func TestCodeModeExposureRecheckedAfterPermitWait(t *testing.T) {
	e := newCodeEngine()
	var calls atomic.Int32
	e.RegisterTool(codeTestTool{name: "leaf", run: func(context.Context, json.RawMessage) (core.ToolOutput, error) {
		calls.Add(1)
		return core.TextOutput("unexpected"), nil
	}})
	c := admitTestCell(t, e)
	for range cap(c.permits) {
		c.permits <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Call(ctx, "leaf", json.RawMessage(`{}`))
		done <- err
	}()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		c.mu.Lock()
		waiting := c.waiting > 0
		c.mu.Unlock()
		if waiting {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("call did not reach permit wait")
		default:
			runtime.Gosched()
		}
	}
	e.SetCodeModeDirectOnly([]string{"leaf"})
	for range cap(c.permits) {
		<-c.permits
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not_dispatched") || calls.Load() != 0 {
			t.Fatalf("revoked waiting call dispatched: %v, calls=%d", err, calls.Load())
		}
	case <-deadline.C:
		t.Fatal("revoked waiting call did not settle")
	}
}

func TestCodeModeExposureDeferredToolsStayPermissionBound(t *testing.T) {
	e := newCodeEngine()
	if err := e.tools.RegisterDeferred(codeTestTool{name: "authorized_deferred"}); err != nil {
		t.Fatal(err)
	}
	if err := e.tools.RegisterDeferred(policyCodeTestTool{codeTestTool: codeTestTool{name: "tool_search"}, exposure: core.CodeModeExposure{Reason: "run_scoped_discovery"}}); err != nil {
		t.Fatal(err)
	}
	if got := e.tools.AllSpecs(); len(got) != 0 {
		t.Fatalf("deferred schemas unexpectedly visible: %+v", got)
	}
	c := admitTestCell(t, e)
	if got := c.Names(); !slices.Equal(got, []string{"authorized_deferred"}) {
		t.Fatalf("deferred permission/exposure catalogue = %v", got)
	}
	if _, err := c.Describe(context.Background(), []string{"authorized_deferred"}); err != nil {
		t.Fatal(err)
	}
	e.SetAllowedToolsFilter([]string{})
	if _, err := c.Describe(context.Background(), []string{"authorized_deferred"}); err == nil {
		t.Fatal("deferred discovery was treated as a permission grant")
	}
}

func TestCodeModeExposureDoesNotInferSemanticsFromNames(t *testing.T) {
	e := newCodeEngine()
	// Remote tools can have these names; their instance declaration, not a
	// duplicated builtin blacklist, determines exposure. Registry provenance
	// separately prevents them replacing a locally registered control.
	for _, name := range []string{"spawn_agent", "spawn_script", "ask_user", "ui_activate_extensions"} {
		e.RegisterTool(codeTestTool{name: name})
	}
	c := admitTestCell(t, e)
	if got := c.Names(); !slices.Equal(got, []string{"ask_user", "spawn_agent", "spawn_script", "ui_activate_extensions"}) {
		t.Fatalf("name-based classification returned %v", got)
	}
}
