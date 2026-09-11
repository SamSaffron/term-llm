package image

import (
	"context"
	"time"

	"github.com/samsaffron/term-llm/internal/cache"
	"github.com/samsaffron/term-llm/internal/venice"
)

const veniceModelLookupTimeout = 5 * time.Second

// modelConstraints uses the same catalog as model completion. Missing/legacy
// metadata triggers a refresh even if the cached ID list is still fresh. A
// failed refresh retains stale capabilities rather than guessing native sizing.
func (p *VeniceProvider) modelConstraints(ctx context.Context, model string) *cache.ImageModelConstraints {
	cached, _ := cache.ReadModelCache(venice.ImageCacheKey)
	var stale *cache.ImageModelConstraints
	if cached != nil {
		stale = veniceConstraintsForModel(cached.ModelInfos, model)
		if cache.IsCacheValid(cached) && stale != nil {
			return stale
		}
	}
	ctx, cancel := context.WithTimeout(ctx, veniceModelLookupTimeout)
	defer cancel()
	models, err := venice.FetchImageModels(ctx, veniceHTTPClient, p.apiKey)
	if err != nil || len(models) == 0 {
		return stale
	}
	// Don't replace usable constraints with an incomplete catalog response.
	constraints := veniceConstraintsForModel(models, model)
	if constraints == nil {
		return stale
	}
	_ = cache.WriteModelInfoCache(venice.ImageCacheKey, models)
	return constraints
}

func veniceConstraintsForModel(models []cache.CachedModel, id string) *cache.ImageModelConstraints {
	for _, model := range models {
		if model.ID == id {
			return model.ImageConstraints
		}
	}
	return nil
}
