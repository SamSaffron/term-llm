package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	mcpoauth "github.com/samsaffron/term-llm/internal/mcp/oauth"
	"golang.org/x/oauth2"
)

type stuckMCPStartup struct{}

func (stuckMCPStartup) ServerStatus(string) (mcp.ServerStatus, error) {
	return mcp.StatusStarting, nil
}

func TestWaitForMCPStartupBounded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		context func() (context.Context, context.CancelFunc)
		want    string
		wantErr error
	}{
		{name: "fallback deadline", context: func() (context.Context, context.CancelFunc) {
			return t.Context(), func() {}
		}, want: "MCP servers still starting after startup timeout: slow, stuck"},
		{name: "caller cancelled", context: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx, cancel
		}, want: "MCP servers still starting: slow, stuck", wantErr: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.context()
			defer cancel()
			err := waitForMCPStartup(ctx, stuckMCPStartup{}, []string{"slow", "stuck"}, io.Discard, false, 20*time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), tc.want) || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("error = %v, want %q (cause %v)", err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestAskMCPStartup(t *testing.T) {
	for _, tc := range []struct {
		name            string
		delay, deadline time.Duration
		wantTimeout     bool
	}{
		// Cross ask's former 10-second cutoff while staying within the manager's budget.
		{name: "slow server becomes ready", delay: 11 * time.Second, deadline: 20 * time.Second},
		{name: "deadline names the server", delay: time.Minute, deadline: 500 * time.Millisecond, wantTimeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), tc.deadline)
			defer cancel()
			server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "slow", Version: "1"}, nil)
			server.AddTool(&sdkmcp.Tool{Name: "test", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
				return &sdkmcp.CallToolResult{}, nil
			})
			handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{Stateless: true})
			var first sync.Once
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				first.Do(func() {
					timer := time.NewTimer(tc.delay)
					defer timer.Stop()
					select {
					case <-timer.C:
					case <-r.Context().Done():
					case <-ctx.Done():
					}
				})
				if r.Context().Err() == nil && ctx.Err() == nil {
					handler.ServeHTTP(w, r)
				}
			}))
			defer func() {
				cancel()
				httpServer.Close()
			}()
			writeServeMCPConfig(t, map[string]mcp.ServerConfig{"slow": {Type: "http", URL: httpServer.URL}})
			var feedback bytes.Buffer
			manager, err := enableMCPServersWithFeedback(ctx, "slow", llm.NewEngine(nil, nil), &feedback, nil)
			if got := feedback.String(); strings.ContainsAny(got, "\r⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏") || !strings.HasPrefix(got, "Starting MCP: slow\n") {
				t.Fatalf("non-terminal startup feedback = %q", got)
			}
			if manager != nil {
				defer manager.StopAll()
			}
			if tc.wantTimeout {
				if err == nil || !strings.Contains(err.Error(), "slow (startup timed out:") {
					t.Fatalf("error = %v, want named startup timeout", err)
				}
				if manager != nil {
					t.Fatal("failed startup returned a manager")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(feedback.String(), "✓ MCP ready: 1 tools from slow\n") {
					t.Fatalf("missing ready feedback: %q", feedback.String())
				}
				if manager == nil || len(manager.AllTools()) != 1 {
					t.Fatal("slow server's tools were not loaded")
				}
			}
		})
	}
}

func TestCheckMCPServerFailures(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "ready", Version: "1"}, nil)
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{Stateless: true})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth" || r.URL.Path == "/other-auth" {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer httpServer.Close()
	writeServeMCPConfig(t, map[string]mcp.ServerConfig{})
	manager := mcp.NewManagerWithConfig(&mcp.Config{Servers: map[string]mcp.ServerConfig{
		"ready":      {URL: httpServer.URL + "/ready"},
		"failed":     {Command: filepath.Join(t.TempDir(), "missing-command")},
		"auth":       {URL: httpServer.URL + "/auth"},
		"other-auth": {URL: httpServer.URL + "/other-auth"},
	}})
	defer manager.StopAll()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for _, name := range manager.AvailableServers() {
		if err := manager.Enable(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := waitForMCPStartup(ctx, manager, manager.AvailableServers(), io.Discard, false, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		servers []string
		want    []string
	}{
		{name: "ready", servers: []string{"ready"}},
		{name: "failed", servers: []string{"failed"}, want: []string{"MCP servers failed to start: failed ("}},
		{name: "auth required", servers: []string{"auth"}, want: []string{"MCP servers need sign-in: auth", "term-llm mcp login auth"}},
		{name: "multiple auth required", servers: []string{"auth", "other-auth"}, want: []string{"MCP servers need sign-in: auth, other-auth", "term-llm mcp login auth", "term-llm mcp login other-auth"}},
		{name: "ready and auth", servers: []string{"ready", "auth"}, want: []string{"MCP servers need sign-in: auth", "term-llm mcp login auth"}},
		{name: "mixed", servers: []string{"ready", "failed", "auth"}, want: []string{"MCP servers failed to start: failed (", "MCP servers need sign-in: auth", "term-llm mcp login auth"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var feedback bytes.Buffer
			err := checkMCPServerFailures(manager, tc.servers, &feedback, true)
			if len(tc.want) == 0 {
				if err != nil || feedback.Len() != 0 {
					t.Fatalf("ready check: err=%v feedback=%q", err, feedback.String())
				}
				return
			}
			if err == nil {
				t.Fatal("expected startup error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, missing %q", err.Error(), want)
				}
			}
			if feedback.String() != "\n" {
				t.Errorf("animated error feedback = %q", feedback.String())
			}
		})
	}
}

func TestEnableMCPServersWithFeedbackRequiresSignIn(t *testing.T) {
	protected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer protected.Close()
	for _, selection := range []string{"protected", "greeter,protected"} {
		t.Run(selection, func(t *testing.T) {
			writeServeMCPConfig(t, map[string]mcp.ServerConfig{
				"protected": {URL: protected.URL},
				"greeter":   {Command: os.Args[0], Env: map[string]string{runServeMCPHandlerTestServerEnv: "1"}},
			})
			var feedback bytes.Buffer
			manager, err := enableMCPServersWithFeedback(t.Context(), selection, llm.NewEngine(nil, llm.NewToolRegistry()), &feedback, nil)
			if manager != nil {
				defer manager.StopAll()
				t.Error("startup with missing authorization returned a manager")
			}
			if err == nil || !strings.Contains(err.Error(), "MCP servers need sign-in: protected") || !strings.Contains(err.Error(), "term-llm mcp login protected") {
				t.Fatalf("startup error = %v", err)
			}
			if strings.Contains(feedback.String(), "✓ MCP ready") {
				t.Fatalf("misleading ready feedback: %q", feedback.String())
			}
		})
	}
}

func TestCheckMCPServerFailuresInsufficientScope(t *testing.T) {
	if !runServeMCPOAuthTestIsolated(t) {
		return
	}
	path, err := mcpoauth.DefaultStorePath()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	_, err = mcpoauth.NewFileStore(path).Update(server.URL, func(*mcpoauth.Session) (*mcpoauth.Session, error) {
		return &mcpoauth.Session{Endpoint: server.URL, Config: mcpoauth.OAuth2Config{ClientID: "client"}, Token: &oauth2.Token{AccessToken: "valid", Expiry: time.Now().Add(time.Hour)}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := mcp.NewManagerWithConfig(&mcp.Config{Servers: map[string]mcp.ServerConfig{"protected": {URL: server.URL}}})
	defer manager.StopAll()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := manager.Enable(ctx, "protected"); err != nil {
		t.Fatal(err)
	}
	if err := waitForMCPStartup(ctx, manager, []string{"protected"}, io.Discard, false, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if manager.AuthStatuses()["protected"].State != mcpoauth.AuthSignedIn {
		t.Fatal("scope challenge invalidated stored grant")
	}
	err = checkMCPServerFailures(manager, []string{"protected"}, io.Discard, false)
	if err == nil || !strings.Contains(err.Error(), "term-llm mcp login --force protected") {
		t.Fatalf("scope step-up hint = %v", err)
	}
}
