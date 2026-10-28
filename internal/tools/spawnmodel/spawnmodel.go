// Package spawnmodel checks and describes the spawn_agent model argument.
package spawnmodel

import (
	"errors"
	"strings"
	"unicode"
)

// Catalog is implemented by spawn runners that know which providers this host
// enables. Without it models guess a provider from their own branding (for
// example "codex:" on ChatGPT), and the unknown prefix reaches the provider
// API as part of the model name.
type Catalog interface {
	// SpawnModelProviders lists the provider names a model override may use.
	SpawnModelProviders() []string
	// ValidateSpawnModel reports why an explicit provider:model override
	// cannot be used, or nil when it can.
	ValidateSpawnModel(model string) error
}

const baseDescription = "Optional model override in exact provider:model format. If omitted, the sub-agent uses its configured/default model."

// Description returns the model argument description, naming the enabled
// providers when runner is a Catalog that lists them.
func Description(runner any) string {
	catalog, ok := runner.(Catalog)
	if !ok {
		return baseDescription
	}
	providers := catalog.SpawnModelProviders()
	if len(providers) == 0 {
		return baseDescription
	}
	return baseDescription + " The provider must be one of: " + strings.Join(providers, ", ") + "."
}

// Validate checks a model override requested through spawn_agent. Models set
// in agent configuration do not pass through here: they may be plain or
// colon-tagged names such as "llama3:8b".
func Validate(runner any, model string) error {
	if model == "" {
		return nil
	}
	if !qualified(model) {
		return errors.New("model must use exact provider:model format; omit it to use the configured/default model")
	}
	if catalog, ok := runner.(Catalog); ok {
		return catalog.ValidateSpawnModel(model)
	}
	return nil
}

func qualified(model string) bool {
	provider, modelName, found := strings.Cut(model, ":")
	return found && provider != "" && modelName != "" &&
		!strings.ContainsFunc(provider, unicode.IsSpace) &&
		!strings.ContainsFunc(modelName, unicode.IsSpace)
}
