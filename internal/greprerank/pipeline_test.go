package greprerank

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunAttemptBoundsAddedRendering(t *testing.T) {
	r := newRR(okBackend(), Options{Budget: 50 * time.Millisecond})
	attempt, err := r.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer close(release)
	started := time.Now()
	text, err := RunAttempt(context.Background(), attempt, func(_ context.Context, publish func(string)) (string, error) {
		publish("ordinary prefix")
		return "expanded matches", nil
	}, func(context.Context, string) (string, error) {
		<-release // non-cooperative rendering must not hold up the caller
		return "late ranked output", nil
	}, func(prefix string) string { return prefix })
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond+250*time.Millisecond {
		t.Fatalf("rendering exceeded budget: %v", elapsed)
	}
	if text != "ordinary prefix" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

func TestRunAttemptIncludesSearchCompletionInBudget(t *testing.T) {
	r := newRR(okBackend(), Options{Budget: 50 * time.Millisecond})
	attempt, _ := r.Begin(context.Background())
	started := time.Now()
	text, err := RunAttempt(context.Background(), attempt, func(context.Context, func(string)) (string, error) { return "all ordinary matches", nil }, func(ctx context.Context, _ string) (string, error) { <-ctx.Done(); return "", ctx.Err() }, func(prefix string) string { return prefix })
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond+250*time.Millisecond {
		t.Fatalf("completion path exceeded budget: %v", elapsed)
	}
	if text != "all ordinary matches" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("text=%q err=%v", text, err)
	}
}
