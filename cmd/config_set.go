package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/term-llm/term-llm/internal/config"
	"gopkg.in/yaml.v3"
)

// configSet sets a configuration value while preserving comments
func configSet(cmd *cobra.Command, args []string) error {
	key := args[0]
	if err := validateMCPConfigEdit(key); err != nil {
		return err
	}
	value := args[1]
	var policyAgents []string
	if strings.EqualFold(strings.Split(key, ".")[0], "mcp") {
		var err error
		policyAgents, err = parseMCPPolicyAgents(value)
		if err != nil {
			return err
		}
	}
	if key == "live.control_plane" {
		controlPlane, err := config.ParseLiveControlPlane(value)
		if err != nil {
			return err
		}
		value = string(controlPlane)
	}
	if isApprovalConfigKey(key) {
		if _, err := parseConfiguredApprovalMode(key, value); err != nil {
			return err
		}
	}

	configPath, err := config.GetConfigPath()
	if err != nil {
		return fmt.Errorf("failed to get config path: %w", err)
	}

	// Ensure config directory exists
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Read existing file or create empty document
	var root yaml.Node
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Create new document with empty mapping
			root = yaml.Node{
				Kind: yaml.DocumentNode,
				Content: []*yaml.Node{{
					Kind: yaml.MappingNode,
				}},
			}
		} else {
			return fmt.Errorf("failed to read config: %w", err)
		}
	} else {
		if err := yaml.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("failed to parse config: %w", err)
		}
	}

	// Navigate/create path and set value
	keyParts, err := normalizeMCPConfigEdit(&root, strings.Split(key, "."))
	if err != nil {
		return err
	}
	if err := setYAMLValue(&root, keyParts, value); err != nil {
		return fmt.Errorf("failed to set value: %w", err)
	}

	if policyAgents != nil {
		if err := setMCPPolicySequence(&root, strings.Join(keyParts, "."), policyAgents); err != nil {
			return fmt.Errorf("set MCP policy: %w", err)
		}
	}

	// Write back
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		return fmt.Errorf("failed to encode config: %w", err)
	}
	encoder.Close()

	if err := config.WritePrivateConfigFile(configPath, buf.Bytes()); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	fmt.Printf("%s = %s\n", key, value)
	return nil
}
