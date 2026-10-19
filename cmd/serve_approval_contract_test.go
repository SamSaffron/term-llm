package cmd

import (
	"context"
	"io"
	"strings"
	"testing"

	appserve "github.com/samsaffron/term-llm/internal/app/serve"
)

func TestServeInvocationRequiresApprovalResolverAfterValidationAndConfig(t *testing.T) {
	for _, surface := range []string{"serve", "mcp"} {
		for _, phase := range []string{"validation", "config", "resolver"} {
			t.Run(surface+"/"+phase, func(t *testing.T) {
				cfg := "default_provider: debug\n"
				if phase != "resolver" {
					cfg = "serve: ["
				}
				writeServeContractConfig(t, cfg)
				opts := serveRuntimeFixtureOptions()
				mcpOpts := appserve.MCPOptions{Tools: "read_file", Port: 8080, EditFormat: "edit_file"}
				want := "approval resolver is required"
				if phase == "validation" {
					opts.Port = 0
					mcpOpts.Port = 0
					want = "invalid --port"
				}
				if phase == "config" {
					want = "load config"
					if surface == "serve" {
						want = "failed to load config"
					}
				}
				var err error
				if surface == "serve" {
					err = Run(context.Background(), &opts, []string{"api"}, appserve.Invocation{Stdout: io.Discard, Stderr: io.Discard})
				} else {
					err = RunMCP(context.Background(), &mcpOpts, nil, appserve.MCPInvocation{Stdout: io.Discard, Stderr: io.Discard})
				}
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %v, want %q", err, want)
				}
			})
		}
	}
}
