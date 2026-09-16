package live

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/samsaffron/term-llm/internal/config"
)

// ClientTool describes an action implemented by the hosting browser, not by the
// execution agent. Only OpenAI Realtime supports named client function calls.
type ClientTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

var clientToolName = regexp.MustCompile(`^ui_[A-Za-z0-9_]{1,60}$`)

// ValidateClientTools rejects unsupported modes and malformed registrations
// before credentials or provider network activity. Argument-level validation
// belongs to the trusted client handler; schemas are passed to the provider.
func ValidateClientTools(cfg config.LiveConfig, tools []ClientTool) error {
	if len(tools) == 0 {
		return nil
	}
	if cfg.Provider != config.LiveProviderOpenAI || !cfg.OpenAI.UsesRealtime() {
		return fmt.Errorf("client tools require the openai provider with an explicitly configured realtime model")
	}
	if len(tools) > 16 {
		return fmt.Errorf("at most 16 client tools are allowed")
	}
	seen := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if !clientToolName.MatchString(tool.Name) || seen[tool.Name] {
			return fmt.Errorf("client tool names must be unique and match ui_[A-Za-z0-9_]{1,60}")
		}
		seen[tool.Name] = true
		if len(tool.Description) == 0 || len(tool.Description) > 1024 {
			return fmt.Errorf("client tool %s description must be 1–1024 bytes", tool.Name)
		}
		if tool.Parameters["type"] != "object" {
			return fmt.Errorf("client tool %s parameters must be an object schema", tool.Name)
		}
		if _, ok := tool.Parameters["properties"].(map[string]any); !ok {
			return fmt.Errorf("client tool %s parameters must define properties", tool.Name)
		}
		encoded, err := json.Marshal(tool.Parameters)
		if err != nil || len(encoded) > 16<<10 {
			return fmt.Errorf("client tool %s schema must be JSON of at most 16 KiB", tool.Name)
		}
	}
	return nil
}
