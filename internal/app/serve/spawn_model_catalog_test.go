package serve

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tools/spawnmodel"
)

var _ spawnmodel.Catalog = (*SpawnAgentRunner)(nil)

func TestSpawnAgentRejectsGuessedProviderBeforeStartingChild(t *testing.T) {
	cfg, _ := modelAllowlistFixture(t, "debug:*")
	// Config discovery keeps local login probes out of the expected list.
	cfg.ProviderDiscovery = config.ProviderDiscoveryConfig
	runner, err := NewSpawnAgentRunner(cfg, false, tools.NewApprovalManager(nil))
	if err != nil {
		t.Fatal(err)
	}
	tool := tools.NewSpawnAgentTool(tools.SpawnConfig{MaxDepth: 2, DefaultTimeout: 5}, 0)
	tool.SetRunner(runner)

	properties := tool.Spec().Schema["properties"].(map[string]any)
	description := properties["model"].(map[string]any)["description"].(string)
	if strings.Contains(description, "must be one of") || strings.Contains(description, "other") {
		t.Fatalf("model description = %q, want no provider enumeration", description)
	}

	for _, model := range []string{"codex:gpt-5.4", "expensive:default"} {
		args, _ := json.Marshal(tools.SpawnAgentArgs{AgentName: "reviewer", Prompt: "inspect", Model: model})
		output, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatal(err)
		}
		result, err := tools.ParseSpawnAgentResult(output.Content)
		if err != nil {
			t.Fatal(err)
		}
		// INVALID_PARAMS is returned before the child run is registered.
		if !output.IsError || result.Type != string(tools.ErrInvalidParams) ||
			!strings.Contains(result.Error, "unknown provider") || !strings.Contains(result.Error, "use one of debug, other") ||
			!strings.Contains(result.Error, "<model>") || !strings.Contains(result.Error, "omit model") {
			t.Fatalf("spawn with %q = %q, want unknown provider error with recovery guidance", model, output.Content)
		}
	}
}
