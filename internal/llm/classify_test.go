package llm

import (
	"context"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	provider := mockprovider.NewMockProvider("mock").AddTextResponse("Steer please")
	got, err := engine.Classify(context.Background(), provider, "classify", 2*time.Second)
	if err != nil {
		t.Fatalf("Classify returned error: %v", err)
	}
	if got != "steer" {
		t.Fatalf("Classify = %q, want steer", got)
	}
}

func TestClassifyTimeout(t *testing.T) {
	t.Parallel()

	provider := mockprovider.NewMockProvider("mock").AddTurn(mockprovider.MockTurn{Delay: 250 * time.Millisecond, Text: "queue"})
	_, err := engine.Classify(context.Background(), provider, "classify", 50*time.Millisecond)
	if err == nil {
		t.Fatalf("expected timeout error")
	}
}

func TestClassifyInterruptExplicitCancel(t *testing.T) {
	t.Parallel()

	for _, command := range []string{"/stop", "/cancel"} {
		if got := engine.ClassifyInterrupt(context.Background(), nil, command, engine.InterruptActivity{}); got != engine.InterruptCancel {
			t.Fatalf("ClassifyInterrupt(%q) = %v, want InterruptCancel", command, got)
		}
	}
}

func TestClassifyInterruptCancellationProseIsNotImmediate(t *testing.T) {
	t.Parallel()

	for _, prose := range []string{"stop changing direction", "cancel that assumption", "abort only the upload", "/stop after this step"} {
		action, immediate := engine.ClassifyInterruptImmediate(prose)
		if immediate || action != engine.InterruptSteer {
			t.Fatalf("ClassifyInterruptImmediate(%q) = (%v, %v), want non-immediate steer", prose, action, immediate)
		}
		if got := engine.ClassifyInterrupt(context.Background(), nil, prose, engine.InterruptActivity{}); got != engine.InterruptSteer {
			t.Fatalf("ClassifyInterrupt(%q) = %v, want fallback steer", prose, got)
		}
	}
}

func TestClassifyInterruptLLM(t *testing.T) {
	t.Parallel()

	provider := mockprovider.NewMockProvider("mock").AddTextResponse("queue")
	a := engine.InterruptActivity{CurrentTask: "analyzing", ActiveTool: "shell", ProseLen: 120}
	if got := engine.ClassifyInterrupt(context.Background(), provider, "new topic", a); got != engine.InterruptSteer {
		t.Fatalf("ClassifyInterrupt(queue) = %v, want InterruptSteer", got)
	}

	provider = mockprovider.NewMockProvider("mock").AddTextResponse("steer")
	if got := engine.ClassifyInterrupt(context.Background(), provider, "also check x", a); got != engine.InterruptSteer {
		t.Fatalf("ClassifyInterrupt(steer) = %v, want InterruptSteer", got)
	}

	provider = mockprovider.NewMockProvider("mock").AddTextResponse("cancel")
	if got := engine.ClassifyInterrupt(context.Background(), provider, "actually stop", a); got != engine.InterruptCancel {
		t.Fatalf("ClassifyInterrupt(cancel) = %v, want InterruptCancel", got)
	}
}

func TestClassifyInterruptFallbackOnError(t *testing.T) {
	t.Parallel()

	provider := mockprovider.NewMockProvider("mock").AddError(context.DeadlineExceeded)
	got := engine.ClassifyInterrupt(context.Background(), provider, "what about y", engine.InterruptActivity{CurrentTask: "task"})
	if got != engine.InterruptSteer {
		t.Fatalf("ClassifyInterrupt fallback = %v, want InterruptSteer", got)
	}
}
