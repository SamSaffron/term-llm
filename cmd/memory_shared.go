package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/embedding"
	"github.com/samsaffron/term-llm/internal/session"
)

func openReadOnlySessionStore(cfg *config.Config) (session.Store, error) {
	storeCfg := sessionStoreConfig(cfg)
	if !storeCfg.Enabled {
		return nil, fmt.Errorf("session storage is disabled (check sessions.enabled and --no-session)")
	}
	storeCfg.ReadOnly = true
	return session.NewStore(storeCfg)
}

func listCompleteSessions(ctx context.Context, store session.Store) ([]session.SessionSummary, error) {
	const pageSize = 200
	offset := 0
	all := make([]session.SessionSummary, 0, pageSize)

	for {
		page, err := store.List(ctx, session.ListOptions{
			Status: session.StatusComplete,
			Limit:  pageSize,
			Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		all = append(all, page...)
		if len(page) < pageSize {
			break
		}
		offset += len(page)
	}

	return all, nil
}

func resolveMemoryAgent(sessionAgent string) string {
	if strings.TrimSpace(memoryAgent) != "" {
		return strings.TrimSpace(memoryAgent)
	}
	sessionAgent = strings.TrimSpace(sessionAgent)
	if sessionAgent != "" {
		return sessionAgent
	}
	return "default"
}

func hasMemoryMiningTag(tags string) bool {
	for _, tag := range parseTags(tags) {
		if tag == "memory-mining" {
			return true
		}
	}
	return false
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "UNIQUE constraint failed: memory_fragments.agent, memory_fragments.path")
}

func resolveMemoryEmbeddingProvider(cfg *config.Config, override string) (provider, model, providerSpec string) {
	raw := strings.TrimSpace(override)
	if raw == "" {
		raw = strings.TrimSpace(cfg.Embed.Provider)
	}
	if raw == "" {
		raw = strings.TrimSpace(embedding.InferEmbeddingProvider(cfg))
	}
	if raw == "" {
		return "", "", ""
	}

	provider, model = parseEmbeddingProviderModel(raw)
	if provider == "" {
		return "", "", ""
	}
	if model == "" {
		model = defaultEmbeddingModel(cfg, provider)
	}

	providerSpec = provider
	if model != "" {
		providerSpec = provider + ":" + model
	}
	return provider, model, providerSpec
}

func parseEmbeddingProviderModel(spec string) (provider, model string) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", ""
	}
	parts := strings.SplitN(spec, ":", 2)
	provider = strings.TrimSpace(parts[0])
	if len(parts) == 2 {
		model = strings.TrimSpace(parts[1])
	}
	return provider, model
}

func defaultEmbeddingModel(cfg *config.Config, provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "gemini":
		if strings.TrimSpace(cfg.Embed.Gemini.Model) != "" {
			return strings.TrimSpace(cfg.Embed.Gemini.Model)
		}
		return "gemini-embedding-001"
	case "openai":
		if strings.TrimSpace(cfg.Embed.OpenAI.Model) != "" {
			return strings.TrimSpace(cfg.Embed.OpenAI.Model)
		}
		return "text-embedding-3-small"
	case "jina":
		if strings.TrimSpace(cfg.Embed.Jina.Model) != "" {
			return strings.TrimSpace(cfg.Embed.Jina.Model)
		}
		return "jina-embeddings-v3"
	case "voyage":
		if strings.TrimSpace(cfg.Embed.Voyage.Model) != "" {
			return strings.TrimSpace(cfg.Embed.Voyage.Model)
		}
		return "voyage-3.5"
	case "ollama":
		if strings.TrimSpace(cfg.Embed.Ollama.Model) != "" {
			return strings.TrimSpace(cfg.Embed.Ollama.Model)
		}
		return "nomic-embed-text"
	default:
		return ""
	}
}
