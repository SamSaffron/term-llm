package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestApplyPersistedContextEstimateSeedsEngineBaseline(t *testing.T) {
	engine := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
	sess := &session.Session{
		LastTotalTokens: 336_000,
	}

	applyPersistedContextEstimate(engine, sess)

	total, count := engine.ContextEstimateBaseline()
	if total != 336_000 || count != 0 {
		t.Fatalf("ContextEstimateBaseline() = (%d, %d), want (%d, %d)", total, count, 336_000, 0)
	}

	msgs := []core.Message{core.UserText("hello"), core.AssistantText("world")}
	if got := engine.EstimateTokens(msgs); got != 336_000 {
		t.Fatalf("EstimateTokens() = %d, want %d", got, 336_000)
	}
}

func TestApplyPersistedContextEstimateIgnoresEmptySessionBaseline(t *testing.T) {
	engine := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
	applyPersistedContextEstimate(engine, &session.Session{})

	total, count := engine.ContextEstimateBaseline()
	if total != 0 || count != 0 {
		t.Fatalf("ContextEstimateBaseline() = (%d, %d), want (0, 0)", total, count)
	}
}
