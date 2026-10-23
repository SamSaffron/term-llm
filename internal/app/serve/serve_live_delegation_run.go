package serve

import (
	"context"
	"encoding/json"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// startDelegatedRun starts one host-owned turn in sessionID: the delegation
// itself, or a continuation carrying page tool results. Both run with the call's
// live context and offer the same page tools.
func (d *serveLiveDelegator) startDelegatedRun(runtime *serveRuntime, sessionID, previousResponseID string, messages []core.Message, tools []json.RawMessage, expectedLatest ...string) (*responseRun, error) {
	s := d.server
	req := s.buildResponsesLLMRequest(responsesCreateRequest{Model: runtime.defaultModel, Tools: tools}, runtime, sessionID, true)
	options := startResponseRunOptions{
		previousResponseID: previousResponseID, uiSession: true, live: d.live,
		notificationTurn: s.notificationTurnAdmission(context.Background(), messages, previousResponseID),
		clientToolRunner: d.inlinePageToolRunner(runtime, sessionID, tools),
	}
	if len(expectedLatest) > 0 {
		expected := expectedLatest[0]
		options.admissionCheck = func() bool {
			return d.previousResponseID(context.Background(), runtime, sessionID) == expected
		}
	}
	options.runtimeSetup = func(req *core.Request) error {
		if d.live != nil {
			runtime.liveContext = d.live.executionContextFor(sessionID)
		}
		return nil
	}
	run, err := s.startResponseRun(runtime, true, false, messages, req, sessionID, options)
	if err != nil {
		return nil, err
	}
	if d.live != nil {
		d.live.touch()
	}
	return run, nil
}
