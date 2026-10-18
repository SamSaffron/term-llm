package runtime

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	runpkg "github.com/samsaffron/term-llm/internal/run"
)

func TestSelectRunModelResumeKeepsAdmittedIdentity(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{"debug": {Model: "new-default"}}}
	selection, _, err := SelectRunModel(cfg, ModelSelectionInput{
		Agent:       &agents.Agent{Name: "child", Provider: "debug", Model: "changed-default"},
		Inherited:   modelpolicy.Policy{}.With("boss", []string{"debug:previous"}),
		Parent:      runpkg.ParentModel{Provider: "debug", Model: "parent-live"},
		ResumeModel: runpkg.ParentModel{Provider: "debug", Model: "previous"},
	})
	if err != nil || selection != (ModelSelection{Provider: "debug", Model: "previous"}) {
		t.Fatalf("resume selection = %+v, error = %v", selection, err)
	}
}
