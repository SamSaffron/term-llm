package cmd

import (
	"context"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// childRunObserver is installed only by serve. CLI children keep the direct
// runner path, while hosted children are registered before their runtime is
// prepared and execute through the normal response-run machinery.
type childRunObserver interface {
	ChildRunStarted(info childRunInfo) childRunSession
}

type childRunInfo struct {
	ChildSessionID  string
	ParentSessionID string
	CallID          string
	Agent           string
	Prompt          string
}

type childRunSession interface {
	Execute(ctx context.Context, env *cmdRunEnvironment, onEvent func(llm.Event) error) (serveRunResult, error)
	AskUser(ctx context.Context, questions []tools.AskUserQuestion) ([]tools.AskUserAnswer, error)
	AskUserAvailable() bool
	Finish(status session.SessionStatus, err error)
	Outcome() childRunOutcome
}

type childRunOutcome struct {
	Interventions   []string
	Disposition     string
	CancelledByUser bool
}
