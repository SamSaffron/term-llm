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
	var beforeNumber int64
	all := make([]session.SessionSummary, 0, pageSize)

	for {
		page, err := store.List(ctx, session.ListOptions{
			Status:           session.StatusComplete,
			Limit:            pageSize,
			BeforeNumber:     beforeNumber,
			SortByNumberDesc: true,
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
		lastNumber := page[len(page)-1].Number
		if lastNumber <= 0 {
			return nil, fmt.Errorf("complete session %s is missing a session number", page[len(page)-1].ID)
		}
		beforeNumber = lastNumber
	}

	return all, nil
}

func resolveMemoryAgent(sessionAgent string) string {
	if strings.TrimSpace(memoryAgent) != "" {
		return strings.TrimSpace(memoryAgent)
	}
	return sessionAgentOrDefault(sessionAgent)
}

func sessionAgentOrDefault(agent string) string {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return "default"
	}
	return agent
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
		return config.DefaultEmbedGeminiModel
	case "openai":
		if strings.TrimSpace(cfg.Embed.OpenAI.Model) != "" {
			return strings.TrimSpace(cfg.Embed.OpenAI.Model)
		}
		return config.DefaultEmbedOpenAIModel
	case "jina":
		if strings.TrimSpace(cfg.Embed.Jina.Model) != "" {
			return strings.TrimSpace(cfg.Embed.Jina.Model)
		}
		return config.DefaultEmbedJinaModel
	case "voyage":
		if strings.TrimSpace(cfg.Embed.Voyage.Model) != "" {
			return strings.TrimSpace(cfg.Embed.Voyage.Model)
		}
		return config.DefaultEmbedVoyageModel
	case "ollama":
		if strings.TrimSpace(cfg.Embed.Ollama.Model) != "" {
			return strings.TrimSpace(cfg.Embed.Ollama.Model)
		}
		return config.DefaultEmbedOllamaModel
	default:
		return ""
	}
}
