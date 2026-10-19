package cmd

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"
)

func TestServeHubWithUsesCapturedValidationOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts apphub.Options
		want string
	}{
		{"auth before bind", apphub.Options{AuthMode: "invalid-mode", Port: 0}, "invalid --auth"},
		{"port before public unauthenticated bind", apphub.Options{AuthMode: "none", Host: "0.0.0.0", Port: 0}, "invalid --port 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runServeHubWith(context.Background(), hubDeps{Stdout: io.Discard, Stderr: io.Discard}, &tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("startup error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestHubBootstrapSecretUsesCapturedWriters(t *testing.T) {
	var out, stderr bytes.Buffer
	old := apphub.OutputIsTerminal
	t.Cleanup(func() { apphub.OutputIsTerminal = old })
	checkedOutput := false
	apphub.OutputIsTerminal = func(w any) bool { checkedOutput = w == &out; return false }
	t.Setenv("TERM_LLM_HUB_BOOTSTRAP_TOKEN", "")
	secret, display, err := resolveHubBootstrapSecret(hubDeps{Stdout: &out, Stderr: &stderr}, &apphub.Options{PrintBootstrapToken: true}, true)
	if err != nil || len(secret) == 0 || display == "" {
		t.Fatalf("bootstrap result unavailable: %v", err)
	}
	if !checkedOutput || out.Len() != 0 || !strings.Contains(stderr.String(), "WARNING: printing a passkey bootstrap code") {
		t.Fatalf("wrong writer routing: output=%q stderr=%q checkedOutput=%v", out.String(), stderr.String(), checkedOutput)
	}
}
