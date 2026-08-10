package llm

import (
	"context"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	provider := NewMockProvider("mock").AddTextResponse("Interject please")
	got, err := Classify(context.Background(), provider, "classify", 2*time.Second)
	if err != nil {
		t.Fatalf("Classify returned error: %v", err)
	}
	if got != "interject" {
		t.Fatalf("Classify = %q, want interject", got)
	}
}

func TestClassifyTimeout(t *testing.T) {
	t.Parallel()

	provider := NewMockProvider("mock").AddTurn(MockTurn{Delay: 250 * time.Millisecond, Text: "queue"})
	_, err := Classify(context.Background(), provider, "classify", 50*time.Millisecond)
	if err == nil {
		t.Fatalf("expected timeout error")
	}
}

func TestClassifyInterruptExplicitCancel(t *testing.T) {
	t.Parallel()

	for _, command := range []string{"/stop", "/cancel"} {
		if got := ClassifyInterrupt(context.Background(), nil, command, InterruptActivity{}); got != InterruptCancel {
			t.Fatalf("ClassifyInterrupt(%q) = %v, want InterruptCancel", command, got)
		}
	}
}

func TestClassifyInterruptCancellationProseIsNotImmediate(t *testing.T) {
	t.Parallel()

	for _, prose := range []string{"stop changing direction", "cancel that assumption", "abort only the upload", "/stop after this step"} {
		action, immediate := ClassifyInterruptImmediate(prose)
		if immediate || action != InterruptInterject {
			t.Fatalf("ClassifyInterruptImmediate(%q) = (%v, %v), want non-immediate interject", prose, action, immediate)
		}
		if got := ClassifyInterrupt(context.Background(), nil, prose, InterruptActivity{}); got != InterruptInterject {
			t.Fatalf("ClassifyInterrupt(%q) = %v, want fallback interject", prose, got)
		}
	}
}

func TestClassifyInterruptLLM(t *testing.T) {
	t.Parallel()

	provider := NewMockProvider("mock").AddTextResponse("queue")
	a := InterruptActivity{CurrentTask: "analyzing", ActiveTool: "shell", ProseLen: 120}
	if got := ClassifyInterrupt(context.Background(), provider, "new topic", a); got != InterruptInterject {
		t.Fatalf("ClassifyInterrupt(queue) = %v, want InterruptInterject", got)
	}

	provider = NewMockProvider("mock").AddTextResponse("interject")
	if got := ClassifyInterrupt(context.Background(), provider, "also check x", a); got != InterruptInterject {
		t.Fatalf("ClassifyInterrupt(interject) = %v, want InterruptInterject", got)
	}

	provider = NewMockProvider("mock").AddTextResponse("cancel")
	if got := ClassifyInterrupt(context.Background(), provider, "actually stop", a); got != InterruptCancel {
		t.Fatalf("ClassifyInterrupt(cancel) = %v, want InterruptCancel", got)
	}
}

func TestClassifyInterruptFallbackOnError(t *testing.T) {
	t.Parallel()

	provider := NewMockProvider("mock").AddError(context.DeadlineExceeded)
	got := ClassifyInterrupt(context.Background(), provider, "what about y", InterruptActivity{CurrentTask: "task"})
	if got != InterruptInterject {
		t.Fatalf("ClassifyInterrupt fallback = %v, want InterruptInterject", got)
	}
}
