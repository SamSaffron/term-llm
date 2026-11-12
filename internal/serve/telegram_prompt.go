package serve

// sessionSystemPrompt keeps the platform's instructions aligned with the engine
// whose skill capabilities were admitted. A non-nil empty prompt intentionally
// overrides the startup default, while legacy factories retain their fallback.
func (m *telegramSessionMgr) sessionSystemPrompt(sess *telegramSession) string {
	if sess != nil && sess.runtime != nil && sess.runtime.SystemPrompt != nil {
		return *sess.runtime.SystemPrompt
	}
	return m.settings.SystemPrompt
}
