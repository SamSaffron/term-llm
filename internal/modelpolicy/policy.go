// Package modelpolicy enforces the ordered intersection of agent model rules.
package modelpolicy

import (
	"fmt"
	"slices"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

type Rule struct {
	Agent   string   `json:"agent"`
	Allowed []string `json:"allowed"`
}

type Policy struct {
	Rules []Rule `json:"rules,omitempty"`
}

func (p Policy) With(agent string, allowed []string) Policy {
	if len(allowed) == 0 || p.hasRule(agent, allowed) {
		// Rules intersect, so repeating an identical rule (for example a live
		// parent snapshot over the same persisted ancestry) adds nothing.
		return p
	}
	next := Policy{Rules: append([]Rule(nil), p.Rules...)}
	next.Rules = append(next.Rules, Rule{Agent: agent, Allowed: append([]string(nil), allowed...)})
	return next
}

func (p Policy) Append(other Policy) Policy {
	out := Policy{Rules: append([]Rule(nil), p.Rules...)}
	for _, rule := range other.Rules {
		out = out.With(rule.Agent, rule.Allowed)
	}
	return out
}

func (p Policy) hasRule(agent string, allowed []string) bool {
	for _, rule := range p.Rules {
		if rule.Agent == agent && slices.Equal(rule.Allowed, allowed) {
			return true
		}
	}
	return false
}

func (p Policy) Restricted() bool                   { return len(p.Rules) > 0 }
func (p Policy) Allows(provider, model string) bool { return p.Check("", provider, model) == nil }

func (p Policy) Check(agent, provider, model string) error {
	return p.CheckWithConfig(nil, agent, provider, model)
}

func (p Policy) CheckWithConfig(cfg *config.Config, agent, provider, model string) error {
	for i, rule := range p.Rules {
		ok := false
		for _, entry := range rule.Allowed {
			key, allowed, found := strings.Cut(entry, ":")
			if found && provider == key && (allowed == "*" || llm.IsModelOrEffortVariantForConfig(cfg, provider, model, allowed)) {
				ok = true
				break
			}
		}
		if !ok {
			return &DeniedError{Agent: agent, Selected: provider + ":" + model, RuleAgent: rule.Agent, Allowed: append([]string(nil), rule.Allowed...), Inherited: i < len(p.Rules)-1 || rule.Agent != agent}
		}
	}
	return nil
}

type DeniedError struct {
	Agent, Selected, RuleAgent string
	Allowed                    []string
	Inherited                  bool
}

func (e *DeniedError) Error() string {
	if e.Inherited {
		return fmt.Sprintf("model %q is not allowed for agent %q: parent agent %q allows only %s", e.Selected, e.Agent, e.RuleAgent, strings.Join(e.Allowed, ", "))
	}
	return fmt.Sprintf("model %q is not allowed for agent %q (allowed: %s)", e.Selected, e.Agent, strings.Join(e.Allowed, ", "))
}

// ParentModel captures the live model identity supplied at child admission.
type ParentModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}
