package config

import (
	"fmt"
	"math"
)

// Defaults and bounds for tools.grep_rerank.
const (
	DefaultGrepRerankEnabled       = true
	DefaultGrepRerankThreshold     = 0.5
	DefaultGrepRerankTimeoutMS     = 2000
	DefaultGrepRerankMaxCandidates = 150

	MaxGrepRerankTimeoutMS     = 10000
	MinGrepRerankMaxCandidates = 2
	MaxGrepRerankMaxCandidates = 500
)

// GrepRerankConfig configures classifier-backed intent reranking for the grep
// tool. The feature only takes effect when a classify provider is usable.
type GrepRerankConfig struct {
	Enabled       bool    `mapstructure:"enabled" yaml:"enabled"`
	Provider      string  `mapstructure:"provider" yaml:"provider,omitempty"` // classify provider name; empty uses classify.default_provider
	Threshold     float64 `mapstructure:"threshold" yaml:"threshold"`
	TimeoutMS     int     `mapstructure:"timeout_ms" yaml:"timeout_ms"`
	MaxCandidates int     `mapstructure:"max_candidates" yaml:"max_candidates"`
}

// Validate rejects unusable numeric settings. An unknown provider name is not
// an error here: it simply leaves the feature unavailable.
func (c GrepRerankConfig) Validate() error {
	if math.IsNaN(c.Threshold) || math.IsInf(c.Threshold, 0) || c.Threshold < 0 || c.Threshold > 1 {
		return fmt.Errorf("tools.grep_rerank.threshold must be between 0 and 1")
	}
	if c.TimeoutMS <= 0 || c.TimeoutMS > MaxGrepRerankTimeoutMS {
		return fmt.Errorf("tools.grep_rerank.timeout_ms must be between 1 and %d", MaxGrepRerankTimeoutMS)
	}
	if c.MaxCandidates < MinGrepRerankMaxCandidates || c.MaxCandidates > MaxGrepRerankMaxCandidates {
		return fmt.Errorf("tools.grep_rerank.max_candidates must be between %d and %d", MinGrepRerankMaxCandidates, MaxGrepRerankMaxCandidates)
	}
	return nil
}
