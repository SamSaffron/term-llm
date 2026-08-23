package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/appdata"
	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/grokprotocol"
	"github.com/samsaffron/term-llm/internal/oauth"
)

const (
	grokModelsURL          = "https://cli-chat-proxy.grok.com/v1/models"
	grokModelsCacheFile    = "grok_models_cache.json"
	grokModelsCacheTTL     = 5 * time.Minute
	grokModelsTimeout      = 30 * time.Second
	maxGrokModelsBodyBytes = 1024 * 1024
	maxGrokModels          = 128
)

var (
	grok46ReasoningEfforts = []string{"low", "medium", "high", "xhigh"}
	grok45ReasoningEfforts = []string{"low", "medium", "high"}
)

type grokModelsResponse struct {
	Data   []grokModelRecord `json:"data"`
	Models []grokModelRecord `json:"models"`
}

type grokModelRecord struct {
	ID                      string `json:"id"`
	Model                   string `json:"model"`
	APIBackend              string `json:"api_backend"`
	Name                    string `json:"name"`
	ContextWindow           int    `json:"context_window"`
	MaxCompletionTokens     int    `json:"max_completion_tokens"`
	SupportsReasoningEffort bool   `json:"supports_reasoning_effort"`
	ReasoningEfforts        []struct {
		Value string `json:"value"`
	} `json:"reasoning_efforts"`
}

type grokModelsCache struct {
	AccountID     string      `json:"account_id"`
	Origin        string      `json:"origin"`
	ClientVersion string      `json:"client_version"`
	FetchedAt     time.Time   `json:"fetched_at"`
	Models        []ModelInfo `json:"models"`
}

func CachedGrokModels() ([]ModelInfo, bool, error) {
	creds, err := credentials.GetGrokCredentials()
	if err != nil {
		return nil, false, err
	}
	cache, err := loadGrokModelsCache(creds.AccountID)
	if err != nil {
		return nil, false, err
	}
	return cache.Models, time.Since(cache.FetchedAt) <= grokModelsCacheTTL, nil
}

func (p *GrokProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	models, _, err := p.ListModelsWithFreshness(ctx)
	return models, err
}

func (p *GrokProvider) ListModelsWithFreshness(ctx context.Context) ([]ModelInfo, bool, error) {
	if cache, err := loadGrokModelsCache(p.creds.AccountID); err == nil && time.Since(cache.FetchedAt) <= grokModelsCacheTTL {
		return cache.Models, true, nil
	}
	models, err := p.fetchGrokModels(ctx)
	if err == nil {
		_ = saveGrokModelsCache(grokModelsCache{AccountID: p.creds.AccountID, FetchedAt: time.Now(), Models: models})
		return models, true, nil
	}
	if cache, cacheErr := loadGrokModelsCache(p.creds.AccountID); cacheErr == nil && len(cache.Models) > 0 {
		return cache.Models, false, nil
	}
	return nil, false, err
}

func (p *GrokProvider) fetchGrokModels(ctx context.Context) ([]ModelInfo, error) {
	if p.creds.IsExpired() {
		if err := refreshGrokSession(ctx, p.creds, false); err != nil {
			return nil, fmt.Errorf("refresh Grok session: %w", err)
		}
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, grokModelsTimeout)
		defer cancel()
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, grokModelsURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.creds.AccessToken)
		req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
		req.Header.Set("x-userid", p.creds.AccountID)
		req.Header.Set("x-grok-client-version", grokProxyCompatibilityVersion)
		// Canonical defines this as a client_mode metrics label, not auth.
		// The provider lacks a reliable runtime interactive signal.
		req.Header.Set("x-grok-client-mode", grokprotocol.ClientModeHeadless)
		req.Header.Set("User-Agent", grokUserAgent)
		client := grokHTTPClient
		if client == nil {
			client = defaultHTTPClient
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("Grok models request failed: %w", err)
		}
		body, readErr := readBoundedGrokModelsBody(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			if err := refreshGrokSession(ctx, p.creds, true); err != nil {
				return nil, fmt.Errorf("refresh Grok session: %w", err)
			}
			continue
		}
		if resp.StatusCode == http.StatusForbidden {
			return nil, errors.New("Grok model catalog access was denied (403); verify subscription and CLI entitlement")
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, &RateLimitError{Message: "Grok model catalog rate limit exceeded", RetryAfter: grokRetryAfter(resp.Header.Get("Retry-After"))}
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("Grok models request failed: HTTP %d", resp.StatusCode)
		}
		return parseGrokModels(body)
	}
	return nil, errors.New("Grok models authentication failed after refresh")
}

func readBoundedGrokModelsBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxGrokModelsBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Grok model catalog: %w", err)
	}
	if len(data) > maxGrokModelsBodyBytes {
		return nil, errors.New("Grok model catalog exceeds 1 MiB limit")
	}
	return data, nil
}

func parseGrokModels(body []byte) ([]ModelInfo, error) {
	if len(body) > maxGrokModelsBodyBytes {
		return nil, errors.New("Grok model catalog exceeds 1 MiB limit")
	}
	var response grokModelsResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode Grok model catalog: %w", err)
	}
	records := response.Data
	if len(records) == 0 {
		records = response.Models
	}
	if len(records) > maxGrokModels {
		return nil, fmt.Errorf("Grok model catalog contains more than %d models", maxGrokModels)
	}
	models := make([]ModelInfo, 0, len(records))
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		if !strings.EqualFold(record.APIBackend, "responses") {
			continue
		}
		id := record.Model
		if id == "" {
			id = record.ID
		}
		if err := validateGrokModel(id); err != nil {
			return nil, fmt.Errorf("invalid Grok model catalog ID: %w", err)
		}
		if seen[id] {
			continue
		}
		if record.ContextWindow < 0 || record.MaxCompletionTokens < 0 || len(record.ReasoningEfforts) > 16 {
			return nil, errors.New("invalid Grok model capability metadata")
		}
		if record.Name != "" && !validGrokCatalogDisplayString(record.Name, 256) {
			return nil, errors.New("invalid Grok model display name")
		}
		seen[id] = true
		efforts, err := knownGrokReasoningEfforts(id, record)
		if err != nil {
			return nil, err
		}
		models = append(models, ModelInfo{
			ID:               id,
			DisplayName:      record.Name,
			InputLimit:       record.ContextWindow,
			OutputLimit:      record.MaxCompletionTokens,
			InputPrice:       -1,
			OutputPrice:      -1,
			ReasoningEfforts: efforts,
		})
	}
	if len(models) == 0 {
		return nil, errors.New("Grok model catalog contains no Responses-compatible models")
	}
	return models, nil
}

func knownGrokReasoningEfforts(id string, record grokModelRecord) ([]string, error) {
	var efforts []string
	for _, raw := range record.ReasoningEfforts {
		if !validGrokCatalogDisplayString(raw.Value, 32) {
			return nil, errors.New("invalid Grok model reasoning effort")
		}
		value := strings.ToLower(strings.TrimSpace(raw.Value))
		if !isKnownGrokReasoningEffort(value) {
			continue
		}
		if !containsGrokString(efforts, value) {
			efforts = append(efforts, value)
		}
	}
	if len(record.ReasoningEfforts) > 0 {
		return efforts, nil
	}
	if record.SupportsReasoningEffort {
		return staticGrokReasoningEfforts(id), nil
	}
	return nil, nil
}

func staticGrokReasoningEfforts(id string) []string {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "grok-4.6":
		return cloneEfforts(grok46ReasoningEfforts)
	case "grok-4.5":
		return cloneEfforts(grok45ReasoningEfforts)
	default:
		return nil
	}
}

func isKnownGrokReasoningEffort(value string) bool {
	switch value {
	case "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

func validGrokCatalogDisplayString(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || isGrokCatalogBidiControl(r) {
			return false
		}
	}
	return true
}

func isGrokCatalogBidiControl(r rune) bool {
	table := unicode.Properties["Bidi_Control"]
	return table != nil && unicode.Is(table, r)
}

func grokCachedModelInfo(model string) (ModelInfo, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return ModelInfo{}, false
	}
	models, _, err := CachedGrokModels()
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

func grokCachedInputLimit(model string) int {
	if info, ok := grokCachedModelInfo(model); ok {
		return info.InputLimit
	}
	return 0
}

func grokCachedOutputLimit(model string) int {
	if info, ok := grokCachedModelInfo(model); ok {
		return info.OutputLimit
	}
	return 0
}

func grokCachedReasoningEfforts(model string) ([]string, bool) {
	info, ok := grokCachedModelInfo(model)
	if !ok {
		return nil, false
	}
	return cloneEfforts(info.ReasoningEfforts), true
}

func clampGrokOutputTokens(requested int, model string) int {
	if requested <= 0 {
		return requested
	}
	if limit := grokCachedOutputLimit(model); limit > 0 && requested > limit {
		return limit
	}
	return ClampOutputTokens(requested, model)
}

func containsGrokString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func grokModelsCachePath() (string, error) {
	dataDir, err := appdata.GetDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, grokModelsCacheFile), nil
}

func loadGrokModelsCache(accountID string) (grokModelsCache, error) {
	if !oauth.ValidGrokAccountID(accountID) {
		return grokModelsCache{}, errors.New("cached Grok model catalog account is invalid")
	}
	path, err := grokModelsCachePath()
	if err != nil {
		return grokModelsCache{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return grokModelsCache{}, err
	}
	if !info.Mode().IsRegular() {
		return grokModelsCache{}, errors.New("cached Grok model catalog is not a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return grokModelsCache{}, fmt.Errorf("cached Grok model catalog has insecure permissions %04o; require 0600", info.Mode().Perm())
	}
	if info.Size() > maxGrokModelsBodyBytes {
		return grokModelsCache{}, errors.New("cached Grok model catalog is too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return grokModelsCache{}, err
	}
	if len(data) > maxGrokModelsBodyBytes {
		return grokModelsCache{}, errors.New("cached Grok model catalog is too large")
	}
	var cache grokModelsCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return grokModelsCache{}, err
	}
	if cache.AccountID != accountID || cache.Origin != grokModelsURL || cache.ClientVersion != grokProxyCompatibilityVersion || len(cache.Models) == 0 || len(cache.Models) > maxGrokModels {
		return grokModelsCache{}, errors.New("cached Grok model catalog is invalid")
	}
	for i := range cache.Models {
		model := &cache.Models[i]
		if err := validateGrokModel(model.ID); err != nil || model.InputLimit < 0 || model.OutputLimit < 0 {
			return grokModelsCache{}, errors.New("cached Grok model catalog is invalid")
		}
		if model.DisplayName != "" && !validGrokCatalogDisplayString(model.DisplayName, 256) {
			return grokModelsCache{}, errors.New("cached Grok model catalog is invalid")
		}
		filtered := model.ReasoningEfforts[:0]
		for _, effort := range model.ReasoningEfforts {
			if !validGrokCatalogDisplayString(effort, 32) {
				return grokModelsCache{}, errors.New("cached Grok model catalog is invalid")
			}
			effort = strings.ToLower(strings.TrimSpace(effort))
			if !isKnownGrokReasoningEffort(effort) {
				continue
			}
			if !containsGrokString(filtered, effort) {
				filtered = append(filtered, effort)
			}
		}
		model.ReasoningEfforts = filtered
	}
	return cache, nil
}

func saveGrokModelsCache(cache grokModelsCache) error {
	if !oauth.ValidGrokAccountID(cache.AccountID) {
		return errors.New("invalid Grok model cache account")
	}
	// Cache entries are endpoint- and wire-version-specific because catalog
	// metadata must never cross subscription proxy origins or protocol versions.
	cache.Origin = grokModelsURL
	cache.ClientVersion = grokProxyCompatibilityVersion
	path, err := grokModelsCachePath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".grok-models-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := file.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	renamed = true
	return nil
}

func clearGrokModelsCache() error {
	path, err := grokModelsCachePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
