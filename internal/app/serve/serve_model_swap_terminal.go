package serve

// terminalResponse reports the retained runtime, which may differ from the
// attempted target after a failed or cancelled handover.
func (exec *responseModelSwapExecution) terminalResponse(id, model string, created int64, status string) map[string]any {
	provider, effort := exec.plan.requestedProvider, exec.plan.requestedEffort
	if exec.rolledBack {
		provider, model, effort = exec.plan.previousProvider, exec.plan.previousModel, exec.plan.previousEffort
	}
	return map[string]any{
		"id": id, "object": "response", "created": created, "status": status,
		"provider": provider, "model": model, "reasoning_effort": effort,
	}
}
