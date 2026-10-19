package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
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

func TestServeStartupOptionsValidation(t *testing.T) {
	t.Parallel()
	base := ServeOptions{Port: 8080, SessionTTL: time.Minute, SessionMax: 100, JobsWorkers: 4}
	for _, tc := range []struct {
		name, errorText string
		change          func(*ServeOptions)
	}{
		{"port", "invalid --port", func(o *ServeOptions) { o.Port = 0 }},
		{"session TTL", "invalid --session-ttl", func(o *ServeOptions) { o.SessionTTL = 0 }},
		{"session max", "invalid --session-max", func(o *ServeOptions) { o.SessionMax = 0 }},
		{"Telegram carryover", "invalid --telegram-carryover-chars", func(o *ServeOptions) { o.TelegramCarryoverChars = -1 }},
		{"jobs workers", "invalid --jobs-workers", func(o *ServeOptions) { o.JobsWorkers = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := base
			tc.change(&opts)
			_, err := resolveServeStartup(&cobra.Command{}, &opts)
			if err == nil || !strings.Contains(err.Error(), tc.errorText) {
				t.Fatalf("resolveServeStartup() error = %v, want %q", err, tc.errorText)
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
			err := runServeLegacy(context.Background(), cmd, []string{"invalid-platform"}, opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("startup error = %v, want %q", err, tc.want)
			}
		})
	}
}

type serveContractWriter func([]byte) (int, error)

func (w serveContractWriter) Write(p []byte) (int, error) { return w(p) }

func TestServeStartupConfigFallbackContract(t *testing.T) {
	// Title and location sharing are local startup values, not written back to
	// Options. Observe the actual mounted HTML rather than copying the fallback
	// expressions into a test. The final startup diagnostic is our readiness
	// signal; no model calls, polling or sleeps are needed.
	for _, tc := range []struct {
		name, basePath, title, timeout string
		locationEnabled                bool
		args                           []string
	}{
		{"config fallbacks", "/configured", "Configured title", "7 minutes", false, nil},
		{"explicit defaults override config", "/ui", "", "30 minutes", true, []string{"--base-path=/ui", "--title=", "--disable-location-sharing=false", "--response-timeout=30m"}},
		{"explicit values override config", "/chat", "CLI title", "1 hour", false, []string{"--base-path=/chat/", "--title= CLI title ", "--disable-location-sharing", "--response-timeout=1h"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeServeContractConfig(t, `default_provider: debug
skills:
  enabled: false
sessions:
  enabled: false
serve:
  base_path: /configured
  title: ' Configured title '
  disable_location_sharing: true
  response_timeout: 7m
  projects:
    enabled: false
  web_push:
    vapid_public_key: fixture-public
    vapid_private_key: fixture-private
`)
			// Today's startup has no listener injection seam. Reserve an OS-picked
			// loopback port and release it just before startup; retry only an actual
			// bind collision, never a failed assertion or a different startup error.
			for attempt := 0; attempt < 3; attempt++ {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				port := listener.Addr().(*net.TCPAddr).Port
				args := append([]string{"--port=" + strconv.Itoa(port), "--token=contract-token", "--approval=prompt", "--disable-widgets", "--disable-extensions"}, tc.args...)
				cmd, opts := bindServeContractArgs(t, args...)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				var output strings.Builder
				var html string
				var observed bool
				var requestErr error
				cmd.SetErr(serveContractWriter(func(p []byte) (int, error) {
					output.Write(p)
					if strings.Contains(string(p), "response timeout:") {
						observed = true
						defer cancel()
						url := fmt.Sprintf("http://127.0.0.1:%d%s/", port, tc.basePath)
						req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
						if err != nil {
							requestErr = err
							return len(p), nil
						}
						req.Header.Set("Authorization", "Bearer contract-token")
						client := &http.Client{Transport: &http.Transport{Proxy: nil}}
						defer client.CloseIdleConnections()
						res, err := client.Do(req)
						if err != nil {
							requestErr = err
							return len(p), nil
						}
						defer res.Body.Close()
						body, err := io.ReadAll(res.Body)
						html, requestErr = string(body), err
						if res.StatusCode != http.StatusOK {
							requestErr = fmt.Errorf("GET %s: status %d", url, res.StatusCode)
						}
					}
					return len(p), nil
				}))
				if err := listener.Close(); err != nil {
					cancel()
					t.Fatal(err)
				}
				err = runServeLegacy(ctx, cmd, []string{"web"}, opts)
				cancel()
				if err != nil && strings.Contains(err.Error(), "address already in use") && attempt < 2 {
					continue
				}
				if err != nil || !observed || requestErr != nil {
					t.Fatalf("startup error=%v observed=%v request=%v\nstderr: %s", err, observed, requestErr, output.String())
				}
				for _, want := range []string{
					fmt.Sprintf("TERM_LLM_UI_PREFIX=%q", tc.basePath),
					fmt.Sprintf("TERM_LLM_UI_TITLE=%q", tc.title),
					fmt.Sprintf("TERM_LLM_LOCATION_SHARING_ENABLED=%v", tc.locationEnabled),
				} {
					if !strings.Contains(html, want) {
						t.Errorf("mounted HTML missing %s", want)
					}
				}
				if !strings.Contains(output.String(), "response timeout: "+tc.timeout+"\n") {
					t.Errorf("timeout diagnostic = %q, want %q", output.String(), tc.timeout)
				}
				return
			}
		})
	}
}

func TestServeStartupEmptyBasePathContract(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"", "--base-path must not be empty"},
		{"/", `--base-path must not be "/"`},
	} {
		t.Run("path="+tc.value, func(t *testing.T) {
			writeServeContractConfig(t, "default_provider: debug\n")
			cmd, opts := bindServeContractArgs(t, "--base-path="+tc.value, "--token=contract-token", "--approval=prompt")
			// api avoids Web Push setup and fails before listener creation.
			err := runServeLegacy(context.Background(), cmd, []string{"api"}, opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("startup error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestServeProjectsExplicitFalseContract(t *testing.T) {
	oldFlags, oldLogOutput := serveFlags, log.Writer()
	t.Cleanup(func() { serveFlags = oldFlags; log.SetOutput(oldLogOutput) })
	for _, tc := range []struct {
		name, diagnostic, wantErr string
		args                      []string
		configEnabled, warning    bool
	}{
		{"unset uses config", "projects auto-disabled", "", nil, true, true},
		{"false no-projects uses config but diagnostic is explicit", "projects explicitly disabled", "", []string{"--no-projects=false"}, true, true},
		{"true no-projects disables", "projects explicitly disabled", "", []string{"--no-projects"}, true, false},
		{"false projects overrides config", "projects auto-disabled", "", []string{"--projects=false"}, true, false},
		{"true projects is strict", "", "--projects requires a writable SQLite session store", []string{"--projects"}, false, false},
		{"disabled config", "projects explicitly disabled", "", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, opts := bindServeContractArgs(t, tc.args...)
			// This helper still reads serveFlags rather than the passed options.
			serveFlags = *opts
			cfg := &config.Config{}
			cfg.Serve.Projects.Enabled = tc.configEnabled
			var stderr, diagnostics bytes.Buffer
			log.SetOutput(&diagnostics)
			enabled, bootstrap, err := initializeServeProjectRuntime(context.Background(), cmd, cfg, nil, t.TempDir(), true, &stderr)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("project error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || enabled || bootstrap != "" {
				t.Fatalf("projects enabled=%v bootstrap=%q err=%v", enabled, bootstrap, err)
			}
			if !strings.Contains(diagnostics.String(), tc.diagnostic) || (stderr.Len() > 0) != tc.warning {
				t.Fatalf("diagnostic=%q warning=%q; want %q warning=%v", diagnostics.String(), stderr.String(), tc.diagnostic, tc.warning)
			}
		})
	}
}
