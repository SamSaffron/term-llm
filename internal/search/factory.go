package search

import (
	"fmt"

	"github.com/samsaffron/term-llm/internal/config"
)

// NewSearcher creates a Searcher based on the config.
// Returns Exa MCP as the default if no provider is specified.
func NewSearcher(cfg *config.Config) (Searcher, error) {
	provider := cfg.Search.Provider
	if provider == "" {
		provider = config.DefaultSearchProvider
	}

	switch provider {
	case "exa":
		apiKey, err := cfg.Search.ExaKey().Resolve()
		if err != nil {
			return nil, fmt.Errorf("search.exa.api_key: %w", err)
		}
		if apiKey == "" {
			return nil, fmt.Errorf("exa search requires EXA_API_KEY")
		}
		return NewExaSearcher(apiKey, nil), nil

	case "exa_mcp":
		url, err := cfg.Search.ExaMCPURLRef().Resolve()
		if err != nil {
			return nil, fmt.Errorf("search.exa_mcp.url: %w", err)
		}
		apiKey, err := cfg.Search.ExaMCPKey().Resolve()
		if err != nil {
			return nil, fmt.Errorf("search.exa_mcp.api_key: %w", err)
		}
		return NewExaMCPClient(url, apiKey), nil

	case "perplexity":
		apiKey, err := cfg.Search.PerplexityKey().Resolve()
		if err != nil {
			return nil, fmt.Errorf("search.perplexity.api_key: %w", err)
		}
		if apiKey == "" {
			return nil, fmt.Errorf("perplexity search requires PERPLEXITY_API_KEY")
		}
		return NewPerplexitySearcher(apiKey, nil), nil

	case "parallel":
		apiKey, err := cfg.Search.ParallelKey().Resolve()
		if err != nil {
			return nil, fmt.Errorf("search.parallel.api_key: %w", err)
		}
		if apiKey == "" {
			return nil, fmt.Errorf("parallel search requires PARALLEL_API_KEY")
		}
		return NewParallelSearcher(apiKey, nil), nil

	case "tavily":
		apiKey, err := cfg.Search.TavilyKey().Resolve()
		if err != nil {
			return nil, fmt.Errorf("search.tavily.api_key: %w", err)
		}
		if apiKey == "" {
			return nil, fmt.Errorf("tavily search requires TAVILY_API_KEY")
		}
		return NewTavilySearcher(apiKey, nil), nil

	case "brave":
		apiKey, err := cfg.Search.BraveKey().Resolve()
		if err != nil {
			return nil, fmt.Errorf("search.brave.api_key: %w", err)
		}
		if apiKey == "" {
			return nil, fmt.Errorf("brave search requires BRAVE_API_KEY")
		}
		return NewBraveSearcher(apiKey, nil), nil

	case "google":
		apiKey, err := cfg.Search.GoogleKey().Resolve()
		if err != nil {
			return nil, fmt.Errorf("search.google.api_key: %w", err)
		}
		if apiKey == "" {
			return nil, fmt.Errorf("google search requires GOOGLE_SEARCH_API_KEY")
		}
		cx, err := cfg.Search.GoogleCXRef().Resolve()
		if err != nil {
			return nil, fmt.Errorf("search.google.cx: %w", err)
		}
		if cx == "" {
			return nil, fmt.Errorf("google search requires GOOGLE_SEARCH_CX (Custom Search Engine ID)")
		}
		return NewGoogleSearcher(apiKey, cx, nil), nil

	case "duckduckgo":
		return NewDuckDuckGoLite(nil), nil

	default:
		return nil, fmt.Errorf("unknown search provider: %s (valid: exa, exa_mcp, perplexity, parallel, tavily, brave, google, duckduckgo)", provider)
	}
}

// Available reports whether the configured search provider has everything it
// needs, without resolving deferred credentials. Use it for "should this tool
// be advertised?" decisions, which must never unlock a vault.
func Available(cfg *config.Config) error {
	provider := cfg.Search.Provider
	if provider == "" {
		provider = config.DefaultSearchProvider
	}

	switch provider {
	case "exa":
		if !cfg.Search.ExaKey().Configured() {
			return fmt.Errorf("exa search requires EXA_API_KEY")
		}
	case "perplexity":
		if !cfg.Search.PerplexityKey().Configured() {
			return fmt.Errorf("perplexity search requires PERPLEXITY_API_KEY")
		}
	case "parallel":
		if !cfg.Search.ParallelKey().Configured() {
			return fmt.Errorf("parallel search requires PARALLEL_API_KEY")
		}
	case "tavily":
		if !cfg.Search.TavilyKey().Configured() {
			return fmt.Errorf("tavily search requires TAVILY_API_KEY")
		}
	case "brave":
		if !cfg.Search.BraveKey().Configured() {
			return fmt.Errorf("brave search requires BRAVE_API_KEY")
		}
	case "google":
		if !cfg.Search.GoogleKey().Configured() {
			return fmt.Errorf("google search requires GOOGLE_SEARCH_API_KEY")
		}
		if !cfg.Search.GoogleCXRef().Configured() {
			return fmt.Errorf("google search requires GOOGLE_SEARCH_CX (Custom Search Engine ID)")
		}
	case "exa_mcp", "duckduckgo":
		// No credentials required.
	default:
		return fmt.Errorf("unknown search provider: %s (valid: exa, exa_mcp, perplexity, parallel, tavily, brave, google, duckduckgo)", provider)
	}
	return nil
}
