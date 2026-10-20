package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/spf13/cobra"
)

func TestServeFlagBindingIsolated(t *testing.T) {
	// AddCommonFlags records command capabilities in a shared cmd registry;
	// parallel flag binding would race that registry.
	for _, tc := range []struct {
		name     string
		args     []string
		wantHost string
		wantPort int
	}{
		{"default", nil, "127.0.0.1", 8080},
		{"custom", []string{"--host", "localhost", "--port", "9090"}, "localhost", 9090},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opts ServeOptions
			cmd := &cobra.Command{}
			bindServeFlags(cmd, &opts)
			if err := cmd.ParseFlags(tc.args); err != nil {
				t.Fatal(err)
			}
			if opts.Host != tc.wantHost || opts.Port != tc.wantPort || opts.SessionTTL != 30*time.Minute {
				t.Fatalf("flags: host=%q port=%d TTL=%s", opts.Host, opts.Port, opts.SessionTTL)
			}
		})
	}
}

func writeServeContractConfig(t *testing.T, content string) {
	t.Helper()
	home := t.TempDir()
	for key, subdir := range map[string]string{"HOME": "home", "XDG_CONFIG_HOME": "config", "XDG_DATA_HOME": "data", "XDG_CACHE_HOME": "cache"} {
		t.Setenv(key, filepath.Join(home, subdir))
	}
	t.Setenv("TERM_LLM_SERVE_TOKEN", "")
	t.Setenv("TERM_LLM_SERVE_PUBLIC_URL", "")
	dir := filepath.Join(home, "config", "term-llm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestServeStartupErrorOrderContract(t *testing.T) {
	// A single false alias survives Cobra parsing and group validation, but
	// fails approval resolution. Earlier startup failures must still win.
	for _, tc := range []struct {
		name, config, want string
		args               []string
	}{
		{"projects conflict before limits and config", "serve: [", "--projects and --no-projects cannot be used together", []string{"--projects=false", "--no-projects=false", "--port=0", "--auto=false"}},
		{"limits before config and approval", "serve: [", "invalid --port 0", []string{"--port=0", "--auto=false"}},
		{"explicit timeout before config", "serve: [", "invalid --response-timeout 0s", []string{"--response-timeout=0", "--auto=false"}},
		{"auth before config and approval", "serve: [", "--auth bearer conflicts with --no-auth=true", []string{"--auth=bearer", "--no-auth", "--auto=false"}},
		{"config before approval", "serve: [", "failed to load config: failed to read config:", []string{"--auto=false"}},
		{"false approval alias before platforms", "default_provider: debug\n", "--auto=false is not supported", []string{"--auto=false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeServeContractConfig(t, tc.config)
			cmd, opts := bindServeContractArgs(t, append([]string{"--token=contract-token"}, tc.args...)...)
			// An invalid platform must not mask any of the earlier errors.
			err := appserve.Run(context.Background(), opts, []string{"invalid-platform"}, captureServeInvocation(cmd, opts))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("startup error = %v, want %q", err, tc.want)
			}
		})
	}
}
