package search

import (
	"context"
	"sync"

	"github.com/samsaffron/term-llm/internal/config"
)

// lazySearcher defers provider construction (and therefore credential
// resolution) until the first query. Building a tool registry must never
// unlock a vault for a search provider the session may never use.
type lazySearcher struct {
	cfg  *config.Config
	mu   sync.Mutex
	impl Searcher
}

// NewLazySearcher returns a Searcher that resolves the configured provider and
// its credentials on first use. Construction errors are returned to the caller
// and retried on the next query, never silently switching search providers.
func NewLazySearcher(cfg *config.Config) Searcher {
	return &lazySearcher{cfg: cfg}
}

func (l *lazySearcher) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	searcher, err := l.searcher()
	if err != nil {
		return nil, err
	}
	return searcher.Search(ctx, query, maxResults)
}

func (l *lazySearcher) searcher() (Searcher, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.impl == nil {
		searcher, err := NewSearcher(l.cfg)
		if err != nil {
			return nil, err
		}
		l.impl = searcher
	}
	return l.impl, nil
}

// NewLazyExaMCPClient defers endpoint and credential resolution until the
// first search or fetch call.
func NewLazyExaMCPClient(resolve func() (url string, apiKey string, err error)) *ExaMCPClient {
	return &ExaMCPClient{resolve: resolve}
}
