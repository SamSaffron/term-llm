package cmd

func (rt *serveRuntime) hasActiveRun() bool {
	rt.interruptMu.Lock()
	defer rt.interruptMu.Unlock()
	return rt.activeInterrupt != nil
}

func (rt *serveRuntime) hasActiveSideQuestion() bool {
	rt.sideQuestion.mu.Lock()
	defer rt.sideQuestion.mu.Unlock()
	return rt.sideQuestion.running
}

// hasActiveActivity protects runtime lifecycle operations from retiring a
// runtime while admitted synchronous work, a main response, or a private side
// request is active. Admission covers setup before runOnce claims rt.mu and
// the gaps between goal/fallback turns without locking out metadata writes.
func (rt *serveRuntime) hasActiveActivity() bool {
	return rt.admittedActivity.Load() > 0 || rt.compacting.Load() || rt.hasActiveRun() || rt.hasActiveSideQuestion()
}

func (rt *serveRuntime) clearLastUIRunError() {
	rt.uiStateMu.Lock()
	defer rt.uiStateMu.Unlock()
	rt.lastUIRunError = ""
}

func (rt *serveRuntime) setLastUIRunError(message string) {
	rt.uiStateMu.Lock()
	defer rt.uiStateMu.Unlock()
	rt.lastUIRunError = message
}

func (rt *serveRuntime) consumeLastUIRunError() string {
	rt.uiStateMu.Lock()
	defer rt.uiStateMu.Unlock()
	message := rt.lastUIRunError
	rt.lastUIRunError = ""
	return message
}
