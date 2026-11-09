package agents

import "path/filepath"

// PolicyIdentity returns the trusted registry lookup identity, not the name
// asserted by agent.yaml. Path loads and configured search paths never qualify.
func PolicyIdentity(agent *Agent) string {
	if agent == nil {
		return ""
	}
	if agent.Source != SourceUser && agent.Source != SourceBuiltin {
		return ""
	}
	return agent.policyIdentity
}

// Reference is the name/path to persist for re-resolution, rather than a
// self-asserted YAML name. This also prevents a local agent impersonating a
// trusted agent when its session is resumed.
func Reference(agent *Agent) string {
	if agent == nil {
		return ""
	}
	if agent.Source == SourceUser || agent.Source == SourceBuiltin {
		return agent.lookupName
	}
	if agent.Source == SourceLocal || agent.Source == SourceSearchPath {
		if agent.SourcePath != "" {
			if path, err := filepath.Abs(agent.SourcePath); err == nil {
				return path
			}
		}
		return ""
	}
	return ""
}

func sourceRank(source AgentSource) int {
	switch source {
	case SourceBuiltin:
		return 0
	case SourceUser:
		return 1
	case SourceSearchPath:
		return 2
	default:
		return 3
	}
}

// WithoutPolicyIdentity preserves a resolved definition while withholding trust
// when a legacy session has ambiguous provenance. Do not mutate registry entries.
func WithoutPolicyIdentity(agent *Agent) *Agent {
	if agent == nil {
		return nil
	}
	copy := *agent
	copy.policyIdentity = ""
	return &copy
}

func bindRegistryIdentity(agent *Agent, name string) {
	agent.lookupName, agent.policyIdentity = name, ""
	if agent.Source == SourceUser || agent.Source == SourceBuiltin {
		agent.policyIdentity = name
	}
}
