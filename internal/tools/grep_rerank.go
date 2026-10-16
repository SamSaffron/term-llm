package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/rerank"
)

const (
	maxGrepRerankDocChars = 1200
	// defaultGrepRerankShown is how many reranked blocks query mode shows when
	// the caller does not pass max_results.
	defaultGrepRerankShown = 10
)

func (t *GrepTool) rerankPool() int {
	if t.config != nil && t.config.Rerank.Pool > 0 {
		return t.config.Rerank.Pool
	}
	return config.DefaultRerankPool
}

func (t *GrepTool) grepReranker(a GrepArgs) (rerank.Provider, error) {
	if strings.TrimSpace(a.Query) == "" || a.FilesWithMatches {
		return nil, nil
	}
	if t.config == nil {
		return rerank.NewProvider(config.RerankConfig{})
	}
	if t.config.RerankProvider != nil {
		return t.config.RerankProvider, nil
	}
	return rerank.NewProvider(t.config.Rerank)
}

// grepRerankPlan resolves the optional reranker and, when one is available,
// widens the candidate search and picks how many ranked blocks to show.
func (t *GrepTool) grepRerankPlan(a GrepArgs, maxResults int) (provider rerank.Provider, providerErr error, searchLimit, shown int) {
	provider, providerErr = t.grepReranker(a)
	searchLimit, shown = maxResults, maxResults
	if provider == nil {
		return provider, providerErr, searchLimit, shown
	}
	searchLimit = min(10000, max(2000, t.rerankPool()*10, maxResults*10))
	if a.MaxResults <= 0 {
		shown = defaultGrepRerankShown
	}
	return provider, providerErr, searchLimit, shown
}

func grepFallbackNote(a GrepArgs, err error, provider rerank.Provider) string {
	if strings.TrimSpace(a.Query) == "" {
		return ""
	}
	if a.FilesWithMatches {
		return "Reranking not applied to files_with_matches; showing normal grep results.\n"
	}
	if provider != nil {
		return ""
	}
	if err != nil {
		return fmt.Sprintf("Reranking unavailable (%v); showing normal grep results.\n", err)
	}
	return "Reranking unavailable (no provider configured); showing normal grep results.\n"
}

func (t *GrepTool) rerankGrepMatches(ctx context.Context, query string, provider rerank.Provider, matches []GrepMatch, maxResults int, truncated bool) string {
	poolSize := t.rerankPool()
	if s, ok := provider.(rerank.Sizer); ok {
		if n := s.MaxDocuments(maxGrepRerankDocChars); n > 0 && n < poolSize {
			poolSize = n // fit the measured throughput within the timeout
		}
	}
	pool := prefilterGrepMatches(query, matches, poolSize)
	docs := make([]string, len(pool))
	for i, m := range pool {
		docs[i] = grepRerankDocument(m)
	}
	results, err := provider.Rerank(ctx, query, docs, min(maxResults, len(docs)))
	if err != nil || len(results) == 0 {
		note := ""
		switch {
		case errors.Is(err, rerank.ErrBusy):
			note = "Reranker busy with a previous search; showing normal grep results.\n"
		case errors.Is(err, rerank.ErrWarmingUp):
			note = "Reranker starting (first use may download the model); showing normal grep results.\n"
		case err == nil:
			note = "Reranking failed (empty response); showing normal grep results.\n"
		default:
			note = fmt.Sprintf("Reranking failed (%v); showing normal grep results.\n", err)
		}
		return note +
			formatGrepResults(sortGrepMatchesByMtime(matches[:min(maxResults, len(matches))]), truncated || len(matches) > maxResults)
	}
	seen := make(map[int]bool)
	ranked := make([]GrepMatch, 0, min(maxResults, len(results)))
	for _, r := range results {
		if r.Index < 0 || r.Index >= len(pool) || seen[r.Index] {
			return "Reranking failed (invalid result index); showing normal grep results.\n" +
				formatGrepResults(sortGrepMatchesByMtime(matches[:min(maxResults, len(matches))]), truncated || len(matches) > maxResults)
		}
		seen[r.Index] = true
		ranked = append(ranked, pool[r.Index])
		if len(ranked) >= maxResults {
			break
		}
	}
	return fmt.Sprintf("Reranked %d of %d match blocks (provider %s); showing top %d.\n", len(pool), len(matches), provider.Name(), len(ranked)) + formatRankedGrepResults(ranked, truncated)
}

func grepRerankDocument(m GrepMatch) string {
	doc := []rune(fmt.Sprintf("%s:%d-%d\n%s", m.FilePath, m.LineNumber, grepBlockEndLine(m), m.Context))
	if len(doc) > maxGrepRerankDocChars {
		doc = doc[:maxGrepRerankDocChars]
	}
	return string(doc)
}

func grepBlockEndLine(m GrepMatch) int {
	end := m.LineNumber
	for _, line := range strings.Split(m.Context, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, ">"))
		number, _, ok := strings.Cut(line, ":")
		if ok {
			if n, err := strconv.Atoi(number); err == nil && n > end {
				end = n
			}
		}
	}
	return end
}

// formatRankedGrepResults retains the existing file/match format while avoiding
// file grouping, which would silently scramble relevance order across files.
func formatRankedGrepResults(matches []GrepMatch, truncated bool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d matches in %d files\n", len(matches), countGrepFiles(matches))
	for i, m := range matches {
		formatted := formatGrepResults([]GrepMatch{m}, false)
		_, block, _ := strings.Cut(formatted, "\n")
		if i > 0 && sb.Len()+len(block) > maxOutputBytes {
			fmt.Fprintf(&sb, "\n[output capped at 50KB — %d match blocks not shown]\n", len(matches)-i)
			return sb.String()
		}
		sb.WriteString(block)
	}
	if truncated {
		sb.WriteString("\n[Candidate search truncated at safety limit]")
	}
	return sb.String()
}

func countGrepFiles(matches []GrepMatch) int {
	seen := make(map[string]bool)
	for _, m := range matches {
		seen[m.FilePath] = true
	}
	return len(seen)
}

// prefilterGrepMatches uses BM25 to avoid sending very broad searches to the
// reranker. Ties preserve ripgrep encounter order, including zero-score blocks.
func prefilterGrepMatches(query string, matches []GrepMatch, pool int) []GrepMatch {
	if pool <= 0 || len(matches) <= pool {
		return matches
	}
	terms := lexicalTerms(query)
	if len(terms) == 0 {
		return matches[:pool]
	}
	unique := make(map[string]bool, len(terms))
	queryTerms := terms[:0]
	for _, term := range terms {
		if !unique[term] {
			unique[term] = true
			queryTerms = append(queryTerms, term)
		}
	}
	terms = queryTerms
	type document struct {
		index  int
		length int
		counts map[string]int
		score  float64
	}
	docs := make([]document, len(matches))
	df := make(map[string]int)
	totalLength := 0
	for i, m := range matches {
		words := lexicalTerms(m.FilePath + " " + m.Context)
		d := document{index: i, length: len(words), counts: make(map[string]int)}
		for _, word := range words {
			d.counts[word]++
		}
		for _, term := range terms {
			if d.counts[term] > 0 {
				df[term]++
			}
		}
		docs[i] = d
		totalLength += d.length
	}
	avgLength := math.Max(1, float64(totalLength)/float64(len(docs)))
	for i := range docs {
		d := &docs[i]
		for _, term := range terms {
			freq := float64(d.counts[term])
			if freq == 0 {
				continue
			}
			idf := math.Log(1 + (float64(len(docs)-df[term])+0.5)/(float64(df[term])+0.5))
			d.score += idf * freq * 2.2 / (freq + 1.2*(0.25+0.75*float64(d.length)/avgLength))
		}
	}
	sort.SliceStable(docs, func(i, j int) bool { return docs[i].score > docs[j].score })
	out := make([]GrepMatch, 0, pool)
	for _, d := range docs[:pool] {
		out = append(out, matches[d.index])
	}
	return out
}

func lexicalTerms(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}
