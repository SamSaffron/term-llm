// Package spawnmodel checks the spawn_agent model argument.
package spawnmodel

import (
	"errors"
	"strings"
	"unicode"
)

// Catalog is implemented by spawn runners that can validate model overrides
// against the providers this host enables. Recovery guidance belongs in
// validation errors, not in the tool schema.
type Catalog interface {
	// ValidateSpawnModel reports why an explicit provider:model override
	// cannot be used, or nil when it can.
	ValidateSpawnModel(model string) error
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
