package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/testutil/mcptest"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func TestMCPPolicyConfigSetCaseVariantsFailWithoutWriting(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := configSet(nil, []string{"mcp.servers.db.agents", "trusted"}); err != nil {
		t.Fatal(err)
	}
	path, _ := config.GetConfigPath()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"MCP", "MCP.servers.db.agents", "mCp.servers.db.agents", "Mcp.servers.Prod.DB.agents", "MCP.servers"} {
		if err := configSet(nil, []string{key, ""}); err == nil {
			t.Errorf("case-variant edit bypassed policy guard: %s", key)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("rejected edits changed file: %s", after)
	}
}

func TestMCPPolicySequenceRejectsMissingPathWithoutMutation(t *testing.T) {
	for _, body := range []string{"{}", "mcp: {}", "mcp: {servers: {db: {}}}", "mcp: {servers: {db: scalar}}"} {
		t.Run(body, func(t *testing.T) {
			var root yaml.Node
			if err := yaml.Unmarshal([]byte(body), &root); err != nil {
				t.Fatal(err)
			}
			before, err := yaml.Marshal(&root)
			if err != nil {
				t.Fatal(err)
			}
			if err := setMCPPolicySequence(&root, "mcp.servers.db.agents", []string{"trusted"}); err == nil {
				t.Fatal("missing segment was overwritten")
			}
			after, err := yaml.Marshal(&root)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("failed sequence edit changed parent: %s", after)
			}
		})
	}
	if err := setMCPPolicySequence(nil, "mcp.servers.db.agents", []string{"trusted"}); err == nil {
		t.Fatal("nil document accepted")
	}
}

func TestMCPPolicyAnnotatedConfigTreatsDottedNamesAsOpaque(t *testing.T) {
	var root yaml.Node
	body := "mcp:\n  servers:\n    Prod.DB: {agents: [trusted]}\n    prod.db: {agents: []}\n    other.dot: {agent: [typo]}\n"
	if err := yaml.Unmarshal([]byte(body), &root); err != nil {
		t.Fatal(err)
	}
	raw, unknown := map[string]bool{}, map[string]bool{}
	extractConfigKeys(&root, "", raw, unknown)
	if len(unknown) != 1 || !unknown["mcp.servers.other.dot.agent"] {
		t.Fatalf("opaque names reported unknown: %v", unknown)
	}
	for _, key := range []string{"mcp.servers.Prod.DB", "mcp.servers.Prod.DB.agents", "mcp.servers.prod.db.agents"} {
		if !raw[key] {
			t.Errorf("raw literal key missing: %s", key)
		}
	}
	if config.IsKnownKey("mcp.servers.Prod.DB.agents") {
		t.Fatal("opaque YAML support accidentally expanded dotted CLI editor syntax")
	}
}

func TestMCPPolicyLoopFiltersImplicitAgentDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"denied": {Command: "must-not-start"}})
	root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("default_provider: debug\nproviders: {debug: {model: fast}}\nsessions: {enabled: false}\nskills: {enabled: false}\nmcp: {servers: {denied: {agents: []}}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "agents", "foo")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("name: foo\nmcp: [{name: denied}, {name: removed}]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "done")
	if err := os.WriteFile(marker, []byte("COMPLETE"), 0600); err != nil {
		t.Fatal(err)
	}
	oldAgent, oldMCP, oldDone, oldDoneFile, oldTools := loopAgent, loopMCP, loopDone, loopDoneFile, loopTools
	oldApproval, oldYolo, oldAuto := loopApproval, loopYolo, loopAuto
	t.Cleanup(func() {
		loopAgent, loopMCP, loopDone, loopDoneFile, loopTools = oldAgent, oldMCP, oldDone, oldDoneFile, oldTools
		loopApproval, loopYolo, loopAuto = oldApproval, oldYolo, oldAuto
	})
	loopAgent, loopMCP, loopDone, loopDoneFile, loopTools = "foo", "", "", marker+":COMPLETE", "read_file"
	loopApproval, loopYolo, loopAuto = "", false, false
	cmd := &cobra.Command{}
	var warnings bytes.Buffer
	cmd.SetOut(&warnings)
	cmd.SetErr(&warnings)
	if err := runLoop(cmd, []string{"must not invoke a model"}); err != nil {
		t.Fatalf("implicit defaults prevented loop: %v", err)
	}
	if !strings.Contains(warnings.String(), "dropping MCP server") || !strings.Contains(warnings.String(), "Done condition met") {
		t.Fatalf("loop did not warn/continue: %s", warnings.String())
	}
}

func TestMCPPolicyConfigSetUpdatesCaseVariantRoot(t *testing.T) {
	for _, rootKey := range []string{"MCP", "mcp", "Mcp"} {
		for _, key := range []string{"MCP.servers.x.agents", "mcp.servers.x.agents"} {
			t.Run(rootKey+"/"+key, func(t *testing.T) {
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				path, err := config.GetConfigPath()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				body := rootKey + ":\n  servers:\n    x: {agents: [old]}\n    Prod.DB: {agents: []}\n"
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				if err := configSet(nil, []string{key, "trusted,reviewer"}); err != nil {
					t.Fatal(err)
				}
				policy, err := config.LoadMCPPolicy()
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(policy.Servers["x"].Agents, []string{"trusted", "reviewer"}) || policy.Allows("Prod.DB", "") {
					t.Fatalf("case edit changed policy: %+v", policy)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var doc yaml.Node
				if err := yaml.Unmarshal(data, &doc); err != nil {
					t.Fatal(err)
				}
				count := 0
				for i := 0; i+1 < len(doc.Content[0].Content); i += 2 {
					if strings.EqualFold(doc.Content[0].Content[i].Value, "mcp") {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("case edit appended a duplicate section: %s", data)
				}
			})
		}
	}
}
