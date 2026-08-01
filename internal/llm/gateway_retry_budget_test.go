package llm

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

type retryDeadlineProvider struct {
	attempts int
	block    bool
}

func (p *retryDeadlineProvider) Name() string               { return "retry-deadline" }
func (p *retryDeadlineProvider) Credential() string         { return "mock" }
func (p *retryDeadlineProvider) Capabilities() Capabilities { return Capabilities{} }
func (p *retryDeadlineProvider) Stream(ctx context.Context, _ Request) (Stream, error) {
	p.attempts++
	if p.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, errors.New("500 Internal Server Error")
}

func drainRetryFailure(t *testing.T, provider Provider) error {
	t.Helper()
	stream, err := provider.Stream(t.Context(), Request{})
	if err != nil {
		return err
	}
	defer stream.Close()
	for {
		event, recvErr := stream.Recv()
		if recvErr != nil {
			if recvErr == io.EOF {
				return nil
			}
			return recvErr
		}
		if event.Type == EventError {
			return event.Err
		}
	}
}

type gatewayOwnedRetryProvider struct{ retryDeadlineProvider }

func (*gatewayOwnedRetryProvider) GatewayHandlesRetries() bool { return true }

func TestEngineDoesNotReplayGatewayOwnedRetryFailures(t *testing.T) {
	provider := &gatewayOwnedRetryProvider{}
	engine := NewEngine(provider, nil)
	stream, err := engine.Stream(t.Context(), Request{Model: "model", Messages: []Message{UserText("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for {
		_, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			break
		}
	}
	if provider.attempts != 1 {
		t.Fatalf("engine gateway attempts = %d, want 1", provider.attempts)
	}
}

func TestRetryProviderPersistent500HonorsAttemptLimitWithoutNestedRetries(t *testing.T) {
	inner := &retryDeadlineProvider{}
	first := WrapWithRetry(inner, RetryConfig{MaxAttempts: 20, MaxElapsedTime: time.Second, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	provider := WrapWithRetry(first, RetryConfig{MaxAttempts: 3, MaxElapsedTime: time.Second, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	started := time.Now()
	if err := drainRetryFailure(t, provider); err == nil {
		t.Fatal("persistent 500 unexpectedly succeeded")
	}
	if inner.attempts != 3 {
		t.Fatalf("attempts = %d, want exactly 3", inner.attempts)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("attempt-limited retry took %s", elapsed)
	}
}

func TestRetryProviderElapsedBudgetCancelsHungAttempt(t *testing.T) {
	inner := &retryDeadlineProvider{block: true}
	provider := WrapWithRetry(inner, RetryConfig{MaxAttempts: 5, MaxElapsedTime: 40 * time.Millisecond, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	started := time.Now()
	err := drainRetryFailure(t, provider)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if inner.attempts != 1 {
		t.Fatalf("hung attempt count = %d, want 1", inner.attempts)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("elapsed retry budget took %s", elapsed)
	}
}
