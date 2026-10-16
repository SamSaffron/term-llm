// Package rerank scores documents against a query using an HTTP reranking
// endpoint, either remote or a lazily started, shared llama-server process.
package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
)

const (
	defaultTimeout        = 20 * time.Second
	defaultStartupTimeout = 10 * time.Minute
	defaultIdleTimeout    = 10 * time.Minute
)

// ErrBusy reports that a previous (possibly timed-out) request is still being
// processed by a local reranker; callers should fall back instead of queueing.
var ErrBusy = errors.New("reranker is busy with a previous request")

// Sizer is implemented by providers that learn their throughput and can say
// how many documents of about docChars characters fit in the request timeout.
// MaxDocuments returns 0 when throughput is not yet known.
type Sizer interface {
	MaxDocuments(docChars int) int
}

// Result is the relevance score of the document at Index in the input slice.
type Result struct {
	Index int
	Score float64
}

// Provider scores documents for relevance to a query.
type Provider interface {
	// Name identifies the provider.
	Name() string
	// Rerank returns results sorted by descending score. topN <= 0 or larger
	// than len(documents) returns every document.
	Rerank(ctx context.Context, query string, documents []string, topN int) ([]Result, error)
}

// NewProvider builds a provider from config. It returns (nil, nil) when
// reranking is disabled ("none") or when the provider is unset and no
// llama-server binary is available.
func NewProvider(cfg config.RerankConfig) (Provider, error) {
	timeout, err := parseDuration(cfg.Timeout, defaultTimeout)
	if err != nil {
		return nil, fmt.Errorf("rerank.timeout: %w", err)
	}
	switch strings.TrimSpace(cfg.Provider) {
	case "none":
		return nil, nil
	case "http":
		return newHTTPProvider(cfg, timeout)
	case "", "llamacpp":
		binary := strings.TrimSpace(cfg.Llamacpp.Binary)
		if binary == "" {
			binary = "llama-server"
		}
		path, err := exec.LookPath(binary)
		if err != nil {
			if cfg.Provider == "" && cfg.Llamacpp.Binary == "" {
				return nil, nil
			}
			return nil, fmt.Errorf("llama-server binary %q not found in PATH: install llama.cpp, set rerank.llamacpp.binary to its path, or set rerank.provider to http or none: %w", binary, err)
		}
		return newLlamacppProvider(cfg, path, timeout)
	default:
		return nil, fmt.Errorf("unknown rerank provider %q", cfg.Provider)
	}
}

func parseDuration(s string, def time.Duration) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration %q must be positive", s)
	}
	return d, nil
}

type rerankRequest struct {
	Model     string   `json:"model,omitempty"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n,omitempty"`
}

type rerankResponse struct {
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
	Results []struct {
		Index          int      `json:"index"`
		RelevanceScore *float64 `json:"relevance_score"`
		Score          *float64 `json:"score"`
	} `json:"results"`
}

// doRerank posts a Jina/Cohere/llama.cpp-style rerank request to url.
func doRerank(ctx context.Context, client *http.Client, url, apiKey, model, query string, docs []string, topN int) ([]Result, error) {
	results, _, err := doRerankUsage(ctx, client, url, apiKey, model, query, docs, topN)
	return results, err
}

// doRerankUsage is doRerank that also returns the reported prompt tokens
// (0 when the server does not report usage).
func doRerankUsage(ctx context.Context, client *http.Client, url, apiKey, model, query string, docs []string, topN int) ([]Result, int, error) {
	if len(docs) == 0 {
		return nil, 0, nil
	}
	if topN <= 0 || topN > len(docs) {
		topN = len(docs)
	}
	body, err := json.Marshal(rerankRequest{Model: model, Query: query, Documents: docs, TopN: topN})
	if err != nil {
		return nil, 0, fmt.Errorf("encode rerank request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("build rerank request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("rerank request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("read rerank response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, 0, fmt.Errorf("rerank request failed: status %d: %s", resp.StatusCode, msg)
	}
	var out rerankResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, 0, fmt.Errorf("decode rerank response: %w", err)
	}
	results := make([]Result, 0, len(out.Results))
	seen := make(map[int]bool, len(out.Results))
	for _, r := range out.Results {
		if r.Index < 0 || r.Index >= len(docs) {
			return nil, 0, fmt.Errorf("rerank response index %d out of range", r.Index)
		}
		s := r.RelevanceScore
		if s == nil {
			s = r.Score
		}
		if s == nil {
			return nil, 0, fmt.Errorf("rerank response missing score for index %d", r.Index)
		}
		if seen[r.Index] {
			return nil, 0, fmt.Errorf("rerank response repeats index %d", r.Index)
		}
		seen[r.Index] = true
		results = append(results, Result{Index: r.Index, Score: *s})
	}
	sort.SliceStable(results, func(a, b int) bool { return results[a].Score > results[b].Score })
	if len(results) > topN {
		results = results[:topN]
	}
	return results, out.Usage.PromptTokens, nil
}

// rerankURL resolves a configured base URL to the rerank endpoint. Explicit
// /rerank, /reranking and /v1/... endpoints are preserved; a /v1 base gets
// /rerank appended and anything else gets /v1/rerank.
func rerankURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	switch {
	case strings.HasSuffix(base, "/rerank"), strings.HasSuffix(base, "/reranking"):
		return base
	case strings.HasSuffix(base, "/v1"):
		return base + "/rerank"
	default:
		return base + "/v1/rerank"
	}
}

type httpProvider struct {
	url    string
	model  string
	cfg    config.RerankConfig
	client *http.Client
}

func newHTTPProvider(cfg config.RerankConfig, timeout time.Duration) (Provider, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("rerank.base_url is required for http provider")
	}
	url := rerankURL(base)
	if cfg.Path != "" {
		url = base + "/" + strings.TrimLeft(cfg.Path, "/")
	}
	model := strings.TrimSpace(cfg.Model)
	return &httpProvider{url: url, model: model, cfg: cfg, client: &http.Client{Timeout: timeout}}, nil
}

func (p *httpProvider) Name() string { return "http" }

func (p *httpProvider) Rerank(ctx context.Context, query string, docs []string, topN int) ([]Result, error) {
	key, err := p.cfg.Key().Resolve()
	if err != nil {
		return nil, fmt.Errorf("resolve rerank api key: %w", err)
	}
	return doRerank(ctx, p.client, p.url, key, p.model, query, docs, topN)
}
