package liveclassify

// GateConfig contains the confidence thresholds used by the pure Phase 1 gate.
type GateConfig struct {
	Status        float64
	NewSession    float64
	SwitchSession float64
	SteerNow      float64
	Side          float64
}

// Gate returns the label Phase 1 may act on. Unsupported future labels,
// low-confidence decisions, and mixed navigate-and-work requests all fail open
// to steer so the original utterance reaches the bound session unchanged.
func Gate(decision Decision, cfg GateConfig) string {
	confidence := decision.Probabilities[decision.Intent]
	switch decision.Intent {
	case IntentStatus:
		if confidence >= cfg.Status {
			return IntentStatus
		}
	case IntentNewSession:
		if confidence >= cfg.NewSession && decision.AlsoRequest < 0.5 {
			return IntentNewSession
		}
	case IntentSwitchSession:
		if confidence >= cfg.SwitchSession && decision.AlsoRequest < 0.5 {
			return IntentSwitchSession
		}
	case IntentSteer, IntentSteerNow, IntentSide:
		// steer_now and side are classified and logged in Phase 1, but deliberately
		// have no acting path until their Phase 2 ownership contracts exist.
		return IntentSteer
	}
	return IntentSteer
}
