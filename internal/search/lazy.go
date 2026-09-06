package search

import (
	"context"
	"log"
	"sync"

	"github.com/samsaffron/term-llm/internal/config"
)

// lazySearcher defers provider construction (and therefore credential
// resolution) until the first query. Building a tool registry must never
// unlock a vault for a search provider the session may never use.
type lazySearcher struct {
	cfg  *config.Config
	once sync.Once
	impl Searcher
}

// NewLazySearcher returns a Searcher that resolves the configured provider and
// its credentials on first use, falling back to DuckDuckGo when the configured
// provider cannot be constructed.
func NewLazySearcher(cfg *config.Config) Searcher {
	return &lazySearcher{cfg: cfg}
}

func (l *lazySearcher) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	l.once.Do(func() {
		searcher, err := NewSearcher(l.cfg)
		if err != nil {
			log.Printf("Warning: search provider error: %v, falling back to DuckDuckGo", err)
			searcher = NewDuckDuckGoLite(nil)
		}
		l.impl = searcher
	})
	return l.impl.Search(ctx, query, maxResults)
}

// NewLazyExaMCPClient defers endpoint and credential resolution until the
// first search or fetch call.
func NewLazyExaMCPClient(resolve func() (url string, apiKey string, err error)) *ExaMCPClient {
	return &ExaMCPClient{resolve: resolve}
}
