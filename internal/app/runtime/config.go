package runtime

import "github.com/samsaffron/term-llm/internal/config"

// CloneConfig returns a run-local config copy. It preserves the historical
// cloneConfigForServeJob copy boundary exactly.
func CloneConfig(src *config.Config) *config.Config {
	if src == nil {
		return &config.Config{}
	}
	clone := *src
	if src.Providers != nil {
		clone.Providers = make(map[string]config.ProviderConfig, len(src.Providers))
		for name, pc := range src.Providers {
			// Deep copy the slice/pointer fields so per-run provider/model
			// overrides never mutate the shared base config (mirrors the
			// deep-copy discipline in spawn_runner.go). A shallow copy would
			// alias Models and UseNativeSearch across runs.
			if pc.Models != nil {
				pc.Models = append([]string(nil), pc.Models...)
			}
			if pc.ModelConfigs != nil {
				pc.ModelConfigs = append([]config.ProviderModelConfig(nil), pc.ModelConfigs...)
				for i := range pc.ModelConfigs {
					pc.ModelConfigs[i].ReasoningEfforts = append([]string(nil), pc.ModelConfigs[i].ReasoningEfforts...)
				}
			}
			if pc.UseNativeSearch != nil {
				v := *pc.UseNativeSearch
				pc.UseNativeSearch = &v
			}
			clone.Providers[name] = pc
		}
	}
	if src.Agents.Preferences != nil {
		clone.Agents.Preferences = make(map[string]config.AgentPreference, len(src.Agents.Preferences))
		for name, pref := range src.Agents.Preferences {
			clone.Agents.Preferences[name] = pref
		}
	}
	return &clone
}
