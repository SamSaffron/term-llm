package cmd_test

import (
	"context"
	"errors"
	"testing"

	app "github.com/samsaffron/term-llm/cmd"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/testutil"
)

func TestSessionInputPublicCoordinatorPreservesInternedSelection(t *testing.T) {
	coordinator := app.NewSessionInputCoordinator()
	store := testutil.NewSessionInputStore(t, ":memory:")
	binding := app.InputBinding(" agent ", "directory")
	if binding.Agent != "agent" || binding.Dir != "directory" {
		t.Fatalf("binding = %#v", binding)
	}
	ticket, err := coordinator.Acquire(context.Background(), store, "first", binding)
	if err != nil {
		t.Fatal(err)
	}
	defer ticket.Fail()
	if !ticket.Owner() {
		t.Fatal("first ticket did not own selection")
	}
	pair := app.SessionInputSelection{BasePrompt: "base", Prompt: "expanded", Tools: "shell"}
	selected := ticket.Finish(pair)
	if selected == nil || *selected != pair || ticket.Owner() || ticket.Selected() != selected || coordinator.Ready(store, "first") != selected {
		t.Fatal("Finish did not publish the returned immutable selection")
	}
	other, err := coordinator.Acquire(context.Background(), store, "second", binding)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Fail()
	if other.Finish(pair) != selected {
		t.Fatal("equal selections were not interned across tickets")
	}
	fresh, err := coordinator.AcquireFresh(context.Background(), store, "first", binding)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Fail()
	if coordinator.Ready(store, "first") != selected {
		t.Fatal("failed fresh preparation did not restore the exact former pointer")
	}
	coordinator.Invalidate(store, "first")
	if coordinator.Ready(store, "first") != nil {
		t.Fatal("invalidation retained the selected input")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := coordinator.Acquire(ctx, store, "cancelled", binding); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquisition = %v", err)
	}
	if !app.EqualSessionTools("shell,read_file,shell", "read_file,shell") {
		t.Fatal("equivalent tool sets compare differently")
	}
}

func TestSessionInputProcessSwapRestoresExactPointer(t *testing.T) {
	original := app.ProcessSessionInputs()
	first := app.NewSessionInputCoordinator()
	restoreFirst := app.SwapProcessSessionInputs(first)
	t.Cleanup(restoreFirst)
	if app.ProcessSessionInputs() != first || app.ProcessSessionInputs() != app.ProcessSessionInputs() {
		t.Fatal("accessor does not use the swapped singleton")
	}
	second := app.NewSessionInputCoordinator()
	restoreSecond := app.SwapProcessSessionInputs(second)
	t.Cleanup(restoreSecond)
	if app.ProcessSessionInputs() != second {
		t.Fatal("nested swap did not install its exact coordinator")
	}
	restoreSecond()
	if app.ProcessSessionInputs() != first {
		t.Fatal("nested restore did not restore the exact former coordinator")
	}
	restoreFirst()
	if app.ProcessSessionInputs() != original {
		t.Fatal("restore did not restore the original process coordinator")
	}
	// Cleanup runs LIFO: restoreSecond before restoreFirst, leaving original.
}

func TestSessionInputPublicCLIFlagsUseSelectedInputs(t *testing.T) {
	selection := &app.SessionInputSelection{BasePrompt: "{{file:must-not-reexpand.md}}", Prompt: "expanded", Tools: "shell"}
	settings, err := app.ResolveSettingsInDir(&config.Config{}, nil, app.CLIFlags{
		Inputs: selection, ToolsSet: true, Tools: "read_file", SystemMessageSet: true, SystemMessage: "replacement",
	}, "", "", "", 0, 1, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if settings.SystemPrompt != selection.BasePrompt || settings.Tools != selection.Tools {
		t.Fatalf("cached inputs were replaced: prompt=%q tools=%q", settings.SystemPrompt, settings.Tools)
	}
}
