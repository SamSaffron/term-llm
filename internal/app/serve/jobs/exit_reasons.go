package jobs

import corejobs "github.com/samsaffron/term-llm/internal/jobs"

const (
	exitReasonNatural    = corejobs.ExitReasonNatural
	exitReasonMaxTurns   = corejobs.ExitReasonMaxTurns
	exitReasonTimeout    = corejobs.ExitReasonTimeout
	exitReasonCancelled  = corejobs.ExitReasonCancelled
	exitReasonException  = corejobs.ExitReasonException
	exitReasonEmpty      = corejobs.ExitReasonEmpty
	exitReasonWorkerLost = corejobs.ExitReasonWorkerLost
)
