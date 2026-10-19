package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"

	"strings"
	"testing"
	"time"

	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/tools"
)

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
			_, err := resolveServeStartup(serveFlagState{}, &opts)
			if err == nil || !strings.Contains(err.Error(), tc.errorText) {
				t.Fatalf("resolveServeStartup() error = %v, want %q", err, tc.errorText)
			}
		})
	}
}

type serveContractWriter func([]byte) (int, error)

func (w serveContractWriter) Write(p []byte) (int, error) { return w(p) }

func TestRunServeWithOwnsInjectedListenerAcrossEarlyFailures(t *testing.T) {
	approvalFailure := errors.New("approval failed after config")
	for _, tc := range []struct {
		name       string
		config     string
		mutate     func(*ServeOptions)
		resolve    func(*config.Config) (resolvedApprovalMode, error)
		want       string
		wantCalled bool
	}{
		{
			name: "validation", config: "serve: [", want: "invalid --port 0",
			mutate: func(opts *ServeOptions) { opts.Port = 0 },
			resolve: func(*config.Config) (resolvedApprovalMode, error) {
				t.Fatal("approval resolver called before validation")
				return resolvedApprovalMode{}, nil
			},
		},
		{
			name: "config", config: "serve: [", want: "failed to load config:",
			resolve: func(*config.Config) (resolvedApprovalMode, error) {
				t.Fatal("approval resolver called before config failure")
				return resolvedApprovalMode{}, nil
			},
		},
		{
			name: "approval", config: "default_provider: debug\n", want: approvalFailure.Error(), wantCalled: true,
			resolve: func(*config.Config) (resolvedApprovalMode, error) { return resolvedApprovalMode{}, approvalFailure },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeServeContractConfig(t, tc.config)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			opts := serveRuntimeFixtureOptions()
			opts.Token = "contract-token"
			if tc.mutate != nil {
				tc.mutate(&opts)
			}
			called := false
			deps := serveDeps{Invocation: appserve.Invocation{
				Flags: serveFlagState{}, Stdout: io.Discard, Stderr: io.Discard,
				ResolveApproval: func(cfg *config.Config) (resolvedApprovalMode, error) {
					called = true
					return tc.resolve(cfg)
				},
			}, Listener: listener}
			err = runServeWith(context.Background(), deps, &opts, []string{"invalid-platform"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("startup error = %v, want %q", err, tc.want)
			}
			if called != tc.wantCalled {
				t.Fatalf("approval resolver called = %v, want %v", called, tc.wantCalled)
			}
			tcpListener := listener.(*net.TCPListener)
			if deadlineErr := tcpListener.SetDeadline(time.Now()); !errors.Is(deadlineErr, net.ErrClosed) {
				t.Fatalf("listener SetDeadline error = %v, want net.ErrClosed", deadlineErr)
			}
		})
	}
}

func TestServeStartupConfigFallbackContract(t *testing.T) {
	// Title and location sharing are local startup values, not written back to
	// Options. Observe the actual mounted HTML rather than copying the fallback
	// expressions into a test. The final startup diagnostic is our readiness
	// signal; no model calls, polling or sleeps are needed.
	for _, tc := range []struct {
		name, basePath, title, timeout string
		locationEnabled                bool
		mutate                         func(*ServeOptions, *serveFlagState)
	}{
		{"config fallbacks", "/configured", "Configured title", "7 minutes", false, nil},
		{"explicit defaults override config", "/ui", "", "30 minutes", true, func(opts *ServeOptions, flags *serveFlagState) {
			opts.BasePath, opts.Title, opts.DisableLocationSharing, opts.ResponseTimeout = "/ui", "", false, 30*time.Minute
			flags.BasePath, flags.Title, flags.DisableLocationSharing, flags.ResponseTimeout = true, true, true, true
		}},
		{"explicit values override config", "/chat", "CLI title", "1 hour", false, func(opts *ServeOptions, flags *serveFlagState) {
			opts.BasePath, opts.Title, opts.DisableLocationSharing, opts.ResponseTimeout = "/chat/", " CLI title ", true, time.Hour
			flags.BasePath, flags.Title, flags.DisableLocationSharing, flags.ResponseTimeout = true, true, true, true
		}},
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
			// Inject an OS-picked listener while retaining a valid nonzero Options
			// port. Startup owns the listener as soon as runServeWith begins.
			for attempt := 0; attempt < 3; attempt++ {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				port := listener.Addr().(*net.TCPAddr).Port
				opts := serveRuntimeFixtureOptions()
				opts.Port, opts.Token, opts.DisableWidgets, opts.DisableExtensions = port, "contract-token", true, true
				flags := serveFlagState{}
				if tc.mutate != nil {
					tc.mutate(&opts, &flags)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				var output strings.Builder
				var html string
				var observed bool
				var requestErr error
				stderr := serveContractWriter(func(p []byte) (int, error) {
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
				})
				deps := serveDeps{Invocation: appserve.Invocation{
					Flags: flags, Stdout: io.Discard, Stderr: stderr,
					ResolveApproval: func(*config.Config) (resolvedApprovalMode, error) {
						return resolvedApprovalMode{Mode: tools.ModePrompt}, nil
					},
				}, Listener: listener}
				err = runServeWith(ctx, deps, &opts, []string{"web"})
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
			opts := serveRuntimeFixtureOptions()
			opts.BasePath, opts.Token = tc.value, "contract-token"
			invocation := appserve.Invocation{
				Flags: serveFlagState{BasePath: true}, Stdout: io.Discard, Stderr: io.Discard,
				ResolveApproval: func(*config.Config) (resolvedApprovalMode, error) {
					return resolvedApprovalMode{Mode: tools.ModePrompt}, nil
				},
			}
			// api avoids Web Push setup and fails before listener creation.
			err := Run(context.Background(), &opts, []string{"api"}, invocation)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("startup error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestServeProjectsExplicitFalseContract(t *testing.T) {
	oldLogOutput := log.Writer()
	t.Cleanup(func() { log.SetOutput(oldLogOutput) })
	for _, tc := range []struct {
		name, diagnostic, wantErr string
		mutate                    func(*ServeOptions, *serveFlagState)
		configEnabled, warning    bool
	}{
		{"unset uses config", "projects auto-disabled", "", nil, true, true},
		{"false no-projects uses config but diagnostic is explicit", "projects explicitly disabled", "", func(opts *ServeOptions, flags *serveFlagState) { flags.NoProjects = true }, true, true},
		{"true no-projects disables", "projects explicitly disabled", "", func(opts *ServeOptions, flags *serveFlagState) { opts.NoProjects, flags.NoProjects = true, true }, true, false},
		{"false projects overrides config", "projects auto-disabled", "", func(opts *ServeOptions, flags *serveFlagState) { flags.Projects = true }, true, false},
		{"true projects is strict", "", "--projects requires a writable SQLite session store", func(opts *ServeOptions, flags *serveFlagState) { opts.Projects, flags.Projects = true, true }, false, false},
		{"disabled config", "projects explicitly disabled", "", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := ServeOptions{}
			flags := serveFlagState{}
			if tc.mutate != nil {
				tc.mutate(&opts, &flags)
			}
			cfg := &config.Config{}
			cfg.Serve.Projects.Enabled = tc.configEnabled
			var stderr, diagnostics bytes.Buffer
			log.SetOutput(&diagnostics)
			enabled, bootstrap, err := initializeServeProjectRuntime(context.Background(), flags, &opts, cfg, nil, t.TempDir(), true, &stderr)
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
