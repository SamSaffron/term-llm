package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
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
