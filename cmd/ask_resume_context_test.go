package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestApplyPersistedContextEstimateSeedsEngineBaseline(t *testing.T) {
	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	sess := &session.Session{
		LastTotalTokens:  336_000,
		LastMessageCount: 2,
	}

	applyPersistedContextEstimate(engine, sess)

	total, count := engine.ContextEstimateBaseline()
	if total != 336_000 || count != 2 {
		t.Fatalf("ContextEstimateBaseline() = (%d, %d), want (%d, %d)", total, count, 336_000, 2)
	}

	msgs := []llm.Message{llm.UserText("hello"), llm.AssistantText("world")}
	if got := engine.EstimateTokens(msgs); got != 336_000 {
		t.Fatalf("EstimateTokens() = %d, want %d", got, 336_000)
	}
}

func TestApplyPersistedContextEstimateIgnoresEmptySessionBaseline(t *testing.T) {
	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	applyPersistedContextEstimate(engine, &session.Session{})

	total, count := engine.ContextEstimateBaseline()
	if total != 0 || count != 0 {
		t.Fatalf("ContextEstimateBaseline() = (%d, %d), want (0, 0)", total, count)
	}
}
