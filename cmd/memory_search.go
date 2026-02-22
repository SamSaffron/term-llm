package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/samsaffron/term-llm/internal/embedding"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/spf13/cobra"
)

const (
	memorySearchCandidateLimit = 24
	memorySearchVectorWeight   = 0.7
	memorySearchBM25Weight     = 0.3
	memorySearchMMRLambda      = 0.5
	memorySearchMinScore       = 0.35
)

var (
	memorySearchLimit         int
	memorySearchJSON          bool
	memorySearchEmbedProvider string
	memorySearchNoDecay       bool
	memorySearchBM25Only      bool
)

type hybridSearchCandidate struct {
	fragment    memorydb.ScoredFragment
	vectorScore float64
	bm25Score   float64
	mergedScore float64
}

var memorySearchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search memory fragments (hybrid BM25 + vector)",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runMemorySearch,
}

func init() {
	memorySearchCmd.Flags().IntVar(&memorySearchLimit, "limit", 6, "Maximum number of results")
	memorySearchCmd.Flags().BoolVar(&memorySearchJSON, "json", false, "Output as JSON")
	memorySearchCmd.Flags().StringVar(&memorySearchEmbedProvider, "embed-provider", "", "Override embedding provider for query embedding (optionally provider:model)")
	memorySearchCmd.Flags().BoolVar(&memorySearchNoDecay, "no-decay", false, "Ignore decay score multiplier")
	memorySearchCmd.Flags().BoolVar(&memorySearchBM25Only, "bm25-only", false, "Force BM25-only retrieval")
	memorySearchCmd.RegisterFlagCompletionFunc("embed-provider", EmbedProviderFlagCompletion)
}

func runMemorySearch(cmd *cobra.Command, args []string) error {
	store, err := openMemoryStore()
	if err != nil {
		return err
	}
	defer store.Close()

	query := strings.TrimSpace(strings.Join(args, " "))
	if query == "" {
		return fmt.Errorf("query cannot be empty")
	}

	if memorySearchLimit <= 0 {
		memorySearchLimit = 6
	}

	ctx := context.Background()
	results, err := searchMemory(ctx, store, query)
	if err != nil {
		return err
	}

	if len(results) > 0 {
		for _, r := range results {
			if r.ID == "" {
				continue
			}
			if err := store.BumpAccess(ctx, r.ID); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to bump access for fragment %s: %v\n", r.ID, err)
			}
		}
	}

	output := projectSearchResults(results)
	if memorySearchJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(output)
	}

	if len(output) == 0 {
		fmt.Println("No memory fragments found.")
		return nil
	}

	if strings.TrimSpace(memoryAgent) == "" {
		fmt.Printf("%-14s %-36s %s\n", "AGENT", "PATH", "SNIPPET")
		fmt.Println(strings.Repeat("-", 108))
		for _, r := range output {
			fmt.Printf("%-14s %-36s %s\n", r.Agent, truncateString(r.Path, 36), oneLine(truncateString(r.Snippet, 64)))
		}
		return nil
	}

	fmt.Printf("%-36s %s\n", "PATH", "SNIPPET")
	fmt.Println(strings.Repeat("-", 96))
	for _, r := range output {
		fmt.Printf("%-36s %s\n", truncateString(r.Path, 36), oneLine(truncateString(r.Snippet, 64)))
	}

	return nil
}

func searchMemory(ctx context.Context, store *memorydb.Store, query string) ([]memorydb.ScoredFragment, error) {
	agent := strings.TrimSpace(memoryAgent)

	if memorySearchBM25Only {
		return store.SearchBM25(ctx, query, memorySearchLimit, agent)
	}

	bm25Candidates, err := store.SearchBM25(ctx, query, memorySearchCandidateLimit, agent)
	if err != nil {
		return nil, err
	}

	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}

	providerName, modelName, providerSpec := resolveMemoryEmbeddingProvider(cfg, memorySearchEmbedProvider)
	if providerName == "" || providerSpec == "" {
		fmt.Fprintln(os.Stderr, "warning: embedding provider unavailable, falling back to BM25-only search")
		return limitScoredFragments(bm25Candidates, memorySearchLimit), nil
	}

	embedder, err := embedding.NewEmbeddingProvider(cfg, providerSpec)
	if err != nil {
		if strings.TrimSpace(memorySearchEmbedProvider) != "" {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "warning: embedding provider initialization failed (%v), falling back to BM25-only search\n", err)
		return limitScoredFragments(bm25Candidates, memorySearchLimit), nil
	}

	embRes, err := embedder.Embed(embedding.EmbedRequest{
		Texts:    []string{query},
		Model:    modelName,
		TaskType: "RETRIEVAL_QUERY",
	})
	if err != nil {
		if strings.TrimSpace(memorySearchEmbedProvider) != "" {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "warning: query embedding failed (%v), falling back to BM25-only search\n", err)
		return limitScoredFragments(bm25Candidates, memorySearchLimit), nil
	}
	if len(embRes.Embeddings) == 0 || len(embRes.Embeddings[0].Vector) == 0 {
		fmt.Fprintln(os.Stderr, "warning: empty query embedding result, falling back to BM25-only search")
		return limitScoredFragments(bm25Candidates, memorySearchLimit), nil
	}

	queryVec := embRes.Embeddings[0].Vector
	if modelName == "" {
		modelName = strings.TrimSpace(embRes.Model)
	}

	vectorCandidates, err := store.VectorSearch(ctx, agent, providerName, modelName, queryVec, memorySearchCandidateLimit)
	if err != nil {
		return nil, err
	}

	merged := mergeSearchCandidates(ctx, store, providerName, modelName, bm25Candidates, vectorCandidates)
	if len(merged) == 0 {
		return nil, nil
	}

	reranked := rerankMMR(merged)
	out := make([]memorydb.ScoredFragment, 0, memorySearchLimit)
	for _, c := range reranked {
		if c.mergedScore < memorySearchMinScore {
			continue
		}
		out = append(out, c.fragment)
		if len(out) >= memorySearchLimit {
			break
		}
	}
	return out, nil
}

func mergeSearchCandidates(ctx context.Context, store *memorydb.Store, provider, model string, bm25, vector []memorydb.ScoredFragment) []*hybridSearchCandidate {
	bm25Norm := normalizeBM25Scores(bm25)
	merged := map[string]*hybridSearchCandidate{}

	for _, candidate := range vector {
		entry := merged[candidate.ID]
		if entry == nil {
			c := candidate
			entry = &hybridSearchCandidate{fragment: c}
			merged[candidate.ID] = entry
		}
		entry.vectorScore = maxFloat(entry.vectorScore, normalizeCosineScore(candidate.Score))
		if entry.fragment.Snippet == "" {
			entry.fragment.Snippet = snippetFromContent(candidate.Content)
		}
	}

	for _, candidate := range bm25 {
		entry := merged[candidate.ID]
		if entry == nil {
			c := candidate
			entry = &hybridSearchCandidate{fragment: c}
			merged[candidate.ID] = entry
		}
		entry.bm25Score = maxFloat(entry.bm25Score, bm25Norm[candidate.ID])
		if entry.fragment.Snippet == "" {
			entry.fragment.Snippet = candidate.Snippet
		}
	}

	if provider != "" && model != "" {
		missing := make([]string, 0, len(merged))
		for id, entry := range merged {
			if len(entry.fragment.Vector) == 0 {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			vectors, err := store.GetEmbeddingsByIDs(ctx, missing, provider, model)
			if err == nil {
				for id, vec := range vectors {
					if entry := merged[id]; entry != nil && len(entry.fragment.Vector) == 0 && len(vec) > 0 {
						entry.fragment.Vector = vec
					}
				}
			}
		}
	}

	out := make([]*hybridSearchCandidate, 0, len(merged))
	for _, candidate := range merged {
		score := memorySearchVectorWeight*candidate.vectorScore + memorySearchBM25Weight*candidate.bm25Score
		if !memorySearchNoDecay {
			score *= candidate.fragment.DecayScore
		}
		candidate.mergedScore = score
		candidate.fragment.Score = score
		if candidate.fragment.Snippet == "" {
			candidate.fragment.Snippet = snippetFromContent(candidate.fragment.Content)
		}
		out = append(out, candidate)
	}
	return out
}

func rerankMMR(candidates []*hybridSearchCandidate) []*hybridSearchCandidate {
	remaining := append([]*hybridSearchCandidate(nil), candidates...)
	selected := make([]*hybridSearchCandidate, 0, len(candidates))

	for len(remaining) > 0 {
		bestIdx := 0
		bestScore := math.Inf(-1)
		for i, candidate := range remaining {
			maxSimilarity := 0.0
			for _, picked := range selected {
				sim := candidateSimilarity(candidate.fragment, picked.fragment)
				if sim > maxSimilarity {
					maxSimilarity = sim
				}
			}
			// MMR only affects ordering; mergedScore drives the final min-score cutoff.
			mmrScore := memorySearchMMRLambda*candidate.mergedScore - (1.0-memorySearchMMRLambda)*maxSimilarity
			if mmrScore > bestScore {
				bestScore = mmrScore
				bestIdx = i
			}
		}

		selected = append(selected, remaining[bestIdx])
		remaining = append(remaining[:bestIdx], remaining[bestIdx+1:]...)
	}

	return selected
}

func candidateSimilarity(a, b memorydb.ScoredFragment) float64 {
	if len(a.Vector) == 0 || len(b.Vector) == 0 {
		return 0
	}
	sim := embedding.CosineSimilarity(a.Vector, b.Vector)
	if sim < 0 {
		return 0
	}
	if sim > 1 {
		return 1
	}
	return sim
}

func normalizeBM25Scores(candidates []memorydb.ScoredFragment) map[string]float64 {
	out := make(map[string]float64, len(candidates))
	if len(candidates) == 0 {
		return out
	}

	transformed := make([]float64, len(candidates))
	minScore := math.Inf(1)
	maxScore := math.Inf(-1)
	for i, candidate := range candidates {
		relevance := candidate.Score
		transformed[i] = relevance
		if relevance < minScore {
			minScore = relevance
		}
		if relevance > maxScore {
			maxScore = relevance
		}
	}

	if maxScore-minScore < 1e-9 {
		for _, candidate := range candidates {
			out[candidate.ID] = 0.5
		}
		return out
	}

	for i, candidate := range candidates {
		norm := (transformed[i] - minScore) / (maxScore - minScore)
		if norm < 0 {
			norm = 0
		} else if norm > 1 {
			norm = 1
		}
		out[candidate.ID] = norm
	}
	return out
}

func normalizeCosineScore(raw float64) float64 {
	if raw < 0 {
		return 0
	}
	if raw > 1 {
		return 1
	}
	return raw
}

func limitScoredFragments(in []memorydb.ScoredFragment, limit int) []memorydb.ScoredFragment {
	if limit <= 0 || len(in) <= limit {
		return in
	}
	return in[:limit]
}

func projectSearchResults(results []memorydb.ScoredFragment) []memorydb.SearchResult {
	out := make([]memorydb.SearchResult, 0, len(results))
	for _, result := range results {
		snippet := result.Snippet
		if snippet == "" {
			snippet = snippetFromContent(result.Content)
		}
		out = append(out, memorydb.SearchResult{
			Agent:   result.Agent,
			Path:    result.Path,
			Snippet: snippet,
			Score:   result.Score,
		})
	}
	return out
}

func snippetFromContent(content string) string {
	content = oneLine(strings.TrimSpace(content))
	return truncateString(content, 140)
}

func maxFloat(a, b float64) float64 {
	if b > a {
		return b
	}
	return a
}

func truncateString(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.TrimSpace(s)
}
