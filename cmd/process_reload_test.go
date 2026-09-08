package cmd

import (
	"os"
	"os/exec"
	"testing"
)

func TestServeReloadTokenIsPrivateToSuccessor(t *testing.T) {
	const probe = "TERM_LLM_TEST_RELOAD_TOKEN_PROBE"
	const secret = "synthetic-reload-token"
	switch os.Getenv(probe) {
	case "successor":
		if serveReloadToken != secret {
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
		if serveReloadToken != "" || os.Getenv("TERM_LLM_SERVE_TOKEN") != "" || os.Getenv("TERM_LLM_SERVE_RELOAD_TOKEN") != "" {
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
	child.Env = processReloadEnviron("TERM_LLM_SERVE_RELOAD_TOKEN=" + secret)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("private token handoff failed: %v\n%s", err, output)
	}
}

func TestServeReloadTokenPreservesExplicitAuthSettings(t *testing.T) {
	old := serveReloadToken
	serveReloadToken = "saved-token"
	defer func() { serveReloadToken = old }()
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
			token, _, err := resolveServeToken(tc.flag, tc.env, tc.auth, restoreOrGenerateServeToken)
			if err != nil || token != tc.want {
				t.Fatal("wrong auth precedence", err)
			}
		})
	}
	serveReloadToken = ""
	if token, err := restoreOrGenerateServeToken(); err != nil || token == "" {
		t.Fatal("fresh startup failed to generate a token", err)
	}
}
