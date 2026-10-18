package chat

import (
	"errors"
	"fmt"

	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/tools"
)

type RuntimeAgentMentionCapability struct {
	Engine  func() *engine.Engine
	Manager *tools.ToolManager
}

func (c RuntimeAgentMentionCapability) spawnTool() (*tools.SpawnAgentTool, error) {
	if c.Manager == nil {
		return nil, errors.New("agent mentions are unavailable because spawn_agent is not enabled in this session")
	}
	tool := c.Manager.GetSpawnAgentTool()
	if tool == nil {
		return nil, errors.New("agent mentions are unavailable because spawn_agent is not enabled in this session")
	}
	if c.Engine == nil {
		return nil, errors.New("agent mentions are unavailable because spawn_agent is not registered with the active engine")
	}
	engine := c.Engine()
	if engine == nil {
		return nil, errors.New("agent mentions are unavailable because spawn_agent is not registered with the active engine")
	}
	registered, ok := engine.Tools().Get(tools.SpawnAgentToolName)
	if !ok || registered != tool {
		return nil, errors.New("agent mentions are unavailable because spawn_agent is not registered with the active engine")
	}
	if !engine.IsToolAllowed(tools.SpawnAgentToolName) {
		return nil, errors.New("agent mentions are unavailable because spawn_agent is blocked by the active tool restriction")
	}
	return tool, nil
}

func (c RuntimeAgentMentionCapability) PermittedAgentNames() ([]string, error) {
	tool, err := c.spawnTool()
	if err != nil {
		return nil, err
	}
	return tool.PermittedAgentNames()
}

func (c RuntimeAgentMentionCapability) ValidateAgentMention(name string) error {
	tool, err := c.spawnTool()
	if err != nil {
		return err
	}
	if err := tool.CanSpawnAgent(name); err != nil {
		return fmt.Errorf("agent %q cannot be delegated to by the active session: %w", name, err)
	}
	return nil
}
