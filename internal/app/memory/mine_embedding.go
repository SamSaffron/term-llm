package memory

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/embedding"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
)

func (opts MineOptions) runMemoryEmbedPhase(ctx context.Context, cfg *config.Config, store *memorydb.Store) (int, error) {
	env, err := opts.Env.withProtection(cfg)
	if err != nil {
		return 0, err
	}
	opts.Env = env

	providerName, modelName, providerSpec := resolveMemoryEmbeddingProvider(cfg, opts.EmbedProvider)
	if providerName == "" || providerSpec == "" {
		fmt.Fprintln(os.Stderr, "warning: embedding provider unavailable, skipping EMBED phase")
		return 0, nil
	}

	embedder, err := embedding.NewEmbeddingProvider(cfg, providerSpec)
	if err != nil {
		if strings.TrimSpace(opts.EmbedProvider) != "" {
			return 0, err
		}
		fmt.Fprintf(os.Stderr, "warning: embedding provider initialization failed (%v), skipping EMBED phase\n", err)
		return 0, nil
	}

	if modelName == "" {
		modelName = embedder.DefaultModel()
	}

	fragments, err := store.GetFragmentsNeedingEmbedding(ctx, opts.AgentFilter(), providerName, modelName)
	if err != nil {
		return 0, fmt.Errorf("query fragments needing embedding: %w", err)
	}
	safeFragments := fragments[:0]
	for _, frag := range fragments {
		if !excludedMemoryPath(frag.Path) {
			safeFragments = append(safeFragments, frag)
		}
	}
	fragments = safeFragments

	if len(fragments) == 0 {
		return 0, nil
	}

	embeddedCount := 0
	embedBatchSize := cfg.Embed.BatchSize
	if embedBatchSize <= 0 {
		embedBatchSize = 32
	}
	for i := 0; i < len(fragments); i += embedBatchSize {
		end := i + embedBatchSize
		if end > len(fragments) {
			end = len(fragments)
		}
		batch := fragments[i:end]
		texts := make([]string, len(batch))
		for j, frag := range batch {
			texts[j] = opts.protection.sanitize(frag.Content)
		}

		result, err := embedder.Embed(ctx, embedding.EmbedRequest{
			Texts:    texts,
			Model:    modelName,
			TaskType: "RETRIEVAL_DOCUMENT",
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed embedding batch starting at %d (provider details withheld)\n", i)
			continue
		}
		for j, emb := range result.Embeddings {
			if len(emb.Vector) == 0 {
				continue
			}
			frag := batch[j]
			if err := store.UpsertEmbedding(ctx, frag.ID, providerName, modelName, len(emb.Vector), emb.Vector); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to persist embedding for fragment %s: %v\n", frag.ID, err)
				continue
			}
			embeddedCount++
		}
	}

	return embeddedCount, nil
}
