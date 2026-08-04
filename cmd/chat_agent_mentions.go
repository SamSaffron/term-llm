package cmd

import (
	"errors"
	"fmt"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

type runtimeAgentMentionCapability struct {
	engine  *llm.Engine
	manager *tools.ToolManager
}

func (c runtimeAgentMentionCapability) spawnTool() (*tools.SpawnAgentTool, error) {
	if c.manager == nil {
		return nil, errors.New("agent mentions are unavailable because spawn_agent is not enabled in this session")
	}
	tool := c.manager.GetSpawnAgentTool()
	if tool == nil {
		return nil, errors.New("agent mentions are unavailable because spawn_agent is not enabled in this session")
	}
	if c.engine == nil {
		return nil, errors.New("agent mentions are unavailable because spawn_agent is not registered with the active engine")
	}
	registered, ok := c.engine.Tools().Get(tools.SpawnAgentToolName)
	if !ok || registered != tool {
		return nil, errors.New("agent mentions are unavailable because spawn_agent is not registered with the active engine")
	}
	if !c.engine.IsToolAllowed(tools.SpawnAgentToolName) {
		return nil, errors.New("agent mentions are unavailable because spawn_agent is blocked by the active tool restriction")
	}
	return tool, nil
}

func (c runtimeAgentMentionCapability) PermittedAgentNames() ([]string, error) {
	tool, err := c.spawnTool()
	if err != nil {
		return nil, err
	}
	return tool.PermittedAgentNames()
}

func (c runtimeAgentMentionCapability) ValidateAgentMention(name string) error {
	tool, err := c.spawnTool()
	if err != nil {
		return err
	}
	if err := tool.CanSpawnAgent(name); err != nil {
		return fmt.Errorf("agent %q cannot be delegated to by the active session: %w", name, err)
	}
	return nil
}
