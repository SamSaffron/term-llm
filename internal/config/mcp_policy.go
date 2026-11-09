package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// MCPConfig holds application access policy, separate from mcp.json connections.
type MCPConfig struct {
	Servers map[string]MCPServerPolicy `mapstructure:"servers" yaml:"servers,omitempty"`
}

// MCPServerPolicy restricts a server to trusted registry identities. A nil list
// is unrestricted; an explicitly empty list denies every identity.
type MCPServerPolicy struct {
	Agents []string `mapstructure:"agents" yaml:"agents"`
}

// Allows reports whether the trusted identity may use the named server.
func (p MCPConfig) Allows(server, identity string) bool {
	rule, ok := p.Servers[server]
	if !ok || rule.Agents == nil {
		return true
	}
	if identity == "" {
		return false
	}
	for _, name := range rule.Agents {
		if name == identity {
			return true
		}
	}
	return false
}

// LoadMCPPolicy reads policy without resolving provider credentials. Invalid
// configuration is returned as an error, never as an unrestricted policy.
func LoadMCPPolicy() (MCPConfig, error) {
	path, err := FindConfigFile()
	if err != nil {
		return MCPConfig{}, err
	}
	policy, _, err := LoadMCPPolicyFile(path)
	return policy, err
}

// LoadMCPPolicyFile reads one selected file, without a second path lookup.
// nonempty distinguishes intentional mappings (including {}) from editor-save
// windows. Missing paths/read failures never silently select a different file.
func LoadMCPPolicyFile(path string) (policy MCPConfig, nonempty bool, err error) {
	file, err := readFileSettings(path)
	if err != nil {
		return MCPConfig{}, false, fmt.Errorf("read MCP policy %s: %w", path, err)
	}
	policy, err = decodeMCPSettings(file.values, path)
	return policy, !file.empty, err
}

// decodeMCPPolicy is intentionally strict, unlike the weakly typed general
// settings decoder. Connection names are opaque, case-sensitive map keys.
func decodeMCPPolicy(value any) (MCPConfig, error) {
	policy := MCPConfig{}
	if value == nil {
		return policy, nil
	}
	section, ok := value.(map[string]any)
	if !ok {
		return policy, fmt.Errorf("mcp must be a mapping")
	}
	for key := range section {
		if key != "servers" {
			return policy, fmt.Errorf("unknown mcp field %q", key)
		}
	}
	if _, present := section["servers"]; !present {
		return policy, nil
	}
	if section["servers"] == nil {
		return policy, fmt.Errorf("mcp.servers must be a mapping, not null; remove the key for no restrictions")
	}
	servers, ok := section["servers"].(map[string]any)
	if !ok {
		return policy, fmt.Errorf("mcp.servers must be a mapping")
	}
	policy.Servers = make(map[string]MCPServerPolicy, len(servers))
	for name, value := range servers {
		entry, ok := value.(map[string]any)
		if !ok {
			return MCPConfig{}, fmt.Errorf("mcp.servers.%s must be a mapping", name)
		}
		rule := MCPServerPolicy{}
		for key, value := range entry {
			if key != "agents" {
				return MCPConfig{}, fmt.Errorf("unknown mcp.servers.%s field %q", name, key)
			}
			if value == nil {
				return MCPConfig{}, fmt.Errorf("mcp.servers.%s.agents must not be null; use [] to deny everyone or remove the key", name)
			}
			entries, ok := value.([]any)
			if !ok {
				return MCPConfig{}, fmt.Errorf("mcp.servers.%s.agents must be a list of strings", name)
			}
			rule.Agents = make([]string, 0, len(entries))
			for _, item := range entries {
				agent, ok := item.(string)
				if !ok || strings.TrimSpace(agent) == "" {
					return MCPConfig{}, fmt.Errorf("mcp.servers.%s.agents entries must be nonblank strings", name)
				}
				rule.Agents = append(rule.Agents, agent)
			}
		}
		policy.Servers[name] = rule
	}
	return policy, nil
}

// isMCPPolicyKey recognizes dynamic names for config show/validation. The CLI
// editor still uses dotted paths; dotted connection names require direct YAML.
func isMCPPolicyKey(path string) bool {
	if path == "mcp" || path == "mcp.servers" {
		return true
	}
	if !strings.HasPrefix(path, "mcp.servers.") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, "mcp.servers."), ".")
	return parts[0] != "" && (len(parts) == 1 || (len(parts) == 2 && parts[1] == "agents"))
}

// MarshalYAML preserves nil (unrestricted) versus explicitly empty (deny all).
// yaml.v3 otherwise serializes both nil and empty slices as [].
func (p MCPServerPolicy) MarshalYAML() (any, error) {
	if p.Agents == nil {
		return map[string]any{}, nil
	}
	return map[string]any{"agents": p.Agents}, nil
}

// decodeMCPSettings distinguishes absent policy from accidentally empty YAML keys.
func decodeMCPSettings(settings map[string]any, path string) (MCPConfig, error) {
	value, present := settings["mcp"]
	if present && value == nil {
		return MCPConfig{}, fmt.Errorf("decode %s: mcp must be a mapping, not null; remove the key for no restrictions", path)
	}
	policy, err := decodeMCPPolicy(value)
	if err != nil {
		return MCPConfig{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return policy, nil
}

// VisitMCPPolicyKeys visits the opaque server names and strict entry fields in
// a YAML mcp.servers mapping. It reports whether it handled this subtree.
// Dotted CLI paths intentionally remain stricter than literal YAML names.
func VisitMCPPolicyKeys(node *yaml.Node, prefix string, visit func(string, bool, int)) bool {
	if !strings.EqualFold(prefix, "mcp.servers") || node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		name, entry := node.Content[i], node.Content[i+1]
		path := prefix + "." + name.Value
		visit(path, true, name.Line)
		if entry.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(entry.Content); j += 2 {
			key := entry.Content[j]
			visit(path+"."+key.Value, key.Value == "agents", key.Line)
		}
	}
	return true
}
