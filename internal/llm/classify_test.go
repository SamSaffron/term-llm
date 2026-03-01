package llm

import (
	"context"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
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
	provider := NewMockProvider("mock").AddTurn(MockTurn{Delay: 250 * time.Millisecond, Text: "queue"})
	_, err := Classify(context.Background(), provider, "classify", 50*time.Millisecond)
	if err == nil {
		t.Fatalf("expected timeout error")
	}
}

func TestClassifyInterruptExplicitCancel(t *testing.T) {
	if got := ClassifyInterrupt(context.Background(), nil, "/cancel now", InterruptActivity{}); got != InterruptCancel {
		t.Fatalf("ClassifyInterrupt(cancel) = %v, want InterruptCancel", got)
	}
}

func TestClassifyInterruptLLM(t *testing.T) {
	provider := NewMockProvider("mock").AddTextResponse("queue")
	a := InterruptActivity{CurrentTask: "analyzing", ActiveTool: "shell", ProseLen: 120}
	if got := ClassifyInterrupt(context.Background(), provider, "new topic", a); got != InterruptQueue {
		t.Fatalf("ClassifyInterrupt(queue) = %v, want InterruptQueue", got)
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
	provider := NewMockProvider("mock").AddError(context.DeadlineExceeded)
	got := ClassifyInterrupt(context.Background(), provider, "what about y", InterruptActivity{CurrentTask: "task"})
	if got != InterruptQueue {
		t.Fatalf("ClassifyInterrupt fallback = %v, want InterruptQueue", got)
	}
}
