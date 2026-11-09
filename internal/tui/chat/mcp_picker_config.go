package chat

func (m *Model) showMCPPicker() {
	if m == nil || m.dialog == nil || m.mcpManager == nil {
		return
	}
	sessionID := ""
	if m.sess != nil {
		sessionID = m.sess.ID
	}
	if m.engine != nil {
		if diagnostics, ok := m.engine.ToolDiscoveryDiagnostics(sessionID); ok {
			m.dialog.ShowMCPPicker(m.mcpManager, diagnostics)
			return
		}
	}
	m.dialog.ShowMCPPicker(m.mcpManager)
}
