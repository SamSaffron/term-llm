package llm

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

type nativeCompactStub struct {
	core.Provider
	err error
}

func (s nativeCompactStub) NativeCompact(context.Context, core.NativeCompactRequest) (*core.NativeCompactResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &core.NativeCompactResult{PreTokens: 10, PostTokens: 2}, nil
}

// The three NativeCompact outcomes documented on the function.
func TestNativeCompactOutcomes(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		err      error
		wantOK   bool
		wantErr  error
		wantDone bool
	}{
		{name: "success", ctx: context.Background(), wantOK: true, wantDone: true},
		{name: "unavailable falls back", ctx: context.Background(), err: core.ErrNativeCompactUnavailable},
		{name: "failure falls back", ctx: context.Background(), err: errors.New("Not enough messages to compact.")},
		{name: "provider deadline is returned", ctx: context.Background(), err: fmt.Errorf("native: %w", context.DeadlineExceeded), wantOK: true, wantErr: context.DeadlineExceeded},
		{name: "caller cancellation wins over unavailable", ctx: cancelled, err: core.ErrNativeCompactUnavailable, wantOK: true, wantErr: context.Canceled},
		{name: "caller cancellation wins over failure", ctx: cancelled, err: errors.New("native failed"), wantOK: true, wantErr: context.Canceled},
		// A compaction that already succeeded is kept, even if the caller has
		// since been cancelled.
		{name: "completed compaction is kept after cancellation", ctx: cancelled, wantOK: true, wantDone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, ok, err := NativeCompact(tc.ctx, nativeCompactStub{err: tc.err}, core.NativeCompactRequest{})
			if ok != tc.wantOK || !errors.Is(err, tc.wantErr) || (result != nil) != tc.wantDone {
				t.Fatalf("NativeCompact = (%v, %t, %v), want ok=%t err=%v compacted=%t", result, ok, err, tc.wantOK, tc.wantErr, tc.wantDone)
			}
		})
	}
	if _, ok, err := NativeCompact(context.Background(), nativeCompactStub{}.Provider, core.NativeCompactRequest{}); ok || err != nil {
		t.Fatalf("non-compactor provider: ok=%t err=%v, want fallback", ok, err)
	}
}
