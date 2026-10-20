package ask

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestAskRequiresApprovalResolverAfterValidationAndConfig(t *testing.T) {
	configErr := errors.New("invalid config")
	deps := testDependencies(io.Discard, io.Discard)
	deps.ResolveApproval = nil
	deps.LoadConfig = func() (*config.Config, error) { return nil, configErr }
	opts := Options{Progressive: true, StopWhen: "invalid"}
	if err := Run(context.Background(), nil, opts, deps); err == nil || errors.Is(err, configErr) || strings.Contains(err.Error(), "resolver") {
		t.Fatalf("validation must win: %v", err)
	}
	opts = Options{}
	if err := Run(context.Background(), nil, opts, deps); !errors.Is(err, configErr) {
		t.Fatalf("config must win: %v", err)
	}
	deps.LoadConfig = func() (*config.Config, error) { return &config.Config{}, nil }
	if err := Run(context.Background(), nil, opts, deps); err == nil || err.Error() != "ask approval resolver is required" {
		t.Fatalf("missing resolver: %v", err)
	}
}
