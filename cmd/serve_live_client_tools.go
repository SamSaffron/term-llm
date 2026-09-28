package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/live"
	"github.com/samsaffron/term-llm/internal/llm"
)

// Page tools in server delegation mode.
//
// A web client whose page offers WebMCP tools passes them to its own chat turns
// as passthrough functions: a call ends the run, and the page runs the tool and
// continues the response with a function_call_output. A voice delegation is a
// turn the host starts, so on its own it has neither the schemas nor anyone to
// run a call. The client therefore declares its page tools for the call —
// client_tools on the start request, replaced later through /client_tools — and
// the host offers them to every delegated turn. When a delegated run stops on
// page calls, the host publishes live.tool_calls_requested, the client runs the
// calls and posts their outputs, and the host continues the turn with them
// itself. The delegation stays a host turn from start to finish, so it keeps its
// live context, steering, progress notes, and spoken answer.

const (
	// liveClientToolsLimitBytes bounds one declared tool set. It is forwarded to
	// the model with every delegated turn, like a chat turn's own tools.
	liveClientToolsLimitBytes = 512 << 10
	liveClientToolsMax        = 128
	// liveClientToolResultLimitBytes bounds one round of page tool outputs. The
	// web UI caps each output at 100k characters, and a round can answer many
	// parallel calls, which a typed turn could send in one /v1/responses body.
	liveClientToolResultLimitBytes = 16 << 20
	// liveClientToolRoundLimit bounds consecutive page tool rounds in one
	// delegation, as the web UI bounds them within one typed turn.
	liveClientToolRoundLimit = 8
	// liveClientToolRequestHistoryLimit bounds the answered rounds kept per call
	// for idempotent replay, like liveClientDelegationHistoryLimit.
	liveClientToolRequestHistoryLimit = 64
)

// liveClientToolNamePattern is the function name alphabet providers accept.
var liveClientToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// errLivePageToolsSuperseded reports page tool results that arrived after
// another turn took the conversation: the calls they answer were already dropped.
var errLivePageToolsSuperseded = errors.New("the conversation moved on before the device tools finished, so their results were not used")

// liveStartClientTools validates the page tools a start request declares. They
// exist for the host to offer and the client to run, so a client-mode call, whose
// delegations never become host turns, has nowhere to use them.
func liveStartClientTools(mode string, raw []json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if mode == liveDelegationModeClient {
		return nil, errors.New(`client_tools is only accepted with "delegation_mode":"server"`)
	}
	return liveClientToolDefinitions(raw)
}

// liveClientToolDefinitions validates one declared page tool set: Responses
// function definitions with provider-safe, unique names. Definitions are kept
// verbatim, because the host forwards them exactly as a chat turn's own tools
// are forwarded.
func liveClientToolDefinitions(raw []json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) > liveClientToolsMax {
		return nil, fmt.Errorf("client_tools may declare at most %d tools", liveClientToolsMax)
	}
	size := 0
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		if size += len(item); size > liveClientToolsLimitBytes {
			return nil, fmt.Errorf("client_tools must be at most %d bytes", liveClientToolsLimitBytes)
		}
		name, err := liveClientToolDefinitionName(item)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, fmt.Errorf("client_tools declares %q more than once", name)
		}
		seen[name] = true
	}
	return raw, nil
}

// liveClientToolDefinitionName returns the function name one declared tool offers.
func liveClientToolDefinitionName(item json.RawMessage) (string, error) {
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(item, &generic); err != nil || generic == nil {
		return "", errors.New("client_tools entries must be function tool objects")
	}
	if strings.ToLower(strings.TrimSpace(jsonString(generic["type"]))) != "function" {
		return "", errors.New(`client_tools entries must have "type":"function"`)
	}
	spec, ok := parseRequestedFunctionTool(generic)
	if !ok || !liveClientToolNamePattern.MatchString(spec.Name) {
		return "", errors.New("client_tools names must be 1-64 letters, digits, underscores, or hyphens")
	}
	return spec.Name, nil
}

// liveClientToolNames returns the function names a validated tool set offers.
func liveClientToolNames(tools []json.RawMessage) map[string]bool {
	names := make(map[string]bool, len(tools))
	for _, item := range tools {
		if name, err := liveClientToolDefinitionName(item); err == nil {
			names[name] = true
		}
	}
	return names
}

// setClientTools replaces the page tools declared for this call.
func (l *liveSession) setClientTools(tools []json.RawMessage) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		return errLiveCallEnded
	}
	l.clientTools = tools
	l.lastActivity = time.Now()
	return nil
}

// declaredClientTools returns the page tools declared for this call.
func (l *liveSession) declaredClientTools() []json.RawMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.clientTools)
}

// liveClientToolCall is one page tool call a delegated run stopped on, as the
// client receives it.
type liveClientToolCall struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// liveClientToolResult is the client's answer to one round: an output for every
// call, in call order, or the reason it produced none.
type liveClientToolResult struct {
	Outputs []string
	Error   string
}

// digest identifies a result for replay comparison, so an answered round keeps
// 32 bytes rather than every output it carried. Each field is length-prefixed,
// so no two different results share a digest input.
func (r liveClientToolResult) digest() [sha256.Size]byte {
	hash := sha256.New()
	write := func(value string) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		hash.Write(size[:])
		hash.Write([]byte(value))
	}
	write(r.Error)
	for _, output := range r.Outputs {
		write(output)
	}
	var sum [sha256.Size]byte
	copy(sum[:], hash.Sum(nil))
	return sum
}

// liveClientToolRequest is one round of page tool calls published to the client.
// Like liveClientDelegation it outlives its waiter, so a result the client
// retries is told "duplicate" rather than "unknown". Every field is read and
// written under liveSession.mu, and calls never changes after publication.
type liveClientToolRequest struct {
	id         string
	sessionID  string
	responseID string
	deadline   time.Time
	calls      []liveClientToolCall
	done       chan struct{}
	// result is the accepted answer until its waiter takes it; digest keeps
	// identifying that answer afterwards, for replays.
	result    *liveClientToolResult
	digest    *[sha256.Size]byte
	abandoned bool
}

// abandonLocked releases the waiter without a result, because the call has ended.
func (e *liveClientToolRequest) abandonLocked() {
	if e.done == nil {
		return
	}
	e.abandoned = true
	close(e.done)
	e.done = nil
}

// beginClientToolRequest registers one round of calls and publishes it. The round
// names the session of the delegation that made it even if the call has since
// moved: the turn it continues stays in the session it started in.
func (l *liveSession) beginClientToolRequest(requestID, sessionID, responseID string, calls []liveClientToolCall, deadline ...time.Time) (<-chan struct{}, error) {
	expires := time.Now().Add(liveClientDelegationTimeout)
	if len(deadline) > 0 {
		expires = deadline[0]
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		return nil, errLiveCallEnded
	}
	if l.clientToolRequestLocked(requestID) != nil {
		return nil, errors.New("this tool call request is already known to the device")
	}
	entry := &liveClientToolRequest{
		id: requestID, sessionID: sessionID, responseID: responseID, calls: calls, deadline: expires, done: make(chan struct{}),
	}
	// A call runs one delegated turn at a time, so only the newest round can
	// have a waiter and dropping the oldest never strands one.
	l.clientToolRequests = append(l.clientToolRequests, entry)
	if len(l.clientToolRequests) > liveClientToolRequestHistoryLimit {
		l.clientToolRequests = l.clientToolRequests[1:]
	}
	l.publishClientToolRequestLocked(entry, false)
	return entry.done, nil
}

func (l *liveSession) clientToolRequestLocked(requestID string) *liveClientToolRequest {
	for _, entry := range l.clientToolRequests {
		if entry.id == requestID {
			return entry
		}
	}
	return nil
}

// publishClientToolRequestLocked emits one live.tool_calls_requested. resend
// marks a repeat, which a client that already ran the round answers again from
// its results instead of running any tool twice.
func (l *liveSession) publishClientToolRequestLocked(entry *liveClientToolRequest, resend bool) {
	l.appendEventLocked(liveEventToolCallsRequested, map[string]any{
		"session_id": entry.sessionID, "request_id": entry.id, "response_id": entry.responseID,
		"calls": entry.calls, "resend": resend, "deadline_ms": entry.deadline.UnixMilli(),
	})
}

// resendClientToolRequest republishes a round still waiting for its results.
// The event ring is bounded, so one publication is not a guaranteed delivery.
func (l *liveSession) resendClientToolRequest(requestID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if entry := l.clientToolRequestLocked(requestID); entry != nil && entry.done != nil {
		l.publishClientToolRequestLocked(entry, true)
	}
}

// clientToolRequestCalls returns the calls a round asked for, which its result
// must answer.
func (l *liveSession) clientToolRequestCalls(requestID string) ([]liveClientToolCall, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.clientToolRequestLocked(requestID)
	if entry == nil {
		return nil, false
	}
	return entry.calls, true
}

// completeClientToolRequest resolves one posted result. As for delegations, the
// first writer wins, and the decision and the hand-off share one critical
// section; only the digest is computed outside it.
func (l *liveSession) completeClientToolRequest(requestID string, result liveClientToolResult) liveDelegationResultOutcome {
	digest := result.digest()
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.clientToolRequestLocked(requestID)
	switch {
	case entry == nil:
		return liveDelegationResultUnknown
	case entry.digest != nil && *entry.digest == digest:
		return liveDelegationResultDuplicate
	case entry.digest != nil, entry.done == nil:
		// It contradicts the accepted answer, or nobody is waiting any more:
		// absorbing it would discard work the client believes it delivered.
		return liveDelegationResultConflict
	}
	accepted := result
	entry.result, entry.digest = &accepted, &digest
	close(entry.done)
	entry.done = nil
	l.lastActivity = time.Now()
	return liveDelegationResultAccepted
}

// finishClientToolRequest retires the waiter for requestID and reports how the
// round resolved, handing over the accepted result. The entry and the result's
// digest stay for idempotent replay.
func (l *liveSession) finishClientToolRequest(requestID string) (*liveClientToolResult, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.clientToolRequestLocked(requestID)
	if entry == nil {
		return nil, false
	}
	result := entry.result
	entry.done, entry.result = nil, nil
	return result, entry.abandoned
}

// cancelClientToolRequest tells a connected page to stop executing a timed-out
// or superseded round. The deadline on the request also fences reconnects that
// miss this event from starting further side effects after expiry.
func (l *liveSession) cancelClientToolRequest(requestID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appendEventLocked(liveEventToolCallsCancelled, map[string]any{"request_id": requestID})
}

// liveClientToolsRequest replaces the page tools declared for a call.
type liveClientToolsRequest struct {
	Tools []json.RawMessage `json:"tools"`
}

// handleLiveClientTools replaces the page tools a server-mode call offers its
// delegated turns. A client keeps the declaration current as its page's tools,
// or the ones it allows in the bound conversation, change. A delegation already
// running keeps the tools it started with.
func (s *serveServer) handleLiveClientTools(w http.ResponseWriter, r *http.Request, liveID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	record, ok := s.lookupLiveSession(liveID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "live session not found")
		return
	}
	if record.delegationMode == liveDelegationModeClient {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", `client_tools is only accepted with "delegation_mode":"server"`)
		return
	}
	var request liveClientToolsRequest
	if !decodeLiveClientBody(w, r, liveClientToolsLimitBytes+liveTextLimitBytes, "the declared tools are too large", &request) {
		return
	}
	tools, err := liveClientToolDefinitions(request.Tools)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := record.setClientTools(tools); err != nil {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tools": len(tools)})
}

// liveToolCallsResultRequest is one round of page tool results from the client:
// an output for every requested call, or the reason there are none.
type liveToolCallsResultRequest struct {
	Outputs []liveToolCallOutput `json:"outputs"`
	Error   string               `json:"error"`
}

type liveToolCallOutput struct {
	CallID string  `json:"call_id"`
	Output *string `json:"output"`
}

// handleLiveToolCallsResult accepts the client's results for one round of page
// tool calls. Authorization is the serve bearer token, as for every live route;
// the request id is correlation, not a capability. Replays follow the delegation
// result contract: an identical one is a duplicate, a different one a conflict.
func (s *serveServer) handleLiveToolCallsResult(w http.ResponseWriter, r *http.Request, liveID, requestID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	record, ok := s.lookupLiveSession(liveID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "live session not found")
		return
	}
	requestID = strings.TrimSpace(requestID)
	calls, ok := record.clientToolRequestCalls(requestID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "live tool call request not found")
		return
	}
	var request liveToolCallsResultRequest
	if !decodeLiveClientBody(w, r, liveClientToolResultLimitBytes, "the tool results are too large", &request) {
		return
	}
	result, err := liveClientToolResultFor(request, calls)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	switch record.completeClientToolRequest(requestID, result) {
	case liveDelegationResultAccepted:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case liveDelegationResultDuplicate:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "duplicate": true})
	case liveDelegationResultConflict:
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "this tool call request is no longer accepting that result")
	default:
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "live tool call request not found")
	}
}

// decodeLiveClientBody decodes one bounded JSON body, answering 413 or 400
// itself, and reports whether the caller may continue.
func decodeLiveClientBody(w http.ResponseWriter, r *http.Request, limit int64, tooLarge string, into any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(into)
	if err == nil {
		return true
	}
	var overflow *http.MaxBytesError
	if errors.As(err, &overflow) {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", tooLarge)
		return false
	}
	writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid request body: "+err.Error())
	return false
}

// liveClientToolResultFor validates one round's result against the calls it
// answers. Outputs are kept verbatim — a tool's output is data, and an empty one
// is a legitimate answer — and aligned to call order, so a replay that lists
// them in another order is still the same answer.
func liveClientToolResultFor(request liveToolCallsResultRequest, calls []liveClientToolCall) (liveClientToolResult, error) {
	failure := strings.TrimSpace(request.Error)
	switch {
	case failure != "" && len(request.Outputs) > 0, failure == "" && len(request.Outputs) == 0:
		return liveClientToolResult{}, errors.New("provide exactly one of outputs or error")
	case failure != "":
		// Published verbatim as the delegation's failure text, so it is capped
		// like a delegation result's error.
		if len(failure) > liveDelegationFailureLimitBytes {
			failure = strings.ToValidUTF8(failure[:liveDelegationFailureLimitBytes], "") + "…"
		}
		return liveClientToolResult{Error: failure}, nil
	}
	byID := make(map[string]string, len(request.Outputs))
	for _, output := range request.Outputs {
		id := strings.TrimSpace(output.CallID)
		if _, duplicate := byID[id]; duplicate || id == "" || output.Output == nil {
			return liveClientToolResult{}, errors.New("each output needs a distinct call_id and an output")
		}
		byID[id] = *output.Output
	}
	if len(byID) != len(calls) {
		return liveClientToolResult{}, errors.New("outputs must answer exactly the requested calls")
	}
	outputs := make([]string, len(calls))
	for i, call := range calls {
		output, ok := byID[call.CallID]
		if !ok {
			return liveClientToolResult{}, errors.New("outputs must answer exactly the requested calls")
		}
		outputs[i] = output
	}
	return liveClientToolResult{Outputs: outputs}, nil
}

// pageTools returns the page tools the call's client declared, if any.
func (d *serveLiveDelegator) pageTools() []json.RawMessage {
	if d.live == nil {
		return nil
	}
	return d.live.declaredClientTools()
}

// streamDelegation speaks a delegated run and, each time a run stops on page
// tool calls, has the client run them and speaks the run that continues with
// their results. One progress tracker spans every round, so the voice model
// hears one account of the work instead of a restart per run.
func (d *serveLiveDelegator) streamDelegation(ctx context.Context, sessionID string, run *responseRun, tools []json.RawMessage, emit func(live.DelegationChunk)) error {
	progress := newLiveProgress(emit, time.Now)
	names := liveClientToolNames(tools)
	for round := 0; ; round++ {
		if err := d.streamRunProgress(ctx, run, emit, progress); err != nil {
			return err
		}
		calls := pendingPageToolCalls(run, names)
		if len(calls) == 0 {
			return nil
		}
		if round == liveClientToolRoundLimit {
			return fmt.Errorf("stopped after %d rounds of device tool calls", liveClientToolRoundLimit)
		}
		results, err := d.runPageTools(ctx, sessionID, run, calls, progress)
		if err != nil {
			return err
		}
		if run, err = d.continueWithPageTools(ctx, sessionID, run, results, tools); err != nil {
			return err
		}
	}
}

// pendingPageToolCalls returns the declared page tool calls a completed run
// stopped on, in call order. Any other passthrough call is left unanswered, as
// the web UI leaves it, and the next turn drops it.
func pendingPageToolCalls(run *responseRun, names map[string]bool) []liveClientToolCall {
	if len(names) == 0 {
		return nil
	}
	run.mu.Lock()
	pending := run.pendingClientCalls
	run.mu.Unlock()
	var calls []liveClientToolCall
	for _, call := range pending {
		id, name := strings.TrimSpace(stringValue(call["call_id"])), stringValue(call["name"])
		if id == "" || !names[name] {
			continue
		}
		arguments := stringValue(call["arguments"])
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		calls = append(calls, liveClientToolCall{CallID: id, Name: name, Arguments: arguments})
	}
	return calls
}

// runPageTools has the client run one round of page tool calls and returns the
// tool results that continue the turn. Progress shows the round as tools
// running on the user's device, so a slow device reads as work, not silence.
func (d *serveLiveDelegator) runPageTools(ctx context.Context, sessionID string, run *responseRun, calls []liveClientToolCall, progress *liveProgress) ([]llm.Message, error) {
	if d.live == nil {
		return nil, errors.New("the live call is unavailable")
	}
	requestID := "tools_" + randomSuffix()
	limit := d.toolTimeout
	if limit <= 0 {
		limit = liveClientDelegationTimeout
	}
	done, err := d.live.beginClientToolRequest(requestID, sessionID, run.id, calls, time.Now().Add(limit))
	if err != nil {
		return nil, err
	}
	started := time.Now()
	observePageTools(progress, "response.tool_exec.start", calls, map[string]any{"started_at": started.UnixMilli()})
	progress.tick()
	waitErr := d.awaitPageTools(ctx, done, requestID, progress)
	result, abandoned := d.live.finishClientToolRequest(requestID)
	if result == nil && !abandoned {
		d.live.cancelClientToolRequest(requestID)
	}
	observePageTools(progress, "response.tool_exec.end", calls, map[string]any{
		"success": result != nil && result.Error == "", "duration_ms": time.Since(started).Milliseconds(),
	})
	switch {
	case result != nil && result.Error != "":
		return nil, errors.New(result.Error)
	case result != nil:
		messages := make([]llm.Message, len(calls))
		for i, call := range calls {
			messages[i] = llm.ToolResultMessage(call.CallID, call.Name, result.Outputs[i], nil)
		}
		return messages, nil
	case abandoned:
		return nil, errors.New("the live call ended before the device ran its tools")
	case waitErr != nil:
		return nil, waitErr
	}
	// done only closes with a result or an abandonment recorded, so this is
	// unreachable — but continuing here would answer the calls with nothing.
	return nil, errors.New("the device tools ended without an answer")
}

// observePageTools feeds progress the tool events a round would have had if the
// engine had run it, so status notes name the tools the device is running.
func observePageTools(progress *liveProgress, event string, calls []liveClientToolCall, fields map[string]any) {
	for _, call := range calls {
		payload := map[string]any{"call_id": call.CallID, "tool_name": call.Name, "tool_info": "on the user's device"}
		for key, value := range fields {
			payload[key] = value
		}
		if data, err := json.Marshal(payload); err == nil {
			progress.observe(responseRunEvent{Event: event, Data: data})
		}
	}
}

// awaitPageTools blocks until the round resolves, republishing the request and
// ticking progress meanwhile, and reports why the wait ended when it was not the
// client answering.
func (d *serveLiveDelegator) awaitPageTools(ctx context.Context, done <-chan struct{}, requestID string, progress *liveProgress) error {
	interval, limit := d.toolResendInterval, d.toolTimeout
	if interval <= 0 {
		interval = liveClientDelegationResendInterval
	}
	if limit <= 0 {
		limit = liveClientDelegationTimeout
	}
	resend := time.NewTicker(interval)
	defer resend.Stop()
	tick := time.NewTicker(liveProgressTick)
	defer tick.Stop()
	timeout := time.NewTimer(limit)
	defer timeout.Stop()
	for {
		select {
		case <-done:
			return nil
		case <-resend.C:
			d.live.resendClientToolRequest(requestID)
		case <-tick.C:
			progress.tick()
		case <-timeout.C:
			return errors.New("the device did not run its tools in time")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// continueWithPageTools starts the run that carries a round's results back to
// the model, once the run that stopped on the calls has settled. It refuses when
// another turn took the session meanwhile, because the results would then answer
// calls that turn has already dropped.
func (d *serveLiveDelegator) continueWithPageTools(ctx context.Context, sessionID string, stopped *responseRun, results []llm.Message, tools []json.RawMessage) (*responseRun, error) {
	if stopped.settled != nil {
		select {
		case <-stopped.settled:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	runtime, _, err := d.server.runtimeForRequest(ctx, sessionID)
	if errors.Is(err, errServeSessionBusy) {
		return nil, errLivePageToolsSuperseded
	}
	if err != nil {
		return nil, err
	}
	stopped.mu.Lock()
	stoppedID := stopped.continuationResponseID
	stopped.mu.Unlock()
	previous := d.previousResponseID(ctx, runtime, sessionID)
	if stoppedID != "" && previous != "" && previous != stoppedID {
		return nil, errLivePageToolsSuperseded
	}
	run, err := d.startDelegatedRun(runtime, sessionID, previous, results, tools, previous)
	if errors.Is(err, errServeSessionBusy) {
		return nil, errLivePageToolsSuperseded
	}
	return run, err
}
