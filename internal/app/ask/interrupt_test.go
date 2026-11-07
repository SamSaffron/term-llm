package ask

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRunInterruptionRequiresLiveCancellation(t *testing.T) {
	providerErr := fmt.Errorf("provider: %w", errors.New("connection failed"))
	tests := []struct {
		name string
		ctx  func() context.Context
		err  error
		want bool
	}{
		{name: "live context with joined cleanup cancellation", ctx: context.Background, err: errors.Join(providerErr, context.Canceled), want: false},
		{name: "live context with provider error", ctx: context.Background, err: providerErr, want: false},
		{name: "nil error", ctx: context.Background, err: nil, want: false},
		{name: "renderer user interruption with live context", ctx: context.Background, err: errUserInterrupted, want: true},
		{name: "cancelled context", ctx: func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}, err: providerErr, want: true},
		{name: "deadline context", ctx: func() context.Context {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
			defer cancel()
			return ctx
		}, err: context.DeadlineExceeded, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := runInterruption(tc.ctx(), tc.err); got != tc.want {
				t.Fatalf("runInterruption = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTerminalRunErrorPrefersProviderFailure(t *testing.T) {
	providerErr := fmt.Errorf("provider failed")
	tests := []struct {
		name       string
		displayErr error
		streamErr  error
		want       string
	}{
		{name: "display cancellation yields to provider failure", displayErr: context.Canceled, streamErr: providerErr, want: "streaming failed: provider failed"},
		{name: "provider failure without display error", streamErr: providerErr, want: "streaming failed: provider failed"},
		{name: "display error remains when stream canceled", displayErr: fmt.Errorf("render failed"), streamErr: context.Canceled, want: "render failed"},
		{name: "both substantive errors stay visible", displayErr: fmt.Errorf("render failed"), streamErr: providerErr, want: "render failed\nprovider failed"},
		{name: "renderer interruption yields to provider failure", displayErr: errUserInterrupted, streamErr: providerErr, want: "streaming failed: provider failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := terminalRunError(tc.displayErr, tc.streamErr)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("terminalRunError = %v, want %q", err, tc.want)
			}
		})
	}
}
