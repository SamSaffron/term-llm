package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const (
	ModelCacheTTL = 6 * time.Hour
	cacheDir      = "term-llm"
)

type ModelCache struct {
	Models     []string      `json:"models"`
	ModelInfos []CachedModel `json:"model_infos,omitempty"`
	FetchedAt  time.Time     `json:"fetched_at"`
}

type CachedModel struct {
	ID                string           `json:"id"`
	DisplayName       string           `json:"display_name,omitempty"`
	Created           int64            `json:"created,omitempty"`
	OwnedBy           string           `json:"owned_by,omitempty"`
	InputLimit        int              `json:"input_limit,omitempty"`
	ConfiguredContext int              `json:"configured_context,omitempty"`
	OutputLimit       int              `json:"output_limit,omitempty"`
	InputPrice        float64          `json:"input_price,omitempty"`
	OutputPrice       float64          `json:"output_price,omitempty"`
	ReasoningEfforts  []string         `json:"reasoning_efforts,omitempty"`
	ReasoningBudgets  map[string]int64 `json:"reasoning_budgets,omitempty"`
	Protocol          string           `json:"protocol,omitempty"`
	Deprecated        bool             `json:"deprecated,omitempty"`
}

func getCacheDir() (string, error) {
	cacheHome := os.Getenv("XDG_CACHE_HOME")
	if cacheHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		cacheHome = filepath.Join(home, ".cache")
	}
	return filepath.Join(cacheHome, cacheDir), nil
}

func getCachePath(provider string) (string, error) {
	dir, err := getCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, provider+"-models.json"), nil
}

func ReadModelCache(provider string) (*ModelCache, error) {
	path, err := getCachePath(provider)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cache ModelCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, err
	}

	return &cache, nil
}

func WriteModelCache(provider string, models []string) error {
	return writeModelCache(provider, models, nil)
}

func WriteModelInfoCache(provider string, modelInfos []CachedModel) error {
	models := make([]string, 0, len(modelInfos))
	for _, m := range modelInfos {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return writeModelCache(provider, models, modelInfos)
}

func writeModelCache(provider string, models []string, modelInfos []CachedModel) error {
	dir, err := getCacheDir()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	path, err := getCachePath(provider)
	if err != nil {
		return err
	}

	cache := ModelCache{
		Models:     models,
		ModelInfos: modelInfos,
		FetchedAt:  time.Now(),
	}

	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}

	f, err := os.CreateTemp(dir, provider+"-models-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	renamed := false
	defer func() {
		if !renamed {
			os.Remove(tmpPath)
		}
	}()

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	if err := os.Chmod(tmpPath, 0644); err != nil {
		return err
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	renamed = true
	return nil
}

func IsCacheValid(cache *ModelCache) bool {
	if cache == nil {
		return false
	}
	return time.Since(cache.FetchedAt) < ModelCacheTTL
}
