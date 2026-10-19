package cmd_test

import (
	"context"
	"strings"
	"testing"

	app "github.com/samsaffron/term-llm/cmd"
	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"
)

func TestServePublicInvocationEntrypointsValidateBeforeConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		run        func() error
	}{
		{"web", "invalid --port 0", func() error {
			return app.Run(context.Background(), &appserve.Options{}, []string{"web"}, appserve.Invocation{})
		}},
		{"mcp", "--tools is required", func() error {
			return app.RunMCP(context.Background(), &appserve.MCPOptions{}, nil, appserve.MCPInvocation{})
		}},
		{"hub", "invalid --port 0", func() error {
			return app.RunHub(context.Background(), apphub.Options{Host: "127.0.0.1", AuthMode: "bearer"}, apphub.Invocation{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("entrypoint error = %v, want %q", err, tc.want)
			}
		})
	}
}
