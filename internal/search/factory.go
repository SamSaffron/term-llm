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
		provider = "exa_mcp"
	}

	switch provider {
	case "exa":
		if cfg.Search.Exa.APIKey == "" {
			return nil, fmt.Errorf("exa search requires EXA_API_KEY")
		}
		return NewExaSearcher(cfg.Search.Exa.APIKey, nil), nil

	case "exa_mcp":
		return NewExaMCPClient(cfg.Search.ExaMCP.URL, cfg.Search.ExaMCP.APIKey), nil

	case "perplexity":
		if cfg.Search.Perplexity.APIKey == "" {
			return nil, fmt.Errorf("perplexity search requires PERPLEXITY_API_KEY")
		}
		return NewPerplexitySearcher(cfg.Search.Perplexity.APIKey, nil), nil

	case "tavily":
		if cfg.Search.Tavily.APIKey == "" {
			return nil, fmt.Errorf("tavily search requires TAVILY_API_KEY")
		}
		return NewTavilySearcher(cfg.Search.Tavily.APIKey, nil), nil

	case "brave":
		if cfg.Search.Brave.APIKey == "" {
			return nil, fmt.Errorf("brave search requires BRAVE_API_KEY")
		}
		return NewBraveSearcher(cfg.Search.Brave.APIKey, nil), nil

	case "google":
		if cfg.Search.Google.APIKey == "" {
			return nil, fmt.Errorf("google search requires GOOGLE_SEARCH_API_KEY")
		}
		if cfg.Search.Google.CX == "" {
			return nil, fmt.Errorf("google search requires GOOGLE_SEARCH_CX (Custom Search Engine ID)")
		}
		return NewGoogleSearcher(cfg.Search.Google.APIKey, cfg.Search.Google.CX, nil), nil

	case "duckduckgo":
		return NewDuckDuckGoLite(nil), nil

	default:
		return nil, fmt.Errorf("unknown search provider: %s (valid: exa, exa_mcp, perplexity, tavily, brave, google, duckduckgo)", provider)
	}
}
