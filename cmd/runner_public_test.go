package cmd_test

import (
	"context"
	"strings"
	"testing"

	command "github.com/samsaffron/term-llm/cmd"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	runpkg "github.com/samsaffron/term-llm/internal/run"
)

// Exercise the opaque preparation API from outside its implementation package.
func TestRunnerPublicPreparationAndEventBoundary(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{"debug": {Model: "prepared-model"}}}
	runner := command.NewRunner(cfg, command.RunnerOptions{})
	req := runpkg.Request{Platform: runpkg.PlatformConsole, Cwd: t.TempDir(), DeferSession: true, MaxTurns: 1, ProviderInstance: mockprovider.NewMockProvider("mock").AddTextResponse("prepared output"), ModelPolicy: modelpolicy.Policy{}.With("owner", []string{"debug:prepared-model"})}
	if err := runner.CheckAgentModel(ctx, req); err != nil {
		t.Fatal(err)
	}
	denied := req
	denied.Model = "denied"
	if err := runner.CheckAgentModel(ctx, denied); err == nil {
		t.Fatal("admission accepted a denied model")
	}
	prepared, err := runner.Prepare(ctx, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	providerReq := prepared.Request()
	if providerReq.Model != "prepared-model" || prepared.Engine() == nil {
		t.Fatalf("prepared request/engine lost: %#v / %p", providerReq, prepared.Engine())
	}
	prepared.ConfigureInteractiveSink(nil)
	var text strings.Builder
	result, err := prepared.RunWithEvents(ctx, false, false, []core.Message{core.UserText("hello")}, providerReq, func(ev core.Event) error {
		if ev.Type == core.EventTextDelta {
			text.WriteString(ev.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text.String() != "prepared output" || text.String() != "prepared output" {
		t.Fatalf("result/event text = %q / %q", result.Text.String(), text.String())
	}
}
