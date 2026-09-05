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
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/appdata"
	"github.com/samsaffron/term-llm/internal/buildinfo"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"
)

const (
	chatGPTModelsBaseURL = "https://chatgpt.com/backend-api/codex/models"
	// Identify this client honestly on discovery, inference, and usage requests.
	// Do not send a `version` header: the backend interprets it as a Codex
	// application version, not a term-llm version.
	chatGPTOriginator = "term-llm"
	// /codex/models requires this semver query parameter to select the catalog
	// protocol. It is not our application identity and is never sent as a header.
	chatGPTModelsClientVersion = "0.153.3"
	chatGPTModelsCacheFile     = "chatgpt_models_cache_v2.json"
	chatGPTModelsCacheTTL      = 5 * time.Minute
	chatGPTModelsTimeout       = 5 * time.Second
)

type chatGPTModelsResponse struct {
	Models []chatGPTModelInfo `json:"models"`
}

type chatGPTReasoningLevels []string

func (l *chatGPTReasoningLevels) UnmarshalJSON(data []byte) error {
	var values []json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	seen := make(map[string]bool, len(values))
	levels := make([]string, 0, len(values))
	for _, value := range values {
		var level string
		if err := json.Unmarshal(value, &level); err != nil {
			var item struct {
				Effort string `json:"effort"`
				Level  string `json:"level"`
				Value  string `json:"value"`
				ID     string `json:"id"`
				Name   string `json:"name"`
			}
			if err := json.Unmarshal(value, &item); err != nil {
				return err
			}
			level = firstNonEmpty(item.Effort, item.Level, item.Value, item.ID, item.Name)
		}
		level = strings.ToLower(strings.TrimSpace(level))
		if level == "" || seen[level] {
			continue
		}
		seen[level] = true
		levels = append(levels, level)
	}
	*l = levels
	return nil
}

type chatGPTModelInfo struct {
	Slug                     string                 `json:"slug"`
	ID                       string                 `json:"id"`
	Name                     string                 `json:"name"`
	Title                    string                 `json:"title"`
	DisplayName              string                 `json:"display_name"`
	MaxInputTokens           int                    `json:"max_input_tokens"`
	InputTokenLimit          int                    `json:"input_token_limit"`
	ContextWindow            int                    `json:"context_window"`
	MaxContextWindow         int                    `json:"max_context_window"`
	ServiceTiers             []ModelServiceTier     `json:"service_tiers"`
	AdditionalSpeedTiers     []string               `json:"additional_speed_tiers"`
	SupportedReasoningLevels chatGPTReasoningLevels `json:"supported_reasoning_levels"`
	DefaultReasoningLevel    string                 `json:"default_reasoning_level"`
	DefaultReasoningEffort   string                 `json:"default_reasoning_effort"`
}

type chatGPTModelsCache struct {
	AccountID     string      `json:"account_id,omitempty"`
	FetchedAt     time.Time   `json:"fetched_at"`
	ETag          string      `json:"etag,omitempty"`
	ClientVersion string      `json:"client_version,omitempty"`
	Models        []ModelInfo `json:"models"`
}

// CachedChatGPTModels returns cached ChatGPT model metadata, if present. Fresh is
// false when the cache is stale but still usable as a network-failure fallback.
func CachedChatGPTModels() (models []ModelInfo, fresh bool, err error) {
	models, fresh, err = cachedChatGPTModelFacts()
	if err == nil {
		models = resolveChatGPTModels("chatgpt", models)
	}
	return
}

func cachedChatGPTModelFacts() (models []ModelInfo, fresh bool, err error) {
	accountID := ""
	if creds, credErr := credentials.GetChatGPTCredentials(); credErr == nil {
		accountID = creds.AccountID
	}
	cache, err := loadChatGPTModelsCacheForAccount(chatGPTModelsClientVersion, accountID)
	if err != nil {
		return nil, false, err
	}
	return cache.Models, time.Since(cache.FetchedAt) <= chatGPTModelsCacheTTL, nil
}

// GetCachedChatGPTModels returns model IDs from the last authenticated catalog.
// It performs no network requests; callers can use the curated catalog when nil.
func GetCachedChatGPTModels() []string {
	models, _, err := CachedChatGPTModels()
	if err != nil || len(models) == 0 {
		return nil
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

// ListModels returns ChatGPT Codex backend model metadata, including service tiers.
func (p *ChatGPTProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	models, _, err := p.ListModelsWithFreshness(ctx)
	return models, err
}

// ListModelsWithFreshness returns model metadata and whether it came from a fresh
// cache or successful network fetch. Failed or empty refreshes use an account-
// scoped stale cache, then the curated static catalog, with fresh=false.
func (p *ChatGPTProvider) ListModelsWithFreshness(ctx context.Context) ([]ModelInfo, bool, error) {
	models, fresh, err := p.chatGPTModelFacts(ctx)
	return resolveChatGPTModels("chatgpt", models), fresh, err
}

// ListModelsForProvider applies the context policy for a configured provider key.
func (p *ChatGPTProvider) ListModelsForProvider(ctx context.Context, provider string) ([]ModelInfo, error) {
	models, _, err := p.chatGPTModelFacts(ctx)
	return resolveChatGPTModels(provider, models), err
}

func (p *ChatGPTProvider) chatGPTModelFacts(ctx context.Context) ([]ModelInfo, bool, error) {
	if cache, err := loadChatGPTModelsCacheForAccount(chatGPTModelsClientVersion, p.creds.AccountID); err == nil && time.Since(cache.FetchedAt) <= chatGPTModelsCacheTTL {
		return cache.Models, true, nil
	}

	models, etag, err := p.fetchChatGPTModels(ctx)
	if err == nil && len(models) > 0 {
		_ = saveChatGPTModelsCache(chatGPTModelsCache{
			AccountID:     p.creds.AccountID,
			FetchedAt:     time.Now(),
			ETag:          etag,
			ClientVersion: chatGPTModelsClientVersion,
			Models:        models,
		})
		return models, true, nil
	}
	if cache, cacheErr := loadChatGPTModelsCacheForAccount(chatGPTModelsClientVersion, p.creds.AccountID); cacheErr == nil && len(cache.Models) > 0 {
		return cache.Models, false, nil
	}
	return staticChatGPTModelInfos(), false, nil
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
	req.Header.Set("originator", chatGPTOriginator)
	req.Header.Set("User-Agent", chatGPTUserAgent())

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
			msg := fmt.Sprintf("ChatGPT models request failed: %s: %s", resp.Status, string(body))
			return nil, "", newHTTPStatusErrorMessage(msg, resp, body)
		}
		msg := fmt.Sprintf("ChatGPT models request failed: %s", resp.Status)
		return nil, "", newHTTPStatusErrorMessage(msg, resp, nil)
	}
	var decoded chatGPTModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, "", err
	}
	models := make([]ModelInfo, 0, len(decoded.Models))
	for _, raw := range decoded.Models {
		// Codex retains the complete remote catalog, including records that are
		// hidden from its picker but remain valid for explicit model selection.
		// term-llm has no separate show-in-picker bit, so keep those IDs available.
		if model := raw.toModelInfo(); model.ID != "" {
			models = append(models, model)
		}
	}
	return models, resp.Header.Get("ETag"), nil
}

func (m chatGPTModelInfo) toModelInfo() ModelInfo {
	id := firstNonEmpty(m.Slug, m.ID)
	// Legacy responses may provide an explicit input budget. Otherwise match
	// Codex's resolved_context_window(): active context_window wins and
	// max_context_window is only the ceiling for explicit overrides.
	inputLimit := firstNonZero(m.MaxInputTokens, m.InputTokenLimit, m.ContextWindow, m.MaxContextWindow)
	fallback, hasFallback := staticChatGPTModelInfo(id)
	outputLimit := 0
	if hasFallback {
		outputLimit = fallback.OutputLimit
	}
	reasoningEfforts := chatGPTWireReasoningEfforts(m.SupportedReasoningLevels)
	if len(reasoningEfforts) == 0 && hasFallback {
		reasoningEfforts = append([]string(nil), fallback.ReasoningEfforts...)
	}
	return ModelInfo{
		ID:                     id,
		DisplayName:            firstNonEmpty(m.DisplayName, m.Title, m.Name),
		InputLimit:             inputLimit,
		BackendContext:         firstNonZero(m.ContextWindow, m.MaxContextWindow),
		MaxContext:             firstNonZero(m.MaxInputTokens, m.InputTokenLimit, m.MaxContextWindow),
		OutputLimit:            outputLimit,
		InputPrice:             -1,
		OutputPrice:            -1,
		ServiceTiers:           m.ServiceTiers,
		AdditionalSpeedTiers:   m.AdditionalSpeedTiers,
		ReasoningEfforts:       reasoningEfforts,
		DefaultReasoningEffort: chatGPTWireReasoningEffort(firstNonEmpty(m.DefaultReasoningEffort, m.DefaultReasoningLevel)),
	}
}

// Ultra is a Codex product mode that combines max effort with subagents. It is
// not an inference API effort, so expose the max wire value to term-llm's
// effort-only selectors instead.
func chatGPTWireReasoningEffort(effort string) string {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "ultra" {
		return "max"
	}
	return effort
}

func chatGPTWireReasoningEfforts(efforts []string) []string {
	seen := make(map[string]bool, len(efforts))
	out := make([]string, 0, len(efforts))
	for _, effort := range efforts {
		effort = chatGPTWireReasoningEffort(effort)
		if effort == "" || seen[effort] {
			continue
		}
		seen[effort] = true
		out = append(out, effort)
	}
	return out
}

func staticChatGPTModelInfos() []ModelInfo {
	entries := ProviderModels[string(config.ProviderTypeChatGPT)]
	models := make([]ModelInfo, 0, len(entries))
	for _, entry := range entries {
		models = append(models, ModelInfo{
			ID:               entry.ID,
			InputLimit:       entry.InputLimit,
			OutputLimit:      entry.OutputLimit,
			InputPrice:       -1,
			OutputPrice:      -1,
			ReasoningEfforts: append([]string(nil), entry.ReasoningEfforts...),
		})
	}
	return models
}

func staticChatGPTModelInfo(id string) (ModelInfo, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, model := range staticChatGPTModelInfos() {
		if strings.ToLower(model.ID) == id {
			return model, true
		}
	}
	return ModelInfo{}, false
}

func chatGPTCachedModelInfo(model string) (ModelInfo, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return ModelInfo{}, false
	}
	models, _, err := cachedChatGPTModelFacts()
	if err != nil {
		return ModelInfo{}, false
	}
	lookup := func(id string) (ModelInfo, bool) {
		for _, info := range models {
			if strings.ToLower(strings.TrimSpace(info.ID)) == id {
				return info, true
			}
		}
		return ModelInfo{}, false
	}
	if info, ok := lookup(model); ok {
		return info, true
	}
	if base, ok := trimKnownEffortSuffix(model); ok {
		return lookup(base)
	}
	return ModelInfo{}, false
}

// effectiveInputLimit refreshes account-specific model metadata before context
// management is configured. A failed refresh still resolves through the stale
// cache or curated fallback used by ListModelsWithFreshness.
func (p *ChatGPTProvider) effectiveInputLimit(ctx context.Context, model string) (int, error) {
	return p.effectiveInputLimitForProvider(ctx, "chatgpt", model)
}

func (p *ChatGPTProvider) effectiveInputLimitForProvider(ctx context.Context, provider, model string) (int, error) {
	models, _, err := p.chatGPTModelFacts(ctx)
	if err != nil {
		return 0, err
	}
	model = chatGPTContextModelID(provider, model)
	base, _ := trimKnownEffortSuffix(model)
	for _, id := range []string{model, base} {
		for _, info := range models {
			if strings.EqualFold(info.ID, id) {
				return resolveChatGPTContext(provider, info).InputLimit, nil
			}
		}
	}
	return resolveChatGPTContext(provider, ModelInfo{ID: model}).InputLimit, nil
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
	return loadChatGPTModelsCacheForAccount(expectedVersion, "")
}

func loadChatGPTModelsCacheForAccount(expectedVersion, expectedAccountID string) (chatGPTModelsCache, error) {
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
	if expectedVersion != "" && cache.ClientVersion != expectedVersion {
		return chatGPTModelsCache{}, fmt.Errorf("cached ChatGPT model metadata is for client version %q", cache.ClientVersion)
	}
	if expectedAccountID != "" && cache.AccountID != expectedAccountID {
		return chatGPTModelsCache{}, fmt.Errorf("cached ChatGPT model metadata is for a different account")
	}
	if len(cache.Models) == 0 {
		return chatGPTModelsCache{}, fmt.Errorf("cached ChatGPT model metadata is empty")
	}
	for i := range cache.Models {
		cache.Models[i].ReasoningEfforts = chatGPTWireReasoningEfforts(cache.Models[i].ReasoningEfforts)
		cache.Models[i].DefaultReasoningEffort = chatGPTWireReasoningEffort(cache.Models[i].DefaultReasoningEffort)
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
	tmp, err := os.CreateTemp(filepath.Dir(path), ".chatgpt-models-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func chatGPTUserAgent() string { return buildinfo.UserAgent() }
