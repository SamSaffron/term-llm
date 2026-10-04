package tools

// AgentRunAdmissionError means a child continuation failed before execution
// began. The previous transcript and terminal status remain safe to retry.
type AgentRunAdmissionError struct{ Err error }

func (e *AgentRunAdmissionError) Error() string { return e.Err.Error() }
func (e *AgentRunAdmissionError) Unwrap() error { return e.Err }
