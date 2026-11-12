package modelpolicy

import (
	"slices"

	"github.com/samsaffron/term-llm/internal/config"
)

// lockSnapshotSuffix labels a rule frozen from provider allowlists at lock time.
const lockSnapshotSuffix = " (provider allowlist at lock)"

// Lock returns the policy persisted for a locked conversation: the complete
// effective chain, marked locked, plus each rule narrowed by the provider
// allowlists in force at admission. Rules only intersect, so a later widening
// of an agent definition or of provider configuration cannot widen the
// conversation; narrowing still applies because live checks keep using the
// current configuration too.
func Lock(cfg *config.Config, effective Policy) Policy {
	out := Policy{Rules: append([]Rule(nil), effective.Rules...), LockSession: true}
	if cfg == nil {
		return out
	}
	for _, rule := range effective.Rules {
		narrowed := deniedAllowedForConfig(cfg, rule.Allowed)
		if !slices.Equal(narrowed, rule.Allowed) {
			out = out.With(rule.Agent+lockSnapshotSuffix, narrowed)
		}
	}
	return out
}

// Locked reports whether the policy fixes conversation identity.
func (p Policy) Locked() bool { return p.LockSession }
