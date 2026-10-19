package reload

import (
	"os"
	"os/exec"
	"reflect"
	"testing"
)

func TestReloadTokensPrivateToSuccessor(t *testing.T) {
	const probe = "TERM_LLM_TEST_LEAF_RELOAD_TOKEN_PROBE"
	const secret = "synthetic-reload-token"
	switch os.Getenv(probe) {
	case "successor":
		if ServeToken() != secret || MCPToken() != secret {
			t.Fatal("successor lost captured tokens")
		}
		for _, key := range []string{"TERM_LLM_SERVE_RELOAD_TOKEN", "TERM_LLM_MCP_RELOAD_TOKEN"} {
			if _, ok := os.LookupEnv(key); ok {
				t.Fatalf("%s exposed to tool children", key)
			}
			t.Setenv(key, "changed-after-startup")
		}
		if ServeToken() != secret || MCPToken() != secret {
			t.Fatal("tokens were not captured once")
		}
		for _, key := range []string{"TERM_LLM_SERVE_RELOAD_TOKEN", "TERM_LLM_MCP_RELOAD_TOKEN"} {
			_ = os.Unsetenv(key)
		}
		t.Setenv(probe, "tool")
		tool := exec.Command(os.Args[0], "-test.run=^TestReloadTokensPrivateToSuccessor$")
		if output, err := tool.CombinedOutput(); err != nil {
			t.Fatalf("tool inherited tokens: %v\n%s", err, output)
		}
		return
	case "tool":
		if ServeToken() != "" || MCPToken() != "" {
			t.Fatal("tool received successor credentials")
		}
		return
	}
	for _, key := range []string{"TERM_LLM_SERVE_RELOAD_TOKEN", "TERM_LLM_MCP_RELOAD_TOKEN"} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	t.Setenv(probe, "successor")
	child := exec.Command(os.Args[0], "-test.run=^TestReloadTokensPrivateToSuccessor$")
	child.Env = Environ("TERM_LLM_SERVE_RELOAD_TOKEN="+secret, "TERM_LLM_MCP_RELOAD_TOKEN="+secret)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("private handoff: %v\n%s", err, output)
	}
}

func TestEnvironProviderOrder(t *testing.T) {
	old := environProviders
	environProviders = nil
	defer func() { environProviders = old }()
	RegisterEnviron(func() []string { return []string{"C1_ORDER=first"} })
	RegisterEnviron(func() []string { return []string{"C1_ORDER=second"} })
	env := Environ("C1_ORDER=extra")
	want := []string{"C1_ORDER=first", "C1_ORDER=second", "C1_ORDER=extra"}
	if got := env[len(env)-len(want):]; !reflect.DeepEqual(got, want) {
		t.Fatalf("provider order %q, want %q", got, want)
	}
}
