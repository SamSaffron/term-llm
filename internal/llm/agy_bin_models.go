package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/cache"
)

const agyBinModelCacheKey = "agy-bin"

// RefreshAgyBinModelsIfStale refreshes the on-disk model catalog when it is
// missing or older than the shared model-cache TTL. A failed refresh leaves any
// stale cache available as a completion fallback.
func RefreshAgyBinModelsIfStale(ctx context.Context, model string, env map[string]string) error {
	cached, err := cache.ReadModelCache(agyBinModelCacheKey)
	if err == nil && cache.IsCacheValid(cached) && (len(cached.ModelInfos) > 0 || len(cached.Models) > 0) {
		return nil
	}
	_, err = NewAgyBinProvider(model, env).ListModels(ctx)
	return err
}

// GetCachedAgyBinModels returns the last live model list fetched from the agy
// CLI. It never spawns agy, so it is safe for shell completion and provider
// pickers. Run `term-llm models --provider agy-bin` to refresh it.
func GetCachedAgyBinModels() []string {
	models := GetCachedAgyBinModelInfos()
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if model.ID != "" {
			ids = append(ids, model.ID)
		}
	}
	return ids
}

// GetCachedAgyBinModelInfos returns cached live agy CLI model metadata. Stale
// entries remain available because they are preferable to the curated fallback
// and keep non-subprocess callers fast.
func GetCachedAgyBinModelInfos() []ModelInfo {
	cached, err := cache.ReadModelCache(agyBinModelCacheKey)
	if err != nil || cached == nil || (len(cached.ModelInfos) == 0 && len(cached.Models) == 0) {
		return nil
	}
	return modelInfosFromCache(cached)
}

// RefreshAgyBinCacheSync stores a freshly fetched agy CLI model list for
// completions and offline provider/model pickers.
func RefreshAgyBinCacheSync(models []ModelInfo) {
	if len(models) == 0 {
		return
	}
	_ = cache.WriteModelInfoCache(agyBinModelCacheKey, modelInfosToCache(models))
}

func defaultAgyModelsCommandOutput(ctx context.Context, p *AgyBinProvider) ([]byte, error) {
	cmd, err := newCLICommand(ctx, "agy", []string{"models"}, "")
	if err != nil {
		return nil, err
	}
	cmd.Env = p.extraEnvList()
	auditedEnv, wireAudit, err := startCLIWireAudit("agy-bin-models", cmd.Env)
	if err != nil {
		return nil, err
	}
	cmd.Env = auditedEnv
	defer stopCLIWireAudit(wireAudit)
	return cmd.Output()
}

var agyModelsCommandOutput = defaultAgyModelsCommandOutput

// ListModels parses the account-specific tab-separated model catalog exposed by
// `agy models` and saves it in term-llm's shared model cache.
func (p *AgyBinProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	out, err := agyModelsCommandOutput(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("list agy models: %w", err)
	}
	models := parseAgyModelsOutput(string(out))
	if len(models) == 0 {
		return nil, fmt.Errorf("list agy models: no models found")
	}
	RefreshAgyBinCacheSync(models)
	return models, nil
}

func parseAgyModelsOutput(output string) []ModelInfo {
	models := make([]ModelInfo, 0)
	seen := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 || seen[fields[0]] {
			continue
		}
		seen[fields[0]] = true
		model := ModelInfo{ID: fields[0]}
		if len(fields) > 1 {
			model.DisplayName = strings.Join(fields[1:], " ")
		}
		models = append(models, model)
	}
	return models
}
