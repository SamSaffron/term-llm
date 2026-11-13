package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// ToolOrigin identifies the configured MCP server and the exact remote tool that
// own this binding. It is never derived from the provider-visible flattened name,
// so a remote tool called spawn_agent receives no local-agent provenance.
func (t *MCPTool) ToolOrigin() core.ToolOrigin {
	if t == nil {
		return core.ToolOrigin{}
	}
	id := t.server
	switch {
	case t.originalName != "":
		id += "\x00" + t.originalName
	case id == "" && t.toolSpec.Name != "":
		// Legacy single-server wrapper: no structured catalogue identity exists.
		id = "legacy\x00" + t.toolSpec.Name
	}
	return core.ToolOrigin{Class: core.ToolOriginMCP, ID: id}
}

// mcpServerBindingConfig keeps the connection/credential fields that determine
// which authority an MCP tool call reaches. Presentation-only settings
// (AlwaysLoad, Sampling) do not invalidate a binding.
type mcpServerBindingConfig struct {
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	OAuth   *OAuthConfig      `json:"oauth,omitempty"`
}

// ToolBindingID is stable across an identical catalogue refresh of the same
// manager, server target, credentials, and tool schema. A schema, manager,
// transport, target, or credential change yields a new generation. The entire
// key is hashed so diagnostics contain an opaque token, never header/env values.
func (t *MCPTool) ToolBindingID() string {
	if t == nil {
		return ""
	}
	manager := ""
	if t.manager != nil {
		manager = fmt.Sprintf("%p", t.manager)
	}
	key := struct {
		Origin  string                  `json:"origin"`
		Manager string                  `json:"manager"`
		Server  *mcpServerBindingConfig `json:"server,omitempty"`
		Spec    core.ToolSpec           `json:"spec"`
	}{
		Origin:  t.ToolOrigin().ID,
		Manager: manager,
		Spec:    t.toolSpec,
	}
	if config, ok := t.serverBindingConfig(); ok {
		key.Server = config
	}
	raw, err := json.Marshal(key)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// serverBindingConfig reads a defensive copy of the configured target. The
// returned values are marshalled and hashed, never rendered directly.
func (t *MCPTool) serverBindingConfig() (*mcpServerBindingConfig, bool) {
	if t == nil || t.manager == nil || t.server == "" {
		return nil, false
	}
	config := t.manager.Config()
	if config == nil {
		return nil, false
	}
	server, ok := config.Servers[t.server]
	if !ok {
		return nil, false
	}
	return &mcpServerBindingConfig{
		Type:    server.Type,
		URL:     server.URL,
		Command: server.Command,
		Args:    append([]string(nil), server.Args...),
		Env:     cloneStringMap(server.Env),
		Headers: cloneStringMap(server.Headers),
		OAuth:   server.OAuth,
	}, true
}
