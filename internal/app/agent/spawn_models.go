package agent

import (
	"fmt"
	"slices"
	"strings"

	"github.com/samsaffron/term-llm/internal/app/providers"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

// spawnModelCredentials memoizes the local login probes behind provider
// discovery, so listing providers for each spawn_agent spec stays cheap.
var spawnModelCredentials llm.ProviderCredentialCache

// SpawnModelCatalog answers which providers a spawn_agent model override may
// name, using the same discovery rules as the web UI model picker. A runner
// embeds it to satisfy spawnmodel.Catalog.
type SpawnModelCatalog struct {
	cfg *config.Config
	// hasLocalCredentials overrides the login probe (tests); nil uses the
	// shared cache.
	hasLocalCredentials func(string) bool
}

// NewSpawnModelCatalog returns a catalog over cfg.
func NewSpawnModelCatalog(cfg *config.Config) SpawnModelCatalog {
	return SpawnModelCatalog{cfg: cfg}
}

// SpawnModelProviders lists the providers this host enables.
func (c SpawnModelCatalog) SpawnModelProviders() []string {
	if c.cfg == nil {
		return nil
	}
	has := c.hasLocalCredentials
	if has == nil {
		has = spawnModelCredentials.Has
	}
	var names []string
	for _, p := range providers.BuildListWith(c.cfg, has) {
		if p.Configured {
			names = append(names, p.Name)
		}
	}
	return names
}

// ValidateSpawnModel rejects a provider:model override whose provider this
// host does not enable. ApplyModelOverride treats an unknown prefix as part of
// a plain model name (for colon-tagged local models), so without this check a
// guessed prefix such as "codex:" or "expensive:" reaches the default
// provider's API and the child fails there.
func (c SpawnModelCatalog) ValidateSpawnModel(model string) error {
	if c.cfg == nil {
		return nil
	}
	enabled := c.SpawnModelProviders()
	provider, _, err := llm.ParseProviderModel(model, c.cfg)
	if err == nil && (provider == "debug" || slices.Contains(enabled, provider)) {
		return nil
	}
	name, _, _ := strings.Cut(model, ":")
	reason := fmt.Sprintf("provider %q is not enabled on this host", name)
	if err != nil {
		reason = fmt.Sprintf("unknown provider %q", name)
	}
	if len(enabled) == 0 {
		return fmt.Errorf("model %q: %s; omit model to use the agent's default", model, reason)
	}
	example := enabled[0]
	if slices.Contains(enabled, c.cfg.DefaultProvider) {
		example = c.cfg.DefaultProvider
	}
	return fmt.Errorf("model %q: %s; use one of %s (for example %q), or omit model to use the agent's default",
		model, reason, strings.Join(enabled, ", "), example+":<model>")
}
