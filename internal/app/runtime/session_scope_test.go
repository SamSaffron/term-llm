package runtime

import (
	"errors"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/session/scopedb"
)

func TestCheckLockedSessionScope(t *testing.T) {
	locked := &session.Session{Agent: "private", CWD: "/work/a", ModelPolicy: modelpolicy.Policy{LockSession: true}}
	for _, tc := range []struct {
		name, agent, cwd string
		sess             *session.Session
		refuse           bool
	}{
		{name: "reuse", sess: locked},
		{name: "same identity", sess: locked, agent: "private", cwd: "/work/a/"},
		{name: "other agent", sess: locked, agent: "other", refuse: true},
		{name: "other workspace", sess: locked, cwd: "/work/b", refuse: true},
		{name: "unlocked retarget", sess: &session.Session{Agent: "private", CWD: "/work/a"}, agent: "other", cwd: "/x"},
		{name: "no session", agent: "other"},
	} {
		err := CheckLockedSessionScope(tc.sess, tc.agent, tc.cwd)
		if got := errors.Is(err, scopedb.ErrScope); got != tc.refuse {
			t.Errorf("%s: err=%v, refuse=%v", tc.name, err, tc.refuse)
		}
	}
}

func TestRefuseLockedDirectRun(t *testing.T) {
	if err := RefuseLockedDirectRun(nil, &agents.Agent{Name: "plain"}, "ask"); err != nil {
		t.Fatalf("ordinary agent refused: %v", err)
	}
	if err := RefuseLockedDirectRun(nil, &agents.Agent{Name: "p", LockSession: true}, "ask"); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("locked agent = %v", err)
	}
	if err := RefuseLockedDirectRun(&session.Session{ModelPolicy: modelpolicy.Policy{LockSession: true}}, nil, "ask"); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("locked session = %v", err)
	}
}

func TestSelectRunModelLocksEffectivePolicy(t *testing.T) {
	cfg := testSelectConfig()
	agent := &agents.Agent{Name: "private", Provider: "debug", Model: "allowed", AllowedModels: []string{"debug:allowed"}, LockSession: true}
	_, policy, err := SelectRunModel(cfg, ModelSelectionInput{Agent: agent})
	if err != nil || !policy.LockSession || !policy.Restricted() {
		t.Fatalf("SelectRunModel policy=%+v err=%v", policy, err)
	}
	if _, _, err := SelectRunModel(cfg, ModelSelectionInput{Agent: agent, ProviderFlag: "other:denied"}); err == nil {
		t.Fatal("locked agent accepted disallowed override")
	}
	plain := &agents.Agent{Name: "plain", Provider: "debug", Model: "allowed"}
	if _, policy, err := SelectRunModel(cfg, ModelSelectionInput{Agent: plain}); err != nil || policy.LockSession || policy.Restricted() {
		t.Fatalf("ordinary policy=%+v err=%v", policy, err)
	}
}

func testSelectConfig() *config.Config {
	return &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{
		"debug": {Model: "allowed"}, "other": {Model: "denied"},
	}}
}

func TestRefuseLockedAux(t *testing.T) {
	if err := RefuseLockedAux(modelpolicy.Policy{}, "side questions"); err != nil {
		t.Fatalf("ordinary = %v", err)
	}
	if err := RefuseLockedAux(modelpolicy.Policy{LockSession: true}, "cross-model swaps"); !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("locked = %v", err)
	}
}
