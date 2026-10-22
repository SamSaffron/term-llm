package serve

import (
	"context"
	"errors"
	"io"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func (rt *serveRuntime) consumeRunStream(runCtx, persistCtx context.Context, stateful, persisted bool, req core.Request, onEvent func(core.Event) error) (_ serveRunResult, streamErr error, _ bool) {
	result := serveRunResult{}
	rt.startStats(req.Model)
	defer func() { rt.finishRuntimeStream(runCtx, streamErr) }()
	stream, err := rt.engine.Stream(runCtx, req)
	if err != nil {
		rt.persistRunError(persistCtx, req.SessionID, persisted, err)
		return serveRunResult{}, err, false
	}
	defer stream.Close()

	sawDone := false
	for {
		event, recvErr := stream.Recv()
		if recvErr == io.EOF {
			return result, nil, false
		}
		// Once EventDone has been delivered the turn is complete. A request
		// context cancelled afterwards (for example a client that disconnects on
		// the final event) can still surface from Recv while the engine tears
		// down; that is the end of the stream, not a failed turn.
		if recvErr != nil && sawDone && runCtx.Err() != nil && (errors.Is(recvErr, context.Canceled) || errors.Is(recvErr, context.DeadlineExceeded)) {
			return result, nil, false
		}
		if recvErr != nil {
			rt.persistRunError(persistCtx, req.SessionID, persisted, recvErr)
			return result, recvErr, false
		}
		if event.Type == core.EventDone {
			sawDone = true
		}
		rt.recordRuntimeEvent(event)
		if onEvent != nil {
			if err := onEvent(event); err != nil {
				rt.persistRunError(persistCtx, req.SessionID, persisted, err)
				return result, err, false
			}
		}
		rt.updateInterruptFromEvent(event)
		suspensionHandled, err := rt.accumulateRunEvent(persistCtx, stateful, persisted, req.SessionID, event, &result)
		if err != nil {
			return result, err, suspensionHandled
		}
	}
}
