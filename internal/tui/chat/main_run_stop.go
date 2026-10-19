package chat

import "context"

// Stop records human intent for the targeted execution, unlike generic Cancel.
// A stale view cannot mark or cancel a replacement.
func (m *MainRunManager) Stop(sessionID, runID string) bool {
	return m.cancelRun(sessionID, runID, true)
}

func (m *MainRunManager) cancelRun(sessionID, runID string, human bool) bool {
	if m == nil || (human && runID == "") {
		return false
	}
	m.mu.Lock()
	run := m.runs[sessionID]
	if runID != "" {
		if run == nil || run.id != runID {
			m.mu.Unlock()
			return false
		}
		run.mu.Lock()
		if !run.active {
			run.mu.Unlock()
			m.mu.Unlock()
			return false
		}
		if human {
			run.userStop.Store(true)
		}
		run.mu.Unlock()
	}
	coordinator := m.steeringCoordinator(sessionID)
	if coordinator != nil {
		coordinator.cancelled.Store(true)
	}
	m.mu.Unlock()
	if run == nil {
		return false
	}
	cancelMainRunPendingUI(run)
	run.cancel()
	if coordinator != nil {
		go func() {
			select {
			case <-coordinator.admitted:
				m.releaseFailedSteering(sessionID, coordinator, context.Canceled)
			case <-m.ctx.Done():
			}
		}()
	}
	return true
}
