package config

// WebConfig holds Web UI-only presentation preferences. It never stores
// per-project agent choices: project_default_agent applies to every project.
type WebConfig struct {
	// ProjectDefaultAgent is the agent a new project conversation starts with.
	// It must name an available agent while projects are enabled; serve rejects
	// an empty or unknown name instead of silently choosing another agent.
	ProjectDefaultAgent string               `mapstructure:"project_default_agent" yaml:"project_default_agent,omitempty"`
	AgentPicker         WebAgentPickerConfig `mapstructure:"agent_picker" yaml:"agent_picker,omitempty"`
}

// WebAgentPickerConfig shapes the new-chat agent picker. Common is the ordered
// block above the alphabetical remainder; an explicit [] disables the split.
// The pointer preserves that distinction when a config is marshaled again:
// nil means "use the stock list", while a non-nil empty slice is explicit.
type WebAgentPickerConfig struct {
	Common *[]string `mapstructure:"common" yaml:"common,omitempty"`
}

// CommonAgents returns the effective ordered common-agent block.
func (c WebAgentPickerConfig) CommonAgents() []string {
	if c.Common == nil {
		return DefaultWebAgentPickerCommon()
	}
	return append([]string{}, (*c.Common)...)
}
