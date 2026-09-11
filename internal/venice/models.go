// Package venice shares Venice model discovery between completion and image requests.
package venice

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/samsaffron/term-llm/internal/cache"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

const (
	BaseURL       = "https://api.venice.ai/api/v1"
	ImageCacheKey = "venice-image"
)

// FetchImageModels discovers generation and edit models, preserving sizing
// constraints so the completion catalog can also guide provider requests.
// It does not write the cache; callers retain stale data on failed refreshes.
func FetchImageModels(ctx context.Context, client *http.Client, apiKey string) ([]cache.CachedModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+"/models?type=all", nil)
	if err != nil {
		return nil, fmt.Errorf("create Venice models request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+config.NormalizeVeniceAPIKey(apiKey))
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch Venice models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("read Venice models error: %w", err)
		}
		return nil, providerhttp.NewStatusError("Venice", resp, body)
	}
	var catalog struct {
		Data []struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			ModelSpec struct {
				Constraints *cache.ImageModelConstraints `json:"constraints"`
			} `json:"model_spec"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return nil, fmt.Errorf("decode Venice models: %w", err)
	}
	var models []cache.CachedModel
	seen := make(map[string]bool)
	for _, model := range catalog.Data {
		id := strings.TrimSpace(model.ID)
		if (model.Type == "image" || model.Type == "inpaint") && id != "" && !seen[id] {
			models = append(models, cache.CachedModel{ID: id, Type: model.Type, ImageConstraints: model.ModelSpec.Constraints})
			seen[id] = true
		}
	}
	slices.SortFunc(models, func(a, b cache.CachedModel) int { return strings.Compare(a.ID, b.ID) })
	return models, nil
}
