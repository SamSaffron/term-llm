package evalfixture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitializeWorkspaceCreatesRealFixtureBackends(t *testing.T) {
	root := t.TempDir()
	if err := InitializeWorkspace(root); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"records/federation.json",
		"documents/DOC-5.md",
		"databases/evaluation.sqlite",
		"repos/acme-app/.git/HEAD",
		"calls.jsonl",
	} {
		if info, err := os.Stat(filepath.Join(root, path)); err != nil || info.IsDir() {
			t.Fatalf("fixture %s missing or invalid: info=%v err=%v", path, info, err)
		}
	}
}

func TestFederationHasTenServersAndTwoHundredRealisticTools(t *testing.T) {
	domains := Domains()
	if len(domains) != 10 {
		t.Fatalf("domains = %d, want 10", len(domains))
	}
	total := 0
	serverNames := make(map[string]bool)
	for _, domain := range domains {
		if serverNames[domain.Name] {
			t.Fatalf("duplicate server %q", domain.Name)
		}
		serverNames[domain.Name] = true
		if len(domain.Tools) != 20 {
			t.Fatalf("server %s tools = %d, want 20", domain.Name, len(domain.Tools))
		}
		seen := make(map[string]bool)
		for _, tool := range domain.Tools {
			if seen[tool.Name] {
				t.Fatalf("server %s duplicate tool %q", domain.Name, tool.Name)
			}
			seen[tool.Name] = true
			if tool.Description == "" || len(tool.Description) < 80 {
				t.Fatalf("server %s tool %s has unrealistic description %q", domain.Name, tool.Name, tool.Description)
			}
		}
		total += len(domain.Tools)
	}
	if total != 200 {
		t.Fatalf("total tools = %d, want 200", total)
	}
}

func TestAggregateHasExactlyTwoHundredUniqueDomainQualifiedToolsAndOracles(t *testing.T) {
	tools := AggregateTools()
	if len(tools) != 200 {
		t.Fatalf("aggregate tools = %d, want 200", len(tools))
	}
	names := make(map[string]bool, len(tools))
	oracles := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if names[tool.Name] {
			t.Fatalf("duplicate aggregate tool name %q", tool.Name)
		}
		names[tool.Name] = true
		oracle := OracleValue(tool.Domain, tool.Definition.Name)
		if oracle == "" || oracles[oracle] {
			t.Fatalf("non-unique oracle %q for %s", oracle, tool.Name)
		}
		oracles[oracle] = true
	}
}

func TestOracleTaskManifestMatchesFixtureWithoutLeakingPrefixedNames(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "evaluation", "mcp-tool-discovery", "oracle_tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Tasks []struct {
			Prompt         string `json:"prompt"`
			RequiredTool   string `json:"required_tool"`
			ExpectedOracle string `json:"expected_oracle"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Tasks) < 2 {
		t.Fatalf("oracle tasks = %d, want multiple scored tasks", len(manifest.Tasks))
	}
	known := make(map[string]string, 200)
	for _, tool := range AggregateTools() {
		name := "federation__" + tool.Name
		known[name] = OracleValue(tool.Domain, tool.Definition.Name)
	}
	for _, task := range manifest.Tasks {
		want, ok := known[task.RequiredTool]
		if !ok {
			t.Fatalf("unknown required oracle tool %q", task.RequiredTool)
		}
		if task.ExpectedOracle != want {
			t.Fatalf("task %s oracle = %q, want %q", task.RequiredTool, task.ExpectedOracle, want)
		}
		if strings.Contains(task.Prompt, task.RequiredTool) {
			t.Fatalf("ordinary prompt leaks exact prefixed name %q", task.RequiredTool)
		}
	}
}

func TestRequiredCatalogueProfiles(t *testing.T) {
	for _, size := range []int{6, 12, 18, 24, 25, 32, 42, 64, 100, 200} {
		limits, err := ProfileLimits(size)
		if err != nil {
			t.Fatal(err)
		}
		total := 0
		for _, limit := range limits {
			if limit < 0 || limit > 20 {
				t.Fatalf("size %d invalid limit %d", size, limit)
			}
			total += limit
		}
		if total != size {
			t.Fatalf("profile %d totals %d", size, total)
		}
	}
}
