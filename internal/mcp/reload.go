package mcp

import (
	"context"
	"fmt"

	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

func (m *Manager) registerReload() {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	if m.reloadUnregister != nil {
		return
	}
	m.reloadUnregister = restart.Default.Register(&restart.Resource{Prepare: func(ctx context.Context) (func(context.Context), error) {
		names := m.EnabledServers()
		undo := func(recovery context.Context) {
			for _, name := range names {
				if err := m.Enable(recovery, name); err != nil {
					runtimeoutput.Logf("[reload] restore MCP %s: %v", name, err)
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := m.stopAll(); err != nil {
			return undo, fmt.Errorf("stop idle MCP servers: %w", err)
		}
		return undo, ctx.Err()
	}})
}

func (m *Manager) unregisterReload() {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	if m.reloadUnregister != nil {
		m.reloadUnregister()
		m.reloadUnregister = nil
	}
}
