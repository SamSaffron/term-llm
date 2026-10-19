package shell

// This file holds small state accessors and overrides for callers that embed a
// Shell (for example HTTP handler tests with a fake Process). They take s.mu so
// they are safe alongside normal operation.

// CollaborationStatus reports the current collaboration state and enabled flag.
func (s *Shell) CollaborationStatus() (state CollaborationState, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.collaborationState, s.collaborationEnabled
}

// ActivityPosition reports the activity cursor, the end of recorded output, and
// whether an activity reservation is outstanding.
func (s *Shell) ActivityPosition() (cursor, end int64, reserved bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activityCursor, s.nextOffset, s.activityReservation != nil
}

// ForceCollaboration overrides the collaboration state and enabled flag without
// emitting events.
func (s *Shell) ForceCollaboration(state CollaborationState, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.collaborationState, s.collaborationEnabled = state, enabled
}

// ForceCollaborationState overrides the collaboration state and reason while
// leaving the enabled flag unchanged.
func (s *Shell) ForceCollaborationState(state CollaborationState, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.collaborationState, s.collaborationReason = state, reason
}

// MarkCollaborationReady enables collaboration as if a probe had succeeded.
func (s *Shell) MarkCollaborationReady() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shellToolAvailable = true
	s.resetActivityCursorLocked()
	s.transitionCollaborationLocked(CollaborationReady, true, "collaboration", "")
}

// DisableCollaborationWithReason transitions collaboration off, emitting an event.
func (s *Shell) DisableCollaborationWithReason(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transitionCollaborationLocked(CollaborationOff, false, "collaboration", reason)
}
