package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/classify"
	"github.com/samsaffron/term-llm/internal/classify/backends"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/greprerank"
)

var newGrepRerankBackend backends.Factory = backends.New

// NewGrepReranker advertises only the selected, cheaply configured classifier.
// Credential commands, endpoint resolution, and backend construction stay lazy.
func NewGrepReranker(cfg *config.Config) *greprerank.Reranker {
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
	// Capture the factory now: registry construction never invokes it.
	factory := newGrepRerankBackend
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

// WithReranker binds the registry-owned capability, including after SetLimits.
func (t *GrepTool) WithReranker(r *greprerank.Reranker) *GrepTool {
	t.reranker = r
	return t
}

func (t *GrepTool) knownGrepParams() []string {
	keys := []string{"pattern", "path", "include", "exclude", "type", "max_results", "context_lines", "files_with_matches", "multiline"}
	if t.reranker != nil {
		keys = append(keys, "intent")
	}
	return keys
}

// Decode separately so disabled grep continues to ignore ALL intent values,
// including non-strings, through its original unknown-parameter warning path.
func (t *GrepTool) grepIntent(args json.RawMessage, a GrepArgs, maxResults int) (string, error) {
	if t.reranker == nil {
		return "", nil
	}
	var extra struct {
		Intent string `json:"intent"`
	}
	if err := json.Unmarshal(args, &extra); err != nil {
		return "", err
	}
	if a.FilesWithMatches || maxResults > t.reranker.MaxCandidates() || strings.TrimSpace(extra.Intent) == "" {
		return "", nil
	}
	return extra.Intent, nil
}

// finishGrep keeps ordinary enrichment and ordering unchanged. Intent results
// use the same bounded pipeline as a streamed search, including direct fixtures.
func (t *GrepTool) finishGrep(ctx context.Context, a GrepArgs, root, intent string, result ripgrepResult, contextLines, maxResults int, useRG bool) string {
	if intent != "" {
		return t.intentPipeline(ctx, a, root, intent, maxResults, func(_ context.Context, publish func(intentSearchResult), _ *greprerank.Attempt) (intentSearchResult, error) {
			value := intentSearchResult{ripgrepResult: result}
			publish(value)
			return value, nil
		})
	}
	matches := result.matches
	if useRG {
		matches, result.truncated = t.autoEnrichGrep(ctx, a, root, matches, contextLines, maxResults, result.truncated)
		matches = sortGrepMatchesByMtime(matches)
	}
	truncated := result.truncated || len(matches) >= maxResults
	return formatGrepDisplay(t.grepDisplayBase(), matches, func(m []GrepMatch) string { return formatGrepResults(m, truncated) })
}

func (t *GrepTool) autoEnrichGrep(ctx context.Context, a GrepArgs, root string, matches []GrepMatch, contextLines, maxResults int, truncated bool) ([]GrepMatch, bool) {
	if a.ContextLines <= 0 {
		if enriched := autoEnrichContextLines(len(matches)); enriched > contextLines {
			if a.Multiline {
				if richer, err := t.executeRipgrep(ctx, a, root, enriched, maxResults); err == nil && len(richer.matches) > 0 {
					matches, truncated = richer.matches, richer.truncated
				}
			} else if richer, err := enrichGrepMatchesFromFiles(ctx, matches, enriched); err == nil && len(richer) > 0 {
				matches = richer
			}
		}
	}
	return matches, truncated
}

func (t *GrepTool) executeIntentGrep(ctx context.Context, a GrepArgs, root, intent string, contextLines, maxResults int) string {
	return t.intentPipeline(ctx, a, root, intent, maxResults, func(ctx context.Context, publish func(intentSearchResult), attempt *greprerank.Attempt) (intentSearchResult, error) {
		return t.searchIntent(ctx, a, root, contextLines, maxResults, attempt, publish)
	})
}

func (t *GrepTool) intentPipeline(ctx context.Context, a GrepArgs, root, intent string, maxResults int, search func(context.Context, func(intentSearchResult), *greprerank.Attempt) (intentSearchResult, error)) string {
	attempt, err := t.reranker.Begin(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return "grep timed out after 1 minute; try a more specific pattern or path"
		}
		return grepRerankSkipped(err) + t.executePlainGrep(ctx, a, root, grepContextLines(a.ContextLines), maxResults)
	}
	text, err := greprerank.RunAttempt(ctx, attempt, func(ctx context.Context, publish func(intentSearchResult)) (intentSearchResult, error) {
		return search(ctx, publish, attempt)
	}, func(ctx context.Context, value intentSearchResult) (string, error) {
		if len(value.matches) < 2 {
			return "", greprerank.ErrSkipped
		}
		items := grepRerankItems(root, value.matches)
		result, err := attempt.Rank(intent, a.Pattern, items)
		if err != nil {
			return "", err
		}
		kept := result.Kept[:min(len(result.Kept), maxResults)]
		matches := make([]GrepMatch, len(kept))
		for i, index := range kept {
			matches[i] = value.matches[index]
		}
		note := func(shown int) string {
			if result.BelowThresholdAll {
				return fmt.Sprintf("[intent rerank: no match scored >= %.2f; showing top %d by relevance — consider a different pattern]\n", t.reranker.Threshold(), shown)
			}
			note := fmt.Sprintf("[intent rerank: showing %d of %d relevant matches (%d of %d candidates below %.2f dropped)", shown, len(result.Kept), result.Culled, len(value.matches), t.reranker.Threshold())
			if value.truncated {
				// Only worth the tokens when matches were actually left unranked.
				note += fmt.Sprintf("; search stopped at %d raw matches, the rest were not ranked — narrow pattern/path to rank them all", t.reranker.MaxCandidates())
			}
			return note + "]\n"
		}
		return formatGrepDisplay(t.grepDisplayBase(), matches, func(m []GrepMatch) string { return formatRankedGrepResults(m, note) }), ctx.Err()
	}, func(value intentSearchResult) string {
		return formatIntentPrefix(value, maxResults, a.ContextLines, t.grepDisplayBase())
	})
	if errors.Is(err, greprerank.ErrBusy) {
		// Saturation never starts another detached search; use the untouched
		// ordinary path synchronously, with its normal cap and enrichment.
		text = t.executePlainGrep(ctx, a, root, grepContextLines(a.ContextLines), maxResults)
	}
	if ctx.Err() != nil {
		return "grep timed out after 1 minute; try a more specific pattern or path"
	}
	if err != nil && !errors.Is(err, greprerank.ErrSkipped) {
		return grepRerankSkipped(err) + text
	}
	return text
}

func formatIntentPrefix(value intentSearchResult, maxResults, contextLines int, displayBase string) string {
	if value.message != "" {
		return value.message
	}
	matches := value.matches
	if value.snapshot != nil {
		matches, _ = parseRipgrepOutput(value.snapshot(), maxResults, grepContextLines(contextLines))
	}
	if len(matches) == 0 {
		return "No matches found."
	}
	matches = sortGrepMatchesByMtime(matches)
	if len(matches) > maxResults {
		matches = matches[:maxResults]
	}
	truncated := value.truncated || len(matches) >= maxResults
	return formatGrepDisplay(displayBase, matches, func(m []GrepMatch) string { return formatGrepResults(m, truncated) })
}

func grepRerankItems(root string, matches []GrepMatch) []greprerank.Item {
	if len(matches) > 0 && filepath.Clean(root) == filepath.Clean(matches[0].FilePath) {
		root = filepath.Dir(root)
	}
	items := make([]greprerank.Item, len(matches))
	for i, m := range matches {
		path, err := filepath.Rel(root, m.FilePath)
		if err != nil {
			path = filepath.Base(m.FilePath)
		}
		items[i] = greprerank.Item{Path: filepath.ToSlash(path), Block: m.Context}
	}
	return items
}

func grepRerankSkipped(err error) string {
	category := "classifier error"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		category = "timeout"
	case errors.Is(err, greprerank.ErrUnavailable):
		category = "classifier unavailable"
	case errors.Is(err, greprerank.ErrBreakerOpen):
		category = "temporarily disabled after repeated failures"
	case errors.Is(err, greprerank.ErrBusy):
		category = "busy"
	}
	return "[intent rerank skipped (" + category + "); showing unranked results]\n"
}

func grepContextLines(lines int) int {
	if lines <= 0 {
		return 2
	}
	return lines
}
