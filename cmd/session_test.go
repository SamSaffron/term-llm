package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
)

func TestResolveSettings_AppendsInsightsToSystemPrompt(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "override", "memory.db")
	defaultPath := filepath.Join(dir, "default", "memory.db")
	t.Setenv("TERM_LLM_MEMORY_DB", filepath.Join(dir, "env", "memory.db"))

	store, err := memorydb.NewStore(memorydb.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()

	if err := store.CreateInsight(context.Background(), &memorydb.Insight{
		Agent:          "jarvis",
		Content:        "Avoid filler.",
		CompactContent: "Avoid filler.",
		Category:       "communication-style",
		Confidence:     0.9,
	}); err != nil {
		t.Fatalf("CreateInsight() error = %v", err)
	}

	agent := &agents.Agent{
		Name:         "jarvis",
		SystemPrompt: "Base prompt",
		Memory: agents.MemoryConfig{
			InsightsExpansion: true,
			InsightsMaxTokens: 500,
		},
	}

	settings, err := appserve.ResolveSettings(&config.Config{}, agent, appserve.CLIFlags{MemoryPaths: appmemory.Paths{DBPath: dbPath, DefaultDBPath: defaultPath}}, "", "", "", 0, 20)
	if err != nil {
		t.Fatalf("ResolveSettings() error = %v", err)
	}
	if !strings.Contains(settings.SystemPrompt, "Base prompt") {
		t.Fatalf("SystemPrompt missing base prompt: %q", settings.SystemPrompt)
	}
	if !strings.Contains(settings.SystemPrompt, "<insights>") {
		t.Fatalf("SystemPrompt missing insights block: %q", settings.SystemPrompt)
	}
	if !strings.Contains(settings.SystemPrompt, "Avoid filler.") {
		t.Fatalf("SystemPrompt missing insight content: %q", settings.SystemPrompt)
	}
	for _, untouched := range []string{defaultPath, filepath.Join(dir, "env", "memory.db")} {
		if _, err := os.Stat(untouched); !os.IsNotExist(err) {
			t.Fatalf("non-selected memory DB %q was touched: %v", untouched, err)
		}
	}
}
