package proxy

import (
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

// ModelAlias is a single routable provider/model entry exported by the proxy.
// Alias is the canonical, client-facing key ("provider/model"); Provider and
// Model are the concrete upstream route the proxy dispatches to.
type ModelAlias struct {
	Alias    string `json:"alias"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Display  string `json:"display,omitempty"`
	Builtin  bool   `json:"builtin"`
}

// Catalog is the set of provider/model aliases the proxy can route to, derived
// from the term-llm configuration (configured providers plus built-in provider
// types such as claude-bin). It also resolves client-supplied model strings to
// concrete routes.
type Catalog struct {
	providers map[string]string // lowercased name -> canonical name
	aliases   []ModelAlias
	byKey     map[string]ModelAlias   // lowercased alias/display/wildcard -> entry
	byModel   map[string][]ModelAlias // lowercased model id -> entries (ambiguity check)
}

// BuildCatalog constructs the alias catalog from configuration. Every provider
// (configured or built-in) gets a "provider/*" wildcard entry so operators can
// grant whole-provider access; providers with enumerable models also get one
// entry per model. This guarantees local binary providers like claude-bin are
// exportable over HTTP even when they publish no curated model list.
func BuildCatalog(cfg *config.Config) *Catalog {
	c := &Catalog{
		providers: map[string]string{},
		byKey:     map[string]ModelAlias{},
		byModel:   map[string][]ModelAlias{},
	}
	builtin := map[string]bool{}
	for _, n := range llm.GetBuiltInProviderNames() {
		builtin[n] = true
	}

	for _, name := range llm.GetProviderNames(cfg) {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		c.providers[strings.ToLower(name)] = name
		isBuiltin := builtin[name]

		// Collect models: curated built-in IDs + configured models/aliases.
		modelIDs := map[string]string{} // id -> display alias
		for _, id := range llm.ResolveProviderModelIDs(name) {
			if id = strings.TrimSpace(id); id != "" {
				if _, ok := modelIDs[id]; !ok {
					modelIDs[id] = ""
				}
			}
		}
		if cfg != nil {
			if pc, ok := cfg.Providers[name]; ok {
				for _, id := range pc.Models {
					if id = strings.TrimSpace(id); id != "" {
						if _, ok := modelIDs[id]; !ok {
							modelIDs[id] = ""
						}
					}
				}
				for _, mc := range pc.ModelConfigs {
					id := strings.TrimSpace(mc.ID)
					if id == "" {
						continue
					}
					modelIDs[id] = strings.TrimSpace(mc.Alias)
				}
			}
		}

		for id, display := range modelIDs {
			entry := ModelAlias{
				Alias:    name + "/" + id,
				Provider: name,
				Model:    id,
				Display:  display,
				Builtin:  isBuiltin,
			}
			c.register(entry)
		}

		// Wildcard entry: always grantable/routable for the provider default.
		wildcard := ModelAlias{
			Alias:    name + "/" + WildcardModel,
			Provider: name,
			Model:    WildcardModel,
			Builtin:  isBuiltin,
		}
		c.byKey[strings.ToLower(wildcard.Alias)] = wildcard
		c.aliases = append(c.aliases, wildcard)
	}

	sort.Slice(c.aliases, func(i, j int) bool {
		if c.aliases[i].Provider != c.aliases[j].Provider {
			return c.aliases[i].Provider < c.aliases[j].Provider
		}
		return c.aliases[i].Model < c.aliases[j].Model
	})
	return c
}

func (c *Catalog) register(entry ModelAlias) {
	c.aliases = append(c.aliases, entry)
	c.byKey[strings.ToLower(entry.Alias)] = entry
	if entry.Display != "" {
		c.byKey[strings.ToLower(entry.Display)] = entry
	}
	c.byModel[strings.ToLower(entry.Model)] = append(c.byModel[strings.ToLower(entry.Model)], entry)
}

// List returns the catalog entries (canonical model aliases plus one wildcard
// per provider), sorted by provider then model.
func (c *Catalog) List() []ModelAlias {
	out := make([]ModelAlias, len(c.aliases))
	copy(out, c.aliases)
	return out
}

// HasProvider reports whether name is a known provider in the catalog.
func (c *Catalog) HasProvider(name string) bool {
	_, ok := c.providers[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// Resolve maps a client-supplied model string to a concrete route. It accepts:
//   - canonical aliases ("provider/model") and configured display aliases
//   - "provider:model" as an alternate separator
//   - provider-qualified models even when the specific model is not enumerated
//     (routes to that provider; grant checks still apply)
//   - a bare model id when it is unambiguous across providers
//
// The second return is false when the string cannot be mapped to any provider.
func (c *Catalog) Resolve(requested string) (ModelAlias, bool) {
	key := strings.TrimSpace(requested)
	if key == "" {
		return ModelAlias{}, false
	}
	lk := strings.ToLower(key)

	// 1. Exact canonical alias, display alias, or wildcard.
	if a, ok := c.byKey[lk]; ok {
		return a, true
	}

	// 2. Provider-qualified ("provider/model" or "provider:model").
	if prov, model, ok := c.splitProviderModel(key); ok {
		if a, ok := c.byKey[strings.ToLower(prov+"/"+model)]; ok {
			return a, true
		}
		return ModelAlias{
			Alias:    prov + "/" + model,
			Provider: prov,
			Model:    model,
			Builtin:  c.isBuiltinProvider(prov),
		}, true
	}

	// 3. Bare model id, only if unambiguous.
	if entries := c.byModel[lk]; len(entries) == 1 {
		return entries[0], true
	}
	return ModelAlias{}, false
}

func (c *Catalog) splitProviderModel(key string) (provider, model string, ok bool) {
	if idx := strings.Index(key, "/"); idx > 0 {
		prov := key[:idx]
		if canonical, known := c.providers[strings.ToLower(prov)]; known {
			return canonical, key[idx+1:], true
		}
		// Unknown provider prefix: still treat as provider-qualified so the
		// resulting denial/access-request captures what the client asked for.
		return prov, key[idx+1:], true
	}
	if idx := strings.Index(key, ":"); idx > 0 {
		prov := key[:idx]
		if canonical, known := c.providers[strings.ToLower(prov)]; known {
			return canonical, key[idx+1:], true
		}
	}
	return "", "", false
}

func (c *Catalog) isBuiltinProvider(name string) bool {
	for _, n := range llm.GetBuiltInProviderNames() {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}
