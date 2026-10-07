package modelpolicy

import (
	"errors"
	"strings"
	"testing"
)

func TestPolicyIntersection(t *testing.T) {
	p := Policy{}.With("boss", []string{"debug:*"}).With("child", []string{"debug:allowed"})
	if err := p.Check("child", "debug", "allowed"); err != nil {
		t.Fatal(err)
	}
	if p.Check("child", "debug", "denied") == nil {
		t.Fatal("own restriction ignored")
	}
	var denied *DeniedError
	if err := p.Check("child", "other", "allowed"); !errors.As(err, &denied) || denied.RuleAgent != "boss" || !denied.Inherited || !strings.Contains(err.Error(), "parent agent \"boss\"") {
		t.Fatalf("inherited denial: %v", err)
	}
	if (Policy{}).Check("any", "other", "anything") != nil {
		t.Fatal("empty policy restricted")
	}
	if !p.Append(Policy{}.With("grandchild", []string{"debug:allowed"})).Restricted() {
		t.Fatal("append lost rules")
	}
}

func TestPolicyWithSkipsIdenticalRule(t *testing.T) {
	p := Policy{}.With("boss", []string{"debug:*"})
	if got := p.With("boss", []string{"debug:*"}).Append(p); len(got.Rules) != 1 {
		t.Fatalf("rules = %+v, want one deduplicated rule", got.Rules)
	}
	if got := p.With("boss", []string{"debug:allowed"}); len(got.Rules) != 2 {
		t.Fatalf("rules = %+v, want distinct rules kept", got.Rules)
	}
}
