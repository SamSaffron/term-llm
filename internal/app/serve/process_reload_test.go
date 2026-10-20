package serve

import (
	"context"
	"github.com/samsaffron/term-llm/internal/app/reload"
	"github.com/samsaffron/term-llm/internal/process"
	"github.com/samsaffron/term-llm/internal/tools"
	"os"
	"os/exec"
	"reflect"
	"testing"
)

func TestServeReloadTokenIsPrivateToSuccessor(t *testing.T) {
	const probe = "TERM_LLM_TEST_RELOAD_TOKEN_PROBE"
	const secret = "synthetic-reload-token"
	switch os.Getenv(probe) {
	case "successor":
		if reload.ServeToken() != secret {
			t.Fatal("successor did not capture its private bearer token")
		}
		if token, err := restoreOrGenerateServeToken(); err != nil || token != secret {
			t.Fatal("successor changed the bearer token")
		}
		for _, key := range []string{"TERM_LLM_SERVE_TOKEN", "TERM_LLM_SERVE_RELOAD_TOKEN"} {
			if _, present := os.LookupEnv(key); present {
				t.Fatalf("%s is exposed to tool children", key)
			}
		}
		// A subsequent tool child inherits only os.Environ, not the private
		// replacement hints. Verify actual startup, not just a helper function.
		t.Setenv(probe, "tool")
		tool := exec.Command(os.Args[0], "-test.run=^TestServeReloadTokenIsPrivateToSuccessor$")
		if output, err := tool.CombinedOutput(); err != nil {
			t.Fatalf("tool inherited reload credentials: %v\n%s", err, output)
		}
		return
	case "tool":
		if reload.ServeToken() != "" || os.Getenv("TERM_LLM_SERVE_TOKEN") != "" || os.Getenv("TERM_LLM_SERVE_RELOAD_TOKEN") != "" {
			t.Fatal("tool process received the server bearer token")
		}
		return
	}
	for _, key := range []string{"TERM_LLM_SERVE_TOKEN", "TERM_LLM_SERVE_RELOAD_TOKEN"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(probe, "successor")
	child := exec.Command(os.Args[0], "-test.run=^TestServeReloadTokenIsPrivateToSuccessor$")
	child.Env = reload.Environ("TERM_LLM_SERVE_RELOAD_TOKEN=" + secret)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("private token handoff failed: %v\n%s", err, output)
	}
}

func TestServeReloadTokenPreservesExplicitAuthSettings(t *testing.T) {
	generate := func() (string, error) { return restoreOrGenerateServeTokenWith("saved-token") }
	for _, tc := range []struct {
		name, flag, env, want string
		auth                  bool
	}{
		{"reload fallback", "", "", "saved-token", true},
		{"explicit flag", "flag-token", "env-token", "flag-token", true},
		{"public environment", "", "env-token", "env-token", true},
		{"no bearer auth", "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, _, err := resolveServeToken(tc.flag, tc.env, tc.auth, generate)
			if err != nil || token != tc.want {
				t.Fatal("wrong auth precedence", err)
			}
		})
	}
	if token, err := restoreOrGenerateServeTokenWith(""); err != nil || token == "" {
		t.Fatal("fresh startup failed to generate a token", err)
	}
}

// Real host registration participates between delegation and UI state handoff.
func TestReloadHostProviderOrder(t *testing.T) {
	const probe = "TERM_LLM_TEST_RELOAD_HOST_ORDER"
	if os.Getenv(probe) != "" {
		if got := resolveServeHubRegistrationToken(""); got != "registration-secret" {
			t.Fatal("host registration capture", got)
		}
		if os.Getenv(hubRegistrationTokenEnv) != "" {
			t.Fatal("registration token leaked")
		}
		publisher := process.Start("test", reload.Executable())
		defer publisher.Stop()
		cleanup, err := process.SaveState("test", map[string]string{"state": "value"})
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup(context.Background())
		want := append(os.Environ(), tools.HubDelegationEnviron()...)
		want = append(want, hubRegistrationEnviron()...)
		want = append(want, process.HandoffEnviron()...)
		want = append(want, "C1_EXTRA=last")
		if got := reload.Environ("C1_EXTRA=last"); !reflect.DeepEqual(got, want) {
			// The inherited environment may contain unrelated credentials. Keep
			// the full equality assertion without printing its secret values.
			t.Fatalf("handoff environment order differs (got %d entries, want %d)", len(got), len(want))
		}
		return
	}
	child := exec.Command(os.Args[0], "-test.run=^TestReloadHostProviderOrder$")
	child.Env = append(os.Environ(), probe+"=1", hubRegistrationTokenEnv+"=registration-secret", "TERM_LLM_HUB_TOKEN=delegation-secret", "TERM_LLM_HUB_URL=http://hub.test", "TERM_LLM_HUB_NODE_ID=node")
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("host handoff: %v\n%s", err, output)
	}
}
