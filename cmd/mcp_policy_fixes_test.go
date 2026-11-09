package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/testutil/mcptest"
	"github.com/spf13/cobra"
)

func TestMCPPolicyConfigSetSequence(t *testing.T) {
	for _, value := range []string{"migration-data", " migration-data , reviewer ", "42,true"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			if err := configSet(nil, []string{"mcp.servers.source-db.agents", value}); err != nil {
				t.Fatal(err)
			}
			policy, err := config.LoadMCPPolicy()
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			for _, name := range strings.Split(value, ",") {
				want = append(want, strings.TrimSpace(name))
			}
			if !reflect.DeepEqual(policy.Servers["source-db"].Agents, want) {
				t.Fatalf("policy=%#v want %v", policy, want)
			}
			// Updating an existing sequence must also retain sequence semantics.
			if err := configSet(nil, []string{"mcp.servers.source-db.agents", "other"}); err != nil {
				t.Fatal(err)
			}
			policy, err = config.LoadMCPPolicy()
			if err != nil || !reflect.DeepEqual(policy.Servers["source-db"].Agents, []string{"other"}) {
				t.Fatalf("updated policy=%#v, %v", policy, err)
			}
		})
	}
}

func TestMCPPolicyConfigSetRejectsDangerousValues(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := configSet(nil, []string{"mcp.servers.db.agents", "trusted"}); err != nil {
		t.Fatal(err)
	}
	path, _ := config.GetConfigPath()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", " ", ",", "trusted,", "trusted, ,other"} {
		err := configSet(nil, []string{"mcp.servers.db.agents", value})
		if err == nil || !strings.Contains(err.Error(), "edit config.yaml") {
			t.Errorf("value %q error=%v", value, err)
		}
	}
	for _, key := range []string{"mcp", "mcp.servers", "mcp.servers.db", "mcp.servers.db.agent", "mcp.servers.Prod.DB.agents"} {
		if err := configSet(nil, []string{key, "trusted"}); err == nil {
			t.Errorf("accepted invalid set key: %s", key)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("rejected set changed policy: %s", after)
	}
}

func TestMCPPolicyStatusIncludesRestrictedServers(t *testing.T) {
	mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"restricted": {Command: "must-not-start"}, "open": {Command: "must-not-start"}})
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm", "config.yaml")
	if err := os.WriteFile(path, []byte("mcp: {servers: {restricted: {agents: []}}}"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := mcpStatus(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "restricted\n") || !strings.Contains(out.String(), "open\n") {
		t.Fatalf("status hid configured servers: %s", out.String())
	}
}

func TestMCPPolicyAgentListingHasContiguousSourceHeadings(t *testing.T) {
	searchRoots := []string{t.TempDir(), t.TempDir()}
	writeServeContractConfig(t, "default_provider: debug\nagents:\n  use_builtin: true\n  search_paths:\n    - "+searchRoots[0]+"\n    - "+searchRoots[1]+"\n")
	userRoot := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm", "agents")
	for _, root := range append([]string{userRoot}, searchRoots...) {
		for _, name := range []string{"aaa-" + filepath.Base(root), "zzz-" + filepath.Base(root)} {
			dir := filepath.Join(root, name)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("description: fixture\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Directory basenames may repeat for temporary roots; ensure unique lookup
	// names across search roots so both directories actually have entries.
	for i, root := range searchRoots {
		dir := filepath.Join(root, []string{"search-first", "search-second"}[i])
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("description: fixture\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	oldBuiltin, oldUser, oldLocal := agentsBuiltin, agentsUser, agentsLocal
	agentsBuiltin, agentsUser, agentsLocal = false, false, false
	t.Cleanup(func() { agentsBuiltin, agentsUser, agentsLocal = oldBuiltin, oldUser, oldLocal })
	var err error
	out := captureStdout(t, func() { err = runAgentsList(agentsCmd, nil) })
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"[builtin]", "[user] " + userRoot + "/", "[search-path] " + searchRoots[0] + "/", "[search-path] " + searchRoots[1] + "/"} {
		if strings.Count(out, header) != 1 {
			t.Errorf("expected one %q header: %s", header, out)
		}
	}
	if strings.Index(out, "[builtin]") > strings.Index(out, "[user]") || strings.Index(out, "[user]") > strings.Index(out, "[search-path]") {
		t.Fatalf("wrong source rank: %s", out)
	}
}

func TestMCPPolicyLoopUsesAgentIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"restricted": {Command: os.Args[0], Env: map[string]string{mcptest.ChildEnv: "1"}}})
	root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("default_provider: debug\nproviders: {debug: {model: fast}}\nsessions: {enabled: false}\nskills: {enabled: false}\nmcp: {servers: {restricted: {agents: [foo]}}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "agents", "foo")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("name: self-asserted\n"), 0600); err != nil {
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
	for _, selected := range []string{"foo", dir, ""} {
		loopAgent, loopMCP, loopDone, loopDoneFile, loopTools = selected, "restricted", "", marker+":COMPLETE", "read_file"
		loopApproval, loopYolo, loopAuto = "", false, false
		cmd := &cobra.Command{}
		cmd.Flags().String("mcp", "", "")
		if err := cmd.Flags().Set("mcp", "restricted"); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		err := runLoop(cmd, []string{"must not invoke a model"})
		if selected == "foo" {
			if err != nil || !strings.Contains(out.String(), "MCP ready") {
				t.Fatalf("trusted loop error=%v output=%s", err, out.String())
			}
		} else {
			if err == nil || !strings.Contains(err.Error(), "requires an authorized trusted agent") {
				t.Fatalf("untrusted loop agent %q error=%v", selected, err)
			}
		}
	}
}
