package search

import (
	"fmt"

	"github.com/samsaffron/term-llm/internal/config"
)

// NewSearcher creates a Searcher based on the config.
// Returns DuckDuckGo as the default if no provider is specified.
func NewSearcher(cfg *config.Config) (Searcher, error) {
	provider := cfg.Search.Provider
	if provider == "" {
		provider = "duckduckgo"
	}

	switch provider {
	case "exa":
		if cfg.Search.Exa.APIKey == "" {
			return nil, fmt.Errorf("exa search requires EXA_API_KEY")
		}
		return NewExaSearcher(cfg.Search.Exa.APIKey, nil), nil

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
		return nil, fmt.Errorf("unknown search provider: %s (valid: exa, tavily, brave, google, duckduckgo)", provider)
	}
}
