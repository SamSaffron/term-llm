package llm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/cache"
)

const grokBinModelCacheKey = "grok-bin"

// RefreshGrokBinModelsIfStale refreshes the on-disk model catalog when it is
// missing or older than the shared model-cache TTL. A failed refresh leaves any
// stale cache available as a completion fallback.
func RefreshGrokBinModelsIfStale(ctx context.Context, model string, env map[string]string) error {
	cached, err := cache.ReadModelCache(grokBinModelCacheKey)
	if err == nil && cache.IsCacheValid(cached) && (len(cached.ModelInfos) > 0 || len(cached.Models) > 0) {
		return nil
	}
	_, err = NewGrokBinProvider(model, env).ListModels(ctx)
	return err
}

// GetCachedGrokBinModels returns the last live model list fetched from the
// Grok Build CLI. It never spawns grok, so it is safe for shell completion
// and provider pickers. Run `term-llm models --provider grok-bin` to refresh it.
func GetCachedGrokBinModels() []string {
	models := GetCachedGrokBinModelInfos()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// GetCachedGrokBinModelInfos returns cached live Grok CLI model metadata.
// Stale cache entries are still returned because they are preferable to the
// small curated fallback and keep non-subprocess callers fast.
func GetCachedGrokBinModelInfos() []ModelInfo {
	cached, err := cache.ReadModelCache(grokBinModelCacheKey)
	if err != nil || cached == nil || (len(cached.ModelInfos) == 0 && len(cached.Models) == 0) {
		return nil
	}
	return modelInfosFromCache(cached)
}

// RefreshGrokBinCacheSync stores a freshly fetched Grok CLI model list for
// completions and offline provider/model pickers. Base grok-4* IDs are expanded
// with term-llm's effort-suffix aliases so pickers stay consistent with the
// curated catalog.
func RefreshGrokBinCacheSync(models []ModelInfo) {
	modelInfos := expandGrokBinListedModels(models)
	if len(modelInfos) == 0 {
		return
	}
	_ = cache.WriteModelInfoCache(grokBinModelCacheKey, modelInfosToCache(modelInfos))
}

func expandGrokBinListedModels(models []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(models)*(1+len(grokBinEffortVariants)))
	seen := make(map[string]bool, len(models)*2)
	add := func(m ModelInfo) {
		m.ID = strings.TrimSpace(m.ID)
		if m.ID == "" || seen[m.ID] {
			return
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	for _, m := range models {
		add(m)
		base, hasSuffix := trimKnownEffortSuffix(m.ID)
		if hasSuffix || !strings.HasPrefix(strings.ToLower(base), "grok-4") {
			continue
		}
		for _, effort := range grokBinEffortVariants {
			variant := m
			variant.ID = base + "-" + effort
			variant.DisplayName = ""
			add(variant)
		}
	}
	return out
}

func defaultGrokModelsCommandOutput(ctx context.Context, p *GrokBinProvider, home string) ([]byte, error) {
	cmd, err := newCLICommand(ctx, "grok", []string{"models"}, filepath.Join(home, "cwd"))
	if err != nil {
		return nil, err
	}
	cmd.Env = p.buildCommandEnvForHome(home)
	return cmd.Output()
}

var grokModelsCommandOutput = defaultGrokModelsCommandOutput

// ListModels parses the account-specific model list exposed by `grok models`.
// Grok fetches this from the remote /v1/models catalog (with a baked-in
// default_models.json fallback), which is how the CLI itself stays current.
func (p *GrokBinProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	home, err := os.MkdirTemp("", "term-llm-grok-models-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	if err := ensureGrokHomeLayout(home); err != nil {
		return nil, err
	}
	output, err := grokModelsCommandOutput(ctx, p, home)
	if err != nil {
		return nil, fmt.Errorf("list Grok models: %w", err)
	}
	models := parseGrokModelsOutput(string(output))
	if len(models) == 0 {
		return nil, fmt.Errorf("list Grok models: no models found")
	}
	RefreshGrokBinCacheSync(models)
	return models, nil
}

func parseGrokModelsOutput(output string) []ModelInfo {
	var models []ModelInfo
	seen := make(map[string]bool)
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		models = append(models, ModelInfo{ID: id, OwnedBy: "grok", InputPrice: -1, OutputPrice: -1})
	}
	inList := false
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if id, ok := strings.CutPrefix(trimmed, "Default model:"); ok {
			add(id)
			continue
		}
		if strings.EqualFold(trimmed, "Available models:") {
			inList = true
			continue
		}
		if !inList {
			continue
		}
		if id, ok := parseGrokListedModelLine(trimmed); ok {
			add(id)
		}
	}
	return models
}

func parseGrokListedModelLine(line string) (string, bool) {
	line = strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(line, "* "):
		line = strings.TrimSpace(line[2:])
	case strings.HasPrefix(line, "- "):
		line = strings.TrimSpace(line[2:])
	default:
		return "", false
	}
	line = strings.TrimSpace(strings.TrimSuffix(line, "(default)"))
	if id, _, found := strings.Cut(line, " - "); found {
		line = strings.TrimSpace(id)
	}
	if line == "" || strings.ContainsAny(line, " \t") {
		return "", false
	}
	return line, true
}
