package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Save writes the config to disk
func Save(cfg *Config) error {
	path, err := GetConfigPath()
	if err != nil {
		return err
	}

	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	type saveProvider struct {
		Type                      ProviderType      `yaml:"type,omitempty"`
		Model                     string            `yaml:"model,omitempty"`
		FastModel                 string            `yaml:"fast_model,omitempty"`
		FastProvider              string            `yaml:"fast_provider,omitempty"`
		ServiceTier               string            `yaml:"service_tier,omitempty"`
		UsePreviousResponseID     *bool             `yaml:"use_previous_response_id,omitempty"`
		RequestEncryptedReasoning *bool             `yaml:"request_encrypted_reasoning,omitempty"`
		Responses                 ResponsesConfig   `yaml:"responses,omitempty"`
		FileUpload                *FileUploadConfig `yaml:"file_upload,omitempty"`
		BaseURL                   string            `yaml:"base_url,omitempty"`
		AppURL                    string            `yaml:"app_url,omitempty"`
		AppTitle                  string            `yaml:"app_title,omitempty"`
	}
	type saveExec struct {
		Suggestions int `yaml:"suggestions"`
	}
	type saveImage struct {
		Provider string `yaml:"provider,omitempty"`
	}
	type saveConfig struct {
		MCP             *MCPConfig              `yaml:"mcp,omitempty"`
		DefaultProvider string                  `yaml:"default_provider,omitempty"`
		Exec            saveExec                `yaml:"exec"`
		Image           *saveImage              `yaml:"image,omitempty"`
		Providers       map[string]saveProvider `yaml:"providers,omitempty"`
	}

	saved := saveConfig{
		DefaultProvider: cfg.DefaultProvider,
		Exec:            saveExec{Suggestions: cfg.Exec.Suggestions},
		Providers:       make(map[string]saveProvider, len(cfg.Providers)),
	}
	policy := cfg.MCP
	if len(policy.Servers) == 0 {
		// Setup wizards save a fresh config. Missing policy is not permission
		// to erase the existing guardrail, including when it cannot be read.
		policy, err = LoadMCPPolicy()
		if err != nil {
			return fmt.Errorf("preserve MCP policy: %w", err)
		}
	}
	if len(policy.Servers) != 0 {
		saved.MCP = &policy
	}
	if cfg.Image.Provider != "" {
		saved.Image = &saveImage{Provider: cfg.Image.Provider}
	}
	for name, p := range cfg.Providers {
		saved.Providers[name] = saveProvider{
			Type:                      p.Type,
			Model:                     p.Model,
			FastModel:                 p.FastModel,
			FastProvider:              p.FastProvider,
			ServiceTier:               p.ServiceTier,
			UsePreviousResponseID:     p.UsePreviousResponseID,
			RequestEncryptedReasoning: p.RequestEncryptedReasoning,
			Responses:                 p.Responses,
			FileUpload:                p.FileUpload,
			BaseURL:                   p.BaseURL,
			AppURL:                    p.AppURL,
			AppTitle:                  p.AppTitle,
		}
	}

	data, err := yaml.Marshal(saved)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	return WritePrivateConfigFile(path, data)
}
