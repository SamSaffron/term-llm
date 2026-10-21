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

func (t *GrepTool) finishGrep(ctx context.Context, a GrepArgs, root, intent string, result ripgrepResult, contextLines, maxResults int, useRG bool) string {
	matches := result.matches
	note := ""
	if intent != "" && len(matches) >= 2 {
		ranked, message, ok := t.rankGrep(ctx, a.Pattern, root, intent, matches, maxResults)
		if ctx.Err() != nil {
			return "grep timed out after 1 minute; try a more specific pattern or path"
		}
		if ok {
			return formatRankedGrepResults(ranked, message)
		}
		note = message
	}
	// A fallback uses the ordinary ordering and formatter, never partial scores.
	// Limit before auto-enrichment so it cannot resurrect the larger candidate set.
	if intent != "" {
		matches = sortGrepMatchesByMtime(matches)
		if len(matches) > maxResults {
			matches = matches[:maxResults]
		}
	}
	if useRG {
		matches, result.truncated = t.autoEnrichGrep(ctx, a, root, matches, contextLines, maxResults, result.truncated)
		matches = sortGrepMatchesByMtime(matches)
	}
	return note + formatGrepResults(matches, result.truncated || len(matches) >= maxResults)
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

func (t *GrepTool) rankGrep(ctx context.Context, pattern, root, intent string, matches []GrepMatch, maxResults int) ([]GrepMatch, string, bool) {
	ctx, cancel := context.WithTimeout(ctx, t.reranker.Budget())
	defer cancel()
	// Relative paths are rooted at the directory being searched (or the parent
	// of a single-file search). Path conversion is lexical: no added file I/O.
	if len(matches) > 0 && filepath.Clean(root) == filepath.Clean(matches[0].FilePath) {
		root = filepath.Dir(root)
	}
	items := make([]greprerank.Item, len(matches))
	for i, m := range matches {
		path, err := filepath.Rel(root, m.FilePath)
		if err != nil {
			return nil, grepRerankSkipped(nil), false
		}
		items[i] = greprerank.Item{Path: filepath.ToSlash(path), Block: m.Context}
	}
	result, err := t.reranker.Rank(ctx, intent, pattern, items)
	if err != nil {
		return nil, grepRerankSkipped(err), false
	}
	kept := result.Kept
	if len(kept) > maxResults {
		kept = kept[:maxResults]
	}
	ranked := make([]GrepMatch, len(kept))
	for i, index := range kept {
		ranked[i] = matches[index]
	}
	var note string
	if result.BelowThresholdAll {
		note = fmt.Sprintf("[intent rerank: no match scored >= %.2f; showing top %d by relevance — consider a different pattern]\n", t.reranker.Threshold(), len(ranked))
	} else {
		note = fmt.Sprintf("[intent rerank: showing %d of %d relevant matches (%d of %d candidates below %.2f dropped); candidate search capped at %d — broaden pattern/path or omit intent for unfiltered results]\n", len(ranked), len(result.Kept), result.Culled, len(matches), t.reranker.Threshold(), t.reranker.MaxCandidates())
	}
	return ranked, note, true
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
	}
	return "[intent rerank skipped (" + category + "); showing unranked results]\n"
}

// Ranked matches must not pass through formatGrepResults: it regroups by file
// and caps ten blocks/file, both of which would silently destroy rank order.
func formatRankedGrepResults(matches []GrepMatch, note string) string {
	var sb strings.Builder
	sb.WriteString(note)
	const capNote = "\n[output capped at 50KB — narrow your search]\n"
	for _, m := range matches {
		block := fmt.Sprintf("\n%s:\n%s\n", m.FilePath, m.Context)
		if sb.Len()+len(block)+len(capNote) > maxOutputBytes {
			sb.WriteString(capNote)
			break
		}
		sb.WriteString(block)
	}
	return sb.String()
}
