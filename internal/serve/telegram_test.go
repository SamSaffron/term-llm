package serve

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

func TestTelegramSessionMgrGetOrCreate_DoesNotBlockOtherChatsWhileCreating(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})

	mgr := &telegramSessionMgr{
		sessions: make(map[int64]*telegramSession),
		settings: Settings{
			NewSession: func(ctx context.Context) (*SessionRuntime, error) {
				started <- struct{}{}
				<-release
				return &SessionRuntime{
					ProviderName: "mock",
					ModelName:    "model",
				}, nil
			},
		},
	}

	errCh := make(chan error, 2)
	go func() {
		_, err := mgr.getOrCreate(context.Background(), 1)
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("first NewSession call did not start")
	}

	go func() {
		_, err := mgr.getOrCreate(context.Background(), 2)
		errCh <- err
	}()
	select {
	case <-started:
		// This confirms the second chat reached NewSession before the first one completed.
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("second getOrCreate blocked while first session was creating")
	}

	close(release)
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("getOrCreate returned error: %v", err)
		}
	}
}

func TestTelegramSessionMgrResetSessionIfCurrent_RejectsStaleExpectedSession(t *testing.T) {
	var cleanupCalls atomic.Int32
	mgr := &telegramSessionMgr{
		sessions: make(map[int64]*telegramSession),
		settings: Settings{
			NewSession: func(ctx context.Context) (*SessionRuntime, error) {
				return &SessionRuntime{
					ProviderName: "mock",
					ModelName:    "model",
					Cleanup: func() {
						cleanupCalls.Add(1)
					},
				}, nil
			},
		},
	}

	original, err := mgr.getOrCreate(context.Background(), 42)
	if err != nil {
		t.Fatalf("getOrCreate failed: %v", err)
	}

	replacement, replaced, err := mgr.resetSessionIfCurrent(context.Background(), 42, original)
	if err != nil {
		t.Fatalf("resetSessionIfCurrent failed: %v", err)
	}
	if !replaced {
		t.Fatalf("expected first reset to replace session")
	}
	if replacement == original {
		t.Fatalf("expected a new replacement session")
	}

	got, replaced, err := mgr.resetSessionIfCurrent(context.Background(), 42, original)
	if err != nil {
		t.Fatalf("second resetSessionIfCurrent failed: %v", err)
	}
	if replaced {
		t.Fatalf("expected stale reset to be ignored")
	}
	if got != replacement {
		t.Fatalf("expected current replacement session to be returned for stale reset")
	}
	if cleanupCalls.Load() != 2 {
		t.Fatalf("cleanup calls = %d, want 2 (original closed + stale duplicate closed)", cleanupCalls.Load())
	}
}

func TestTelegramSessionMgrRunStoreOpWithTimeout_UsesLiveContext(t *testing.T) {
	mgr := &telegramSessionMgr{
		store: &session.NoopStore{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var sawCanceled bool
	mgr.runStoreOp(ctx, "sess-1", "op", func(opCtx context.Context) error {
		sawCanceled = opCtx.Err() != nil
		return nil
	})
	if !sawCanceled {
		t.Fatalf("runStoreOp should pass through canceled context")
	}

	var sawLive bool
	var sawDeadline bool
	mgr.runStoreOpWithTimeout("sess-1", "op_timeout", func(opCtx context.Context) error {
		sawLive = opCtx.Err() == nil
		_, sawDeadline = opCtx.Deadline()
		return nil
	})
	if !sawLive {
		t.Fatalf("runStoreOpWithTimeout should use a live context")
	}
	if !sawDeadline {
		t.Fatalf("runStoreOpWithTimeout should set a deadline")
	}
}
