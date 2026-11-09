package mcp

import "github.com/samsaffron/term-llm/internal/runtimeoutput"

// Disable stops an MCP server.
func (m *Manager) Disable(name string) error { return m.disable(name, false) }

func (m *Manager) disable(name string, deferred bool) error {
	m.mu.Lock()
	if startup, ok := m.startups[name]; ok {
		delete(m.startups, name)
		startup.startupCancel()
		startup.processCancel()
	}
	client, ok := m.clients[name]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.clients, name)
	_, hadCatalogue := m.catalogues[name]
	delete(m.catalogues, name)
	var aggregate *CatalogueSnapshot
	var catalogueHandler func(CatalogueEvent)
	if hadCatalogue {
		aggregate = m.publishAggregateLocked()
		catalogueHandler = m.catalogueHandler
	}
	if state, ok := m.statuses[name]; ok {
		state.Status = StatusStopped
		state.Error = nil
		state.RefreshError = nil
		state.ToolCount = 0
		state.Client = nil
	}
	var stopped chan struct{}
	if deferred {
		stopped = make(chan struct{})
		m.stopping[client] = stopped
	}
	m.mu.Unlock()

	m.sendStatus(name, StatusStopped, nil)
	if aggregate != nil && catalogueHandler != nil {
		catalogueHandler(CatalogueEvent{Server: name, Snapshot: copyCatalogueSnapshot(aggregate)})
	}

	if !deferred {
		return client.Stop()
	}
	// Logical revocation and catalogue publication are synchronous. Process
	// shutdown is joined by StopAll but must not block a serve session mutex.
	go func() {
		if err := client.Stop(); err != nil {
			runtimeoutput.Logf("[mcp] stop revoked %s: %v", name, err)
		}
		m.mu.Lock()
		delete(m.stopping, client)
		m.mu.Unlock()
		close(stopped)
	}()
	return nil
}
