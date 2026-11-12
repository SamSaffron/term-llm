package memory

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/embedding"
	"github.com/samsaffron/term-llm/internal/session"
)

func openReadOnlySessionStore(env Env, cfg *config.Config) (session.Store, error) {
	storeCfg := env.SessionStoreConfig(cfg)
	if !storeCfg.Enabled {
		return nil, fmt.Errorf("session storage is disabled (check sessions.enabled and --no-session)")
	}
	storeCfg.ReadOnly = true
	return session.NewStore(storeCfg)
}

// memorySourceStatuses lists the session statuses memory reads from. Web and
// chat-platform conversations stay active between turns and never complete, so
// excluding active sessions would hide them from memory entirely. Readers resume
// from a stored message offset, so new turns in an active session are picked up
// later without rereading earlier messages.
var memorySourceStatuses = []session.SessionStatus{session.StatusComplete, session.StatusActive}

// activeSessionQuietPeriod is how long an active session must go without
// updates before memory reads it. While a turn streams, serve stores the
// assistant reply as one row and rewrites it in place until the response
// finishes; each rewrite bumps the session's updated_at. Reading that row early
// would advance the stored offset past it and lose the finished reply, so
// readers wait until the session has been quiet for this long.
const activeSessionQuietPeriod = 10 * time.Minute

// activeSessionMayBeWriting reports whether summary is an active session
// updated too recently to rule out an in-progress turn.
func activeSessionMayBeWriting(summary session.SessionSummary, now time.Time) bool {
	return summary.Status == session.StatusActive && summary.UpdatedAt.After(now.Add(-activeSessionQuietPeriod))
}

func isMemorySourceStatus(status session.SessionStatus) bool {
	return slices.Contains(memorySourceStatuses, status)
}

// listMemorySourceSessions returns sessions memory may read from, newest
// session number first. An empty agent lists sessions for every agent.
func listMemorySourceSessions(ctx context.Context, store session.Store, agent string) ([]session.SessionSummary, error) {
	const pageSize = 200
	all := make([]session.SessionSummary, 0, pageSize)
	seen := map[string]struct{}{}

	for _, status := range memorySourceStatuses {
		var beforeNumber int64
		for {
			page, err := store.List(ctx, session.ListOptions{
				Agent:  agent,
				Status: status,
				// Locked conversations never feed memory mining or insights.
				ExcludeScopeLocked: true,
				Limit:              pageSize,
				BeforeNumber:       beforeNumber,
				SortByNumberDesc:   true,
			})
			if err != nil {
				return nil, err
			}
			for _, summary := range page {
				// Custom stores may ignore the status filter.
				if !isMemorySourceStatus(summary.Status) {
					continue
				}
				if _, dup := seen[summary.ID]; dup {
					continue
				}
				seen[summary.ID] = struct{}{}
				all = append(all, summary)
			}
			if len(page) < pageSize {
				break
			}
			lastNumber := page[len(page)-1].Number
			if lastNumber <= 0 {
				return nil, fmt.Errorf("%s session %s is missing a session number", status, page[len(page)-1].ID)
			}
			beforeNumber = lastNumber
		}
	}

	sort.SliceStable(all, func(i, j int) bool {
		return all[i].Number > all[j].Number
	})
	return all, nil
}

// ResolveAgent returns the --agent filter, or the session agent (or "default").
func (e Env) ResolveAgent(sessionAgent string) string {
	if e.AgentFilter() != "" {
		return e.AgentFilter()
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

func hasMemoryMiningTag(env Env, tags string) bool {
	for _, tag := range env.ParseTags(tags) {
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
