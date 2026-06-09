package cmd

import (
	"math"
	"testing"
	"time"

	memorydb "github.com/samsaffron/term-llm/internal/memory"
)

func TestNormalizeCosineScoreNegative(t *testing.T) {
	if got := normalizeCosineScore(-0.25); got != 0 {
		t.Fatalf("normalizeCosineScore(-0.25) = %f, want 0", got)
	}
}

func TestMemorySearchDoesNotApplyDecayByDefault(t *testing.T) {
	withMemorySearchScoringFlags(t)

	candidates := []memorydb.ScoredFragment{{
		ID:         "old",
		Path:       "fragments/old.md",
		Score:      10,
		DecayScore: 0.1,
		UpdatedAt:  time.Now().Add(-365 * 24 * time.Hour),
	}}

	merged := mergeSearchCandidates(nil, nil, "", "", candidates, nil)
	if len(merged) != 1 {
		t.Fatalf("mergeSearchCandidates len = %d, want 1", len(merged))
	}
	if math.Abs(merged[0].mergedScore-0.5) > 1e-9 {
		t.Fatalf("merged score = %f, want un-decayed BM25 normalization 0.5", merged[0].mergedScore)
	}
}

func TestMemorySearchDecayAndFreshnessAreOptIn(t *testing.T) {
	withMemorySearchScoringFlags(t)

	now := time.Now().UTC()
	frag := memorydb.ScoredFragment{
		ID:         "old",
		Path:       "fragments/old.md",
		Score:      0.8,
		DecayScore: 0.25,
		UpdatedAt:  now.Add(-30 * 24 * time.Hour),
	}

	if got := applySearchScoreModifiers(frag.Score, frag, now); got != frag.Score {
		t.Fatalf("default score = %f, want %f", got, frag.Score)
	}

	memorySearchApplyDecay = true
	if got := applySearchScoreModifiers(frag.Score, frag, now); math.Abs(got-0.2) > 1e-9 {
		t.Fatalf("decay opt-in score = %f, want 0.2", got)
	}

	memorySearchFreshness = true
	memorySearchFreshnessHalfLife = 30
	if got := applySearchScoreModifiers(frag.Score, frag, now); math.Abs(got-0.1) > 1e-9 {
		t.Fatalf("decay+freshness opt-in score = %f, want 0.1", got)
	}
}

func TestMemorySearchBM25ModifiersResortResults(t *testing.T) {
	withMemorySearchScoringFlags(t)
	memorySearchApplyDecay = true

	in := []memorydb.ScoredFragment{
		{ID: "stale", Score: 10, DecayScore: 0.1, UpdatedAt: time.Now().Add(-time.Hour)},
		{ID: "kept", Score: 2, DecayScore: 1.0, UpdatedAt: time.Now()},
	}
	out := applySearchScoreOptions(in)
	if len(out) != 2 {
		t.Fatalf("applySearchScoreOptions len = %d, want 2", len(out))
	}
	if out[0].ID != "kept" {
		t.Fatalf("top result = %s, want kept after decay modifier", out[0].ID)
	}
	if in[0].Score != 10 {
		t.Fatalf("applySearchScoreOptions mutated input score = %f", in[0].Score)
	}
}

func withMemorySearchScoringFlags(t *testing.T) {
	t.Helper()
	oldApplyDecay := memorySearchApplyDecay
	oldFreshness := memorySearchFreshness
	oldHalfLife := memorySearchFreshnessHalfLife
	t.Cleanup(func() {
		memorySearchApplyDecay = oldApplyDecay
		memorySearchFreshness = oldFreshness
		memorySearchFreshnessHalfLife = oldHalfLife
	})
	memorySearchApplyDecay = false
	memorySearchFreshness = false
	memorySearchFreshnessHalfLife = memorydb.DefaultDecayHalfLifeDays
}
