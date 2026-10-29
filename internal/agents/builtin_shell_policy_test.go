package agents

import (
	"slices"
	"testing"
)

func TestBuiltinDeveloperShellPolicy(t *testing.T) {
	agent, err := getBuiltinAgent("developer")
	if err != nil {
		t.Fatalf("getBuiltinAgent(developer): %v", err)
	}

	// Keep the bundled grant limited to directory and Git inspection. An
	// allowlist of the remaining grants also catches new execution shortcuts,
	// not just reintroduction of the removed build/test/package-manager grants.
	wantAllow := []string{"pwd", "git diff", "git status", "git log"}
	if len(agent.Shell.Allow) != len(wantAllow) {
		t.Fatalf("bundled shell grants = %v, want %v", agent.Shell.Allow, wantAllow)
	}
	for _, pattern := range agent.Shell.Allow {
		if !slices.Contains(wantAllow, pattern) {
			t.Errorf("unexpected bundled shell grant %q", pattern)
		}
	}
	for _, pattern := range wantAllow {
		if !slices.Contains(agent.Shell.Allow, pattern) {
			t.Errorf("missing bundled inspection grant %q", pattern)
		}
	}
	if agent.Shell.AutoRun {
		t.Error("bundled developer must not enable shell.auto_run")
	}
	if len(agent.Shell.Scripts) != 0 {
		t.Errorf("bundled developer must not grant named scripts: %v", agent.Shell.Scripts)
	}
}

func TestBuiltinDeveloperShellPolicyDoesNotGrantCodeExecution(t *testing.T) {
	agent, err := getBuiltinAgent("developer")
	if err != nil {
		t.Fatalf("getBuiltinAgent(developer): %v", err)
	}

	// Formatters are included: go fmt can use an alternate gofmt executable,
	// and cargo fmt invokes an external formatter/plugin.
	for _, pattern := range []string{
		"go build*", "go test*", "go run*", "go fmt*",
		"npm *", "yarn *", "pnpm *",
		"cargo build*", "cargo test*", "cargo fmt*",
		"python *", "pytest*", "make *",
	} {
		t.Run(pattern, func(t *testing.T) {
			if slices.Contains(agent.Shell.Allow, pattern) {
				t.Errorf("bundled developer must not pre-approve code execution via %q", pattern)
			}
		})
	}
}
