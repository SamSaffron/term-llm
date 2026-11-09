package cmd

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// MCP connection names containing dots intentionally require direct YAML edits.
func validateMCPConfigEdit(key string) error {
	if !strings.EqualFold(strings.Split(key, ".")[0], "mcp") {
		return nil
	}
	parts := strings.Split(key, ".")
	if len(parts) != 4 || parts[1] != "servers" || parts[2] == "" || parts[3] != "agents" {
		return fmt.Errorf("set MCP policy with mcp.servers.<name>.agents; edit config.yaml directly for dotted server names")
	}
	return nil
}

func parseMCPPolicyAgents(value string) ([]string, error) {
	var names []string
	for _, raw := range strings.Split(value, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, fmt.Errorf("MCP agents must be nonblank comma-separated names; edit config.yaml directly to set agents: [] (deny everyone)")
		}
		names = append(names, name)
	}
	return names, nil
}

// setMCPPolicySequence replaces only the already-created leaf, preserving the
// surrounding YAML comments and always encoding agent names as strings.
func setMCPPolicySequence(root *yaml.Node, key string, names []string) error {
	if root == nil || root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return fmt.Errorf("invalid config document")
	}
	current := root.Content[0]
	for _, part := range strings.Split(key, ".") {
		if current == nil || current.Kind != yaml.MappingNode {
			return fmt.Errorf("MCP policy path %q is not a mapping", part)
		}
		var next *yaml.Node
		for j := 0; j+1 < len(current.Content); j += 2 {
			if current.Content[j].Value == part {
				next = current.Content[j+1]
				break
			}
		}
		if next == nil {
			return fmt.Errorf("MCP policy path segment %q is missing", part)
		}
		current = next
	}
	current.Kind, current.Tag, current.Value = yaml.SequenceNode, "!!seq", ""
	current.Content = nil
	for _, name := range names {
		current.Content = append(current.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name})
	}
	return nil
}

// normalizeMCPConfigEdit reuses a case-variant root instead of creating a second
// top-level policy section, while preserving opaque case-sensitive server names.
func normalizeMCPConfigEdit(root *yaml.Node, parts []string) ([]string, error) {
	if len(parts) == 0 || !strings.EqualFold(parts[0], "mcp") {
		return parts, nil
	}
	parts[0] = "mcp"
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return parts, nil
	}
	mapping := root.Content[0]
	if mapping.Kind != yaml.MappingNode {
		return parts, nil
	}
	var policyKey *yaml.Node
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if strings.EqualFold(mapping.Content[i].Value, "mcp") {
			if policyKey != nil {
				return nil, fmt.Errorf("duplicate case-variant mcp keys; repair config.yaml before editing policy")
			}
			policyKey = mapping.Content[i]
		}
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if strings.EqualFold(mapping.Content[i].Value, "mcp") {
			mapping.Content[i].Value = "mcp"
		}
	}
	return parts, nil
}
