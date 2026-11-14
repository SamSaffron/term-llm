package configured

import (
	"strings"
	"time"

	"github.com/term-llm/term-llm/internal/classify"
	"github.com/term-llm/term-llm/internal/classify/backends"
	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/greprerank"
)

// New advertises only the selected, cheaply configured classifier, or returns
// nil. Credential commands, endpoint resolution, and backend construction stay
// lazy; factory is captured now and only invoked when the reranker first runs.
func New(cfg *config.Config, factory backends.Factory) *greprerank.Reranker {
	if cfg == nil || !cfg.Tools.GrepRerank.Enabled || cfg.Tools.GrepRerank.Validate() != nil {
		return nil
	}
	options := cfg.Tools.GrepRerank
	provider, err := cfg.Classify.ResolveProvider(options.Provider)
	if err != nil || !provider.Key().Configured() {
		return nil
	}
	if provider.Type == config.ClassifyProviderCloudflare && !provider.AccountIDRef().Configured() {
		return nil
	}
	budget := time.Duration(options.TimeoutMS) * time.Millisecond
	return greprerank.New(func() (classify.Backend, error) {
		conn, err := backends.Resolve(provider, backends.Overrides{Timeout: budget})
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(conn.APIKey) == "" {
			return nil, greprerank.ErrUnavailable
		}
		return factory(conn)
	}, provider.Model, greprerank.Options{Threshold: options.Threshold, Budget: budget, MaxCandidates: options.MaxCandidates})
}
