package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/samsaffron/term-llm/internal/appdata"
	"github.com/samsaffron/term-llm/internal/credentials"
)

const (
	chatGPTModelsBaseURL = "https://chatgpt.com/backend-api/codex/models"
	// The ChatGPT /codex/models endpoint validates client_version as semver.
	// Codex sends its Cargo package version; term-llm builds can report "dev", so
	// use a stable semver-shaped value here instead of the CLI version string.
	chatGPTModelsClientVersion = "0.0.0"
	chatGPTModelsCacheFile     = "chatgpt_models_cache.json"
	chatGPTModelsCacheTTL      = 5 * time.Minute
	chatGPTModelsTimeout       = 5 * time.Second
)

type chatGPTModelsResponse struct {
	Models []chatGPTModelInfo `json:"models"`
}

type chatGPTModelInfo struct {
	Slug                 string             `json:"slug"`
	ID                   string             `json:"id"`
	Name                 string             `json:"name"`
	Title                string             `json:"title"`
	DisplayName          string             `json:"display_name"`
	MaxInputTokens       int                `json:"max_input_tokens"`
	InputTokenLimit      int                `json:"input_token_limit"`
	ContextWindow        int                `json:"context_window"`
	ServiceTiers         []ModelServiceTier `json:"service_tiers"`
	AdditionalSpeedTiers []string           `json:"additional_speed_tiers"`
}

type chatGPTModelsCache struct {
	FetchedAt     time.Time   `json:"fetched_at"`
	ETag          string      `json:"etag,omitempty"`
	ClientVersion string      `json:"client_version,omitempty"`
	Models        []ModelInfo `json:"models"`
}

// CachedChatGPTModels returns cached ChatGPT model metadata, if present. Fresh is
// false when the cache is stale but still usable as a network-failure fallback.
func CachedChatGPTModels() (models []ModelInfo, fresh bool, err error) {
	cache, err := loadChatGPTModelsCache(chatGPTModelsClientVersion)
	if err != nil {
		return nil, false, err
	}
	return cache.Models, time.Since(cache.FetchedAt) <= chatGPTModelsCacheTTL, nil
}

// ListModels returns ChatGPT Codex backend model metadata, including service tiers.
func (p *ChatGPTProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	models, _, err := p.ListModelsWithFreshness(ctx)
	return models, err
}

// ListModelsWithFreshness returns model metadata and whether it came from a fresh
// cache or successful network fetch. If a network fetch fails and stale cache is
// available, it returns the stale models with fresh=false and nil error.
func (p *ChatGPTProvider) ListModelsWithFreshness(ctx context.Context) ([]ModelInfo, bool, error) {
	if cache, err := loadChatGPTModelsCache(chatGPTModelsClientVersion); err == nil && time.Since(cache.FetchedAt) <= chatGPTModelsCacheTTL {
		return cache.Models, true, nil
	}

	models, etag, err := p.fetchChatGPTModels(ctx)
	if err == nil {
		_ = saveChatGPTModelsCache(chatGPTModelsCache{
			FetchedAt:     time.Now(),
			ETag:          etag,
			ClientVersion: chatGPTModelsClientVersion,
			Models:        models,
		})
		return models, true, nil
	}

	if cache, cacheErr := loadChatGPTModelsCache(chatGPTModelsClientVersion); cacheErr == nil && len(cache.Models) > 0 {
		return cache.Models, false, nil
	}
	return nil, false, err
}

func (p *ChatGPTProvider) fetchChatGPTModels(ctx context.Context) ([]ModelInfo, string, error) {
	if p.creds.IsExpired() {
		if err := credentials.RefreshChatGPTCredentials(p.creds); err != nil {
			return nil, "", fmt.Errorf("token refresh failed: %w", err)
		}
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, chatGPTModelsTimeout)
		defer cancel()
	}

	u, err := url.Parse(chatGPTModelsBaseURL)
	if err != nil {
		return nil, "", err
	}
	q := u.Query()
	q.Set("client_version", chatGPTModelsClientVersion)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+p.creds.AccessToken)
	if p.creds.AccountID != "" {
		req.Header.Set("ChatGPT-Account-ID", p.creds.AccountID)
	}
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("originator", "term-llm")

	client := chatGPTHTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if len(body) > 0 {
			return nil, "", fmt.Errorf("ChatGPT models request failed: %s: %s", resp.Status, string(body))
		}
		return nil, "", fmt.Errorf("ChatGPT models request failed: %s", resp.Status)
	}
	var decoded chatGPTModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, "", err
	}
	models := make([]ModelInfo, 0, len(decoded.Models))
	for _, raw := range decoded.Models {
		models = append(models, raw.toModelInfo())
	}
	return models, resp.Header.Get("ETag"), nil
}

func (m chatGPTModelInfo) toModelInfo() ModelInfo {
	id := firstNonEmpty(m.Slug, m.ID)
	inputLimit := firstNonZero(m.MaxInputTokens, m.InputTokenLimit, m.ContextWindow)
	if inputLimit == 0 {
		inputLimit = InputLimitForModel(id)
	}
	return ModelInfo{
		ID:                   id,
		DisplayName:          firstNonEmpty(m.DisplayName, m.Title, m.Name),
		InputLimit:           inputLimit,
		InputPrice:           -1,
		OutputPrice:          -1,
		ServiceTiers:         m.ServiceTiers,
		AdditionalSpeedTiers: m.AdditionalSpeedTiers,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func chatGPTModelsCachePath() (string, error) {
	dataDir, err := appdata.GetDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, chatGPTModelsCacheFile), nil
}

func loadChatGPTModelsCache(expectedVersion string) (chatGPTModelsCache, error) {
	path, err := chatGPTModelsCachePath()
	if err != nil {
		return chatGPTModelsCache{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return chatGPTModelsCache{}, err
	}
	var cache chatGPTModelsCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return chatGPTModelsCache{}, err
	}
	if expectedVersion != "" && cache.ClientVersion != "" && cache.ClientVersion != expectedVersion {
		return chatGPTModelsCache{}, fmt.Errorf("cached ChatGPT model metadata is for client version %q", cache.ClientVersion)
	}
	if len(cache.Models) == 0 {
		return chatGPTModelsCache{}, fmt.Errorf("cached ChatGPT model metadata is empty")
	}
	return cache, nil
}

func saveChatGPTModelsCache(cache chatGPTModelsCache) error {
	path, err := chatGPTModelsCachePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
