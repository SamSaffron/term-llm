package cmd

import (
	"io"

	"os"
	"path/filepath"

	"testing"

	apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"
)

func TestResolveHubBootstrapSecretNonInteractiveAndEnvScrub(t *testing.T) {
	oldTerminal := apphub.OutputIsTerminal
	opts := &apphub.Options{}
	deps := hubDeps{Stdout: io.Discard, Stderr: io.Discard}
	t.Cleanup(func() {
		apphub.OutputIsTerminal = oldTerminal
	})
	apphub.OutputIsTerminal = func(any) bool { return false }
	opts.BootstrapTokenFile = ""
	opts.PrintBootstrapToken = false
	t.Setenv("TERM_LLM_HUB_BOOTSTRAP_TOKEN", "")
	if _, _, err := resolveHubBootstrapSecret(deps, opts, true); err == nil {
		t.Fatal("expected noninteractive error")
	}
	t.Setenv("TERM_LLM_HUB_BOOTSTRAP_TOKEN", "abcdefghijklmnopqrst")
	secret, display, err := resolveHubBootstrapSecret(deps, opts, true)
	if err != nil || string(secret) != "abcdefghijklmnopqrst" || display != "" {
		t.Fatalf("secret=%q display=%q err=%v", secret, display, err)
	}
	if value, ok := os.LookupEnv("TERM_LLM_HUB_BOOTSTRAP_TOKEN"); ok {
		t.Fatalf("environment not scrubbed: %q", value)
	}
	secretFile := filepath.Join(t.TempDir(), "bootstrap")
	if err := os.WriteFile(secretFile, []byte("file-secret-abcdefghijklmnop"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts.BootstrapTokenFile = secretFile
	t.Setenv("TERM_LLM_HUB_BOOTSTRAP_TOKEN", "environment-secret-abcdef")
	secret, display, err = resolveHubBootstrapSecret(deps, opts, true)
	if err != nil || string(secret) != "file-secret-abcdefghijklmnop" || display != "" {
		t.Fatalf("file precedence secret=%q display=%q err=%v", secret, display, err)
	}
	if _, ok := os.LookupEnv("TERM_LLM_HUB_BOOTSTRAP_TOKEN"); ok {
		t.Fatal("environment not scrubbed when file won")
	}
	opts.BootstrapTokenFile = ""
	t.Setenv("TERM_LLM_HUB_BOOTSTRAP_TOKEN", "")
	opts.PrintBootstrapToken = true
	secret, display, err = resolveHubBootstrapSecret(deps, opts, true)
	if err != nil || len(secret) == 0 || display == "" {
		t.Fatalf("print override secret=%q display=%q err=%v", secret, display, err)
	}
}
