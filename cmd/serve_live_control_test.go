package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/live"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// controlToolNames are the call-scoped tools that belong to the routing lane and
// must never be attached to a delegated chat turn.
func controlToolNames() []string {
	return []string{tools.SessionDirectoryToolName, tools.LiveSwitchSessionToolName, tools.LiveNewSessionToolName, tools.LiveSettingsToolName}
}

// liveControlToolNames is the whole surface one routing turn is offered: the four
// call-scoped control tools and the host-local handoff.
func liveControlToolNames() []string {
	return append(controlToolNames(), livePassToWorkspaceToolName)
}

func assertNoControlToolSchemas(t *testing.T, requests []llm.Request) {
	t.Helper()
	if len(requests) == 0 {
		t.Fatal("no provider requests recorded")
	}
	for _, request := range requests {
		for _, spec := range request.Tools {
			for _, name := range controlToolNames() {
				if spec.Name == name {
					t.Fatalf("delegated turn was offered %s", name)
				}
			}
		}
	}
}

// TestLiveDelegatedTurnCarriesNoControlToolSchemas is the §4 regression for the
// removed safety net. The call-scoped tools now belong to the routing lane
// alone: a delegated chat turn must neither be offered their schemas nor have
// them executable in its engine.
func TestLiveDelegatedTurnCarriesNoControlToolSchemas(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := newTestServeServer()
	runtime, _, err := s.runtimeForRequest(ctx, "no-control-tools")
	if err != nil {
		t.Fatal(err)
	}
	runtime.provider.(*llm.MockProvider).AddTextResponse("the work is done")
	record := newLiveSession("call-no-control-tools", "no-control-tools")
	record.capabilities = live.ConfigCapabilities(config.LiveConfig{})
	delegator := &serveLiveDelegator{server: s, live: record}
	if err := delegator.Run(ctx, live.DelegationRequest{Input: "list the files"}, func(live.DelegationChunk) {}); err != nil {
		t.Fatal(err)
	}
	assertNoControlToolSchemas(t, runtime.provider.(*llm.MockProvider).RecordedRequests())
	for _, name := range controlToolNames() {
		if _, registered := runtime.engine.Tools().Get(name); registered {
			t.Fatalf("%s stayed executable in a delegated turn", name)
		}
	}
	// A registered schema was never what granted authority, so the absence of the
	// schema is not by itself proof: an ordinary turn's session id must still be
	// refused by the tool itself.
	forged := llm.ContextWithSessionID(context.Background(), "no-control-tools")
	if _, err := (&tools.SessionDirectoryTool{}).Execute(forged, json.RawMessage(`{}`)); err == nil {
		t.Fatal("a delegated turn could execute session_directory")
	}
}

// TestLiveResumedRunCarriesNoControlToolSchemas is the same claim for the
// suspension path, which is where the removed runtimeSetup attachment lived: a
// resumed run replays its request from a checkpoint, and nothing in that path may
// reintroduce the control tools — the request carries no control schema, and the
// engine has no control tool to execute even if a replayed request named one.
func TestLiveResumedRunCarriesNoControlToolSchemas(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv := &serveServer{responseRuns: newServeResponseRunManager()}
	srv.sessionMgr = newServeSessionManager(time.Hour, 10, nil)
	srv.runtimeFactory = func(_ context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
		mock := llm.NewMockProvider(request.Provider)
		runtime := &serveRuntime{provider: mock, providerKey: request.Provider, engine: llm.NewEngine(mock, nil), defaultModel: "mock-model"}
		runtime.Touch()
		return runtime, nil
	}
	t.Cleanup(func() { srv.responseRuns.Close(); srv.sessionMgr.Close() })
	saved := webReloadFixture(t, "live_resume_control", "mock")
	runtime, _, err := srv.runtimeForProviderModelRequest(ctx, saved.View.SessionID, saved.Provider, saved.Engine.Request.Model)
	if err != nil {
		t.Fatal(err)
	}
	runtime.provider.(*llm.MockProvider).AddTextResponse("resumed answer")
	record := newLiveSession("call-resume-control", saved.View.SessionID)
	record.capabilities = live.ConfigCapabilities(config.LiveConfig{})
	if err := srv.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	// The fixture's checkpoint carries no tools; the resumed request is rebuilt from
	// it, so anything that reattaches control tools would be visible here.
	for _, name := range controlToolNames() {
		if _, registered := runtime.engine.Tools().Get(name); registered {
			t.Fatalf("%s stayed executable for a resumed live run", name)
		}
	}
	request := saved.Engine.Request
	request.Resume = saved.Engine
	options := startResponseRunOptions{resume: &saved, uiSession: true, live: record, previousResponseID: saved.View.PreviousResponseID}
	options.runtimeSetup = func(req *llm.Request) error {
		runtime.liveContext = record.executionContextFor(saved.View.SessionID)
		return nil
	}
	run, err := srv.startResponseRun(runtime, saved.Stateful, false, nil, request, saved.View.SessionID, options)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-run.settled:
	case <-ctx.Done():
		t.Fatal("resumed run did not settle")
	}
	assertNoControlToolSchemas(t, runtime.provider.(*llm.MockProvider).RecordedRequests())
	for _, name := range controlToolNames() {
		if _, registered := runtime.engine.Tools().Get(name); registered {
			t.Fatalf("%s was re-registered by a resumed live run", name)
		}
	}
}

// controlLaneHarness is one live call with a real controller, the production router,
// and a scripted model for the routing turn, so a spoken request travels the whole
// path it takes in production.
type controlLaneHarness struct {
	server     *serveServer
	store      *session.SQLiteStore
	record     *liveSession
	provider   *stubLiveSession
	controller *live.Controller
	delegator  *recordingLiveDelegator
	// agent is the provider the router's throwaway engine runs against. Every
	// assertion about what the router was asked and told reads its recorded requests.
	agent *llm.MockProvider
}

// recordingLiveDelegator proves the session lane was used only when it should be:
// a request the router handles may never become a chat turn.
type recordingLiveDelegator struct {
	mu      sync.Mutex
	calls   int
	steered int
	seen    []string
}

func (d *recordingLiveDelegator) Run(_ context.Context, request live.DelegationRequest, _ func(live.DelegationChunk)) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	d.seen = append(d.seen, request.Input)
	return nil
}

func (d *recordingLiveDelegator) Steer(context.Context, live.DelegationRequest) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.steered++
	// No task is running in these tests, so no request is ever admitted as
	// guidance and the run loop starts an ordinary Run instead.
	return false, nil
}

func (d *recordingLiveDelegator) counts() (runs, steers int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls, d.steered
}

// inputs returns what the session lane was asked to run, in order.
func (d *recordingLiveDelegator) inputs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

// assertSessionLaneUntouched fails when any request reached the delegated path.
func (d *recordingLiveDelegator) assertSessionLaneUntouched(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.calls != 0 || d.steered != 0 {
		t.Fatalf("a handled request entered the session lane: runs=%d steers=%d inputs=%v", d.calls, d.steered, d.seen)
	}
}

func newControlLaneHarness(t *testing.T, sourceID string) *controlLaneHarness {
	t.Helper()
	ctx := context.Background()
	store := newSessionDirectoryTestStore(t)
	createDirectorySession(t, store, &session.Session{ID: sourceID, GeneratedShortTitle: "Voice work"})
	addDirectoryMessage(t, store, sourceID, "the work in progress")
	srv := newTestServeServer()
	// The harness drives the router by hand, so it has to declare the same opt-in the
	// production wiring reads: the tool authority a routing turn gets is installed
	// only while live.control_plane is on, whatever built the router.
	srv.cfgRef = &config.Config{Live: config.LiveConfig{ControlPlane: true}}
	// The directory and the transcript assertions read the same store the host
	// server uses, so a message the routing lane wrote would be visible here.
	srv.store = store
	agent := llm.NewMockProvider("mock-agent")
	srv.liveControlProviderFactory = func(string) (llm.Provider, error) { return agent, nil }
	provider := &stubLiveSession{events: make(chan live.Event, 8), closed: make(chan struct{})}
	record := newLiveSession("call-control-lane", sourceID)
	record.capabilities = live.ConfigCapabilities(config.LiveConfig{})
	record.providerSession = provider
	record.voiceSession = &liveVoiceControlStub{voice: "cove"}
	if err := srv.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	delegator := &recordingLiveDelegator{}
	controller := live.NewController(live.ControllerOptions{
		Session:       provider,
		Delegator:     delegator,
		Router:        &serveLiveControlExecutor{server: srv, live: record},
		Observer:      record.observe,
		FlushInterval: 5 * time.Millisecond,
		BusyRetry:     5 * time.Millisecond,
		BusyRetries:   2,
	})
	controller.Start(ctx)
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = controller.Close(closeCtx)
	})
	return &controlLaneHarness{server: srv, store: store, record: record, provider: provider, controller: controller, delegator: delegator, agent: agent}
}

// commentary waits for the controller to answer the request and returns the text the
// voice model heard.
func (h *controlLaneHarness) commentary(t *testing.T) string {
	t.Helper()
	waitForLiveCondition(t, "the answer", func() bool {
		h.provider.mu.Lock()
		defer h.provider.mu.Unlock()
		return len(h.provider.delegated) > 0
	})
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	var out strings.Builder
	for _, chunk := range h.provider.delegated {
		if chunk.Channel != live.ChannelCommentary {
			t.Fatalf("the answer used the %s channel", chunk.Channel)
		}
		out.WriteString(chunk.Text)
	}
	return out.String()
}

// answer waits for the request to settle and then reports which lane settled it. A
// request the session lane ran is the production failure these tests exist for, so it
// is named as such instead of arriving as a bare timeout: the router was supposed to
// read the request before anything could become a chat turn in the bound session.
func (h *controlLaneHarness) answer(t *testing.T) string {
	t.Helper()
	waitForLiveCondition(t, "the request to be handled", func() bool {
		h.provider.mu.Lock()
		answered := len(h.provider.delegated) > 0
		h.provider.mu.Unlock()
		return answered || len(h.delegator.inputs()) > 0
	})
	if inputs := h.delegator.inputs(); len(inputs) > 0 {
		t.Fatalf("the request was not handled by the router; the session lane ran it as work: %q", inputs)
	}
	return h.commentary(t)
}

func (h *controlLaneHarness) messageCount(t *testing.T, sessionID string) int {
	t.Helper()
	messages, err := h.store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(messages)
}

// delegationEvents returns the delegation updates the browser would have been
// sent, in order, exactly as the SSE payload carries them.
func (h *controlLaneHarness) delegationEvents() []map[string]any {
	h.record.mu.Lock()
	defer h.record.mu.Unlock()
	var out []map[string]any
	for _, event := range h.record.events {
		if event.Type == liveEventDelegation {
			out = append(out, event.Data)
		}
	}
	return out
}

// delegationStates renders a delegation event list as its comma-separated state
// sequence, which is the part of the browser contract these tests pin.
func delegationStates(events []map[string]any) string {
	states := make([]string, 0, len(events))
	for _, event := range events {
		states = append(states, fmt.Sprint(event["state"]))
	}
	return strings.Join(states, ",")
}

// assertNoTerminalDelegationText fails when a terminal delegation update carried a
// text field, which is how the panel's error line is populated. `running` updates
// describe the request being worked on and may name it; only the terminal states
// divide into "speak the reason" and "say nothing".
func assertNoTerminalDelegationText(t *testing.T, events []map[string]any) {
	t.Helper()
	for _, event := range events {
		switch fmt.Sprint(event["state"]) {
		case string(live.DelegationDone), string(live.DelegationRefused), string(live.DelegationFailed):
		default:
			continue
		}
		if text, ok := event["text"]; ok && fmt.Sprint(text) != "" {
			t.Fatalf("terminal delegation update published browser-facing text: %+v", event)
		}
	}
}

// touchVoice returns the call's voice control stub, which is how a test sees the
// effect of a live_settings tool call.
func (h *controlLaneHarness) voice() string {
	h.record.mu.Lock()
	defer h.record.mu.Unlock()
	if h.record.voiceSession == nil {
		return ""
	}
	return h.record.voiceSession.CurrentVoice()
}

// toolSpecNames lists the tool schemas one recorded request was offered.
func toolSpecNames(request llm.Request) []string {
	names := make([]string, 0, len(request.Tools))
	for _, spec := range request.Tools {
		names = append(names, spec.Name)
	}
	return names
}

// toolResultText returns the content of the named tool's result in a recorded
// request, which is what the router saw when it called that tool.
func toolResultText(request llm.Request, name string) string {
	for _, message := range request.Messages {
		for _, part := range message.Parts {
			if part.ToolResult != nil && part.ToolResult.Name == name {
				return part.ToolResult.Content
			}
		}
	}
	return ""
}

// firstSystemText returns the system/developer text of a recorded request.
func firstSystemText(request llm.Request) string {
	var b strings.Builder
	for _, message := range request.Messages {
		if message.Role != llm.RoleSystem && message.Role != llm.RoleDeveloper {
			continue
		}
		for _, part := range message.Parts {
			if part.Type == llm.PartText {
				b.WriteString(part.Text)
			}
		}
	}
	return b.String()
}

// TestLiveRouterEngineExposesExactlyTheControlTools pins the lane's whole reach. A
// registered schema grants no authority — each control tool still demands the
// host-built context — but the surface itself is part of the lane's contract, so both
// the registry and the engine's allowlist are asserted to hold nothing else.
func TestLiveRouterEngineExposesExactlyTheControlTools(t *testing.T) {
	engine, specs := newLiveControlEngine(llm.NewMockProvider("mock"), &workspaceHandoff{cancel: func() {}}, &controlActivity{})
	if names := toolSpecNames(llm.Request{Tools: specs}); !slices.Equal(names, liveControlToolNames()) {
		t.Fatalf("router tools = %v, want %v", names, liveControlToolNames())
	}
	allowed, present := engine.AllowedToolsFilter()
	if !present {
		t.Fatal("the router engine has no allowlist, so a tool registered later would be callable")
	}
	slices.Sort(allowed)
	want := slices.Clone(liveControlToolNames())
	slices.Sort(want)
	if !slices.Equal(allowed, want) {
		t.Fatalf("router allowlist = %v, want %v", allowed, want)
	}
	registered := make([]string, 0, len(specs))
	for _, spec := range engine.Tools().AllSpecs() {
		registered = append(registered, spec.Name)
	}
	slices.Sort(registered)
	if !slices.Equal(registered, want) {
		t.Fatalf("router registry = %v, want %v", registered, want)
	}
	// Nothing else can be executed even if a provider hallucinates the name.
	for _, name := range []string{"read_file", "shell", "spawn_agent", tools.HubDelegateToolName} {
		if _, ok := engine.Tools().Get(name); ok {
			t.Fatalf("the routing turn can reach %s", name)
		}
	}
}

// TestLiveRouterDoesNotTouchTheChatSession is the core regression test for the whole
// feature: a session-management request is answered by the router — one cheap model
// turn against a throwaway engine — with no chat turn, no run in the bound session,
// and nothing written to its durable transcript.
func TestLiveRouterDoesNotTouchTheChatSession(t *testing.T) {
	h := newControlLaneHarness(t, "control-source")
	// Idle reaping is driven by lastActivity, so backdate it: a call whose requests are
	// all handled by the router would otherwise be closed as idle.
	h.record.mu.Lock()
	h.record.lastActivity = time.Now().Add(-time.Hour)
	h.record.mu.Unlock()
	before := h.messageCount(t, "control-source")
	h.agent.
		AddToolCall("call_dir", tools.SessionDirectoryToolName, map[string]any{"query": "work"}).
		AddTextResponse("The Voice work session is the one you want.")

	h.provider.events <- live.Event{
		Kind:         live.EventDelegationCreated,
		DelegationID: "control-1",
		Text:         "which session matches work?",
	}
	answer := h.commentary(t)

	if !strings.Contains(answer, "Voice work") {
		t.Fatalf("answer = %q", answer)
	}
	h.delegator.assertSessionLaneUntouched(t)
	if after := h.messageCount(t, "control-source"); after != before {
		t.Fatalf("a handled request wrote to the transcript: %d messages, want %d", after, before)
	}
	if run := h.server.ensureResponseRuns().latestRun("control-source"); run != nil {
		t.Fatal("a handled request started a response run")
	}
	if idle := h.record.idleFor(time.Now()); idle > time.Minute {
		t.Fatalf("the request did not count as activity: idle for %s", idle)
	}
	// The browser still sees the exchange, on the delegation channel only.
	if states := delegationStates(h.delegationEvents()); states != "queued,running,done" {
		t.Fatalf("delegation updates = %v", states)
	}

	requests := h.agent.RecordedRequests()
	if len(requests) != 2 {
		t.Fatalf("routing turns = %d, want a tool call and its answer", len(requests))
	}
	first := requests[0]
	if names := toolSpecNames(first); !slices.Equal(names, liveControlToolNames()) {
		t.Fatalf("router tools = %v, want %v", names, liveControlToolNames())
	}
	if !first.Ephemeral || first.SessionID != "control-source" {
		t.Fatalf("router request leaked session state: ephemeral=%v session=%q", first.Ephemeral, first.SessionID)
	}
	if first.MaxTurns != liveControlMaxTurns {
		t.Fatalf("router turn budget = %d, want %d", first.MaxTurns, liveControlMaxTurns)
	}
	if prompt := firstSystemText(first); prompt != liveControlSystemPrompt {
		t.Fatalf("router system prompt = %q", prompt)
	}
	last := lastUserMessageText(t, first)
	for _, want := range []string{"which session matches work?", "Current call binding:"} {
		if !strings.Contains(last, want) {
			t.Fatalf("router user message lost %q: %q", want, last)
		}
	}
	if !strings.Contains(last, "Voice work") {
		t.Fatalf("router user message does not name the bound session: %q", last)
	}
	// The directory answer reached the router, which is what lets it resolve a
	// description to a session number instead of inventing one.
	if result := toolResultText(requests[1], tools.SessionDirectoryToolName); !strings.Contains(result, "Voice work") {
		t.Fatalf("the router did not receive the directory result: %q", result)
	}
}

// TestLiveProductionFailureSwitchesByDescription is the first of the two failures
// that motivated this lane, end to end and with the production router behind a real
// controller. The user pointed at a directory entry and the voice model delegated
// "Switch to the attachment icons one" as ordinary work, so the host ran it as a chat
// turn in the bound session and the request to leave that session became its content.
// Nothing in the sentence names a session, and no rule can be asked to know that it
// does.
func TestLiveProductionFailureSwitchesByDescription(t *testing.T) {
	h := newControlLaneHarness(t, "icons-source")
	createDirectorySession(t, h.store, &session.Session{ID: "icons-target", GeneratedShortTitle: "Attachment icons"})
	target, err := h.store.Get(context.Background(), "icons-target")
	if err != nil || target == nil {
		t.Fatal(err)
	}
	before := h.messageCount(t, "icons-source")
	h.agent.
		AddToolCall("call_dir", tools.SessionDirectoryToolName, map[string]any{}).
		AddToolCall("call_switch", tools.LiveSwitchSessionToolName, map[string]any{"session": target.Number}).
		AddTextResponse("Switched to the Attachment icons session.")

	h.provider.events <- live.Event{
		Kind:         live.EventDelegationCreated,
		DelegationID: "icons-1",
		Text:         "Switch to the attachment icons one",
	}
	answer := h.answer(t)

	if !strings.Contains(answer, "Attachment icons") {
		t.Fatalf("answer = %q", answer)
	}
	if binding := h.record.boundSession(); binding != "icons-target" {
		t.Fatalf("binding = %q, want the session the user pointed at", binding)
	}
	h.delegator.assertSessionLaneUntouched(t)
	if after := h.messageCount(t, "icons-source"); after != before {
		t.Fatalf("the session the call left gained messages: %d, want %d", after, before)
	}
	if run := h.server.ensureResponseRuns().latestRun("icons-source"); run != nil {
		t.Fatal("the switch started a response run in the session it left")
	}
	events := h.delegationEvents()
	if states := delegationStates(events); states != "queued,running,done" {
		t.Fatalf("delegation updates = %v", states)
	}
	assertNoTerminalDelegationText(t, events)
}

// TestLiveProductionFailureSwitchesBackByDescription is the second failure: the user
// paraphrased, "Do you mind switching back to the session ... uh ... where I was
// managing session with voice", and the detector of the day knew "switch" but not
// "switching". The router reads the request, so the word form it chose cannot matter.
func TestLiveProductionFailureSwitchesBackByDescription(t *testing.T) {
	h := newControlLaneHarness(t, "switchback-source")
	createDirectorySession(t, h.store, &session.Session{ID: "switchback-target", GeneratedShortTitle: "Manage sessions with voice"})
	target, err := h.store.Get(context.Background(), "switchback-target")
	if err != nil || target == nil {
		t.Fatal(err)
	}
	before := h.messageCount(t, "switchback-source")
	h.agent.
		AddToolCall("call_switch", tools.LiveSwitchSessionToolName, map[string]any{"session": target.Number}).
		AddTextResponse("Back in the session where you were managing sessions with voice.")

	h.provider.events <- live.Event{
		Kind:         live.EventDelegationCreated,
		DelegationID: "switchback-1",
		Text:         "Do you mind switching back to the session... uh... where I was managing session with voice",
	}
	answer := h.answer(t)

	if !strings.Contains(answer, "managing sessions with voice") {
		t.Fatalf("answer = %q", answer)
	}
	if binding := h.record.boundSession(); binding != "switchback-target" {
		t.Fatalf("binding = %q, want the session the user described", binding)
	}
	h.delegator.assertSessionLaneUntouched(t)
	if after := h.messageCount(t, "switchback-source"); after != before {
		t.Fatalf("the session the call left gained messages: %d, want %d", after, before)
	}
	if states := delegationStates(h.delegationEvents()); states != "queued,running,done" {
		t.Fatalf("delegation updates = %v", states)
	}
}

// TestLiveWorkspaceRequestIsHandedOverInOneProviderRequest drives the common path:
// the router recognises workspace work, calls pass_to_workspace, and the request runs
// in the session lane. The handoff costs exactly one provider request — the router's
// turn is cancelled from inside the tool rather than paying for a closing remark —
// and nothing is scripted after the tool call, so a second request would fail loudly
// here as well as being counted.
func TestLiveWorkspaceRequestIsHandedOverInOneProviderRequest(t *testing.T) {
	h := newControlLaneHarness(t, "handoff-source")
	h.agent.AddToolCall("call_pass", livePassToWorkspaceToolName, map[string]any{"request": "run the session store tests"})

	h.provider.events <- live.Event{
		Kind:         live.EventDelegationCreated,
		DelegationID: "work-1",
		Text:         "um, can you run the session store tests",
	}

	waitForLiveCondition(t, "the handed-over request", func() bool { return len(h.delegator.inputs()) == 1 })
	if got := h.delegator.inputs()[0]; got != "run the session store tests" {
		t.Fatalf("session lane received %q, want the routed request", got)
	}
	if requests := h.agent.RecordedRequests(); len(requests) != 1 {
		t.Fatalf("the handoff cost %d provider requests, want 1", len(requests))
	}
	// A handoff is not an answer: the router must not have spoken for the work it
	// handed over, and the browser must see the delegation run in the session lane.
	h.provider.mu.Lock()
	commentary := append([]live.DelegationChunk(nil), h.provider.delegated...)
	h.provider.mu.Unlock()
	for _, chunk := range commentary {
		if chunk.Channel == live.ChannelCommentary {
			t.Fatalf("a handed-over request was answered by the router: %+v", chunk)
		}
	}
	waitForLiveCondition(t, "the session lane run", func() bool {
		states := delegationStates(h.delegationEvents())
		return states == "queued,running,done"
	})
}

// TestLiveRouterActsThroughEveryControlTool drives the call-scoped tools through the
// router's own engine, which is the only place they are registered: the directory
// lookup, the rebind, and the voice change all take effect. (Creating a conversation
// has its own harness, in serve_live_new_session_test.go.)
func TestLiveRouterActsThroughEveryControlTool(t *testing.T) {
	h := newControlLaneHarness(t, "agent-source")
	createDirectorySession(t, h.store, &session.Session{ID: "agent-target", GeneratedShortTitle: "Discourse Backport"})
	addDirectoryMessage(t, h.store, "agent-target", "the discourse backport discussion")
	target, err := h.store.Get(context.Background(), "agent-target")
	if err != nil || target == nil {
		t.Fatal(err)
	}
	h.agent.
		AddToolCall("call_dir", tools.SessionDirectoryToolName, map[string]any{"query": "backport"}).
		AddToolCall("call_switch", tools.LiveSwitchSessionToolName, map[string]any{"session": target.Number}).
		AddToolCall("call_voice", tools.LiveSettingsToolName, map[string]any{"voice": "maple"}).
		AddTextResponse("Moved to Discourse Backport and switched the voice to maple.")

	h.provider.events <- live.Event{
		Kind:         live.EventDelegationCreated,
		DelegationID: "control-all",
		Text:         "move to the backport session and use the maple voice",
	}
	if answer := h.commentary(t); !strings.Contains(answer, "maple") {
		t.Fatalf("answer = %q", answer)
	}
	if binding := h.record.boundSession(); binding != "agent-target" {
		t.Fatalf("binding = %q, want the switched session", binding)
	}
	if voice := h.voice(); voice != "maple" {
		t.Fatalf("voice = %q, want the requested voice", voice)
	}
	h.delegator.assertSessionLaneUntouched(t)
	if states := delegationStates(h.delegationEvents()); states != "queued,running,done" {
		t.Fatalf("delegation updates = %v", states)
	}
	requests := h.agent.RecordedRequests()
	if len(requests) != 4 {
		t.Fatalf("routing turns = %d, want three tool calls and an answer", len(requests))
	}
	if result := toolResultText(requests[1], tools.SessionDirectoryToolName); !strings.Contains(result, "Discourse Backport") {
		t.Fatalf("the switch did not see the directory result: %q", result)
	}
}

// TestLiveControlActionThatCannotFinishNeverBecomesAChatTurn is the production
// failure, exactly. Asked to switch back to a session by description, the router
// searched the directory three times with widening queries, exhausted its turn
// budget, and produced no closing sentence. The empty answer was read as a routing
// failure, the host fell open, and a request it had already proved was session
// management was written into the bound session's transcript.
//
// A control tool that ran is the evidence: the outcome is reported to the voice
// model, and the session lane never sees the request.
func TestLiveControlActionThatCannotFinishNeverBecomesAChatTurn(t *testing.T) {
	h := newControlLaneHarness(t, "budget-source")
	before := h.messageCount(t, "budget-source")
	// Three widening lookups and nothing left to say with, which is what was
	// observed live.
	h.agent.
		AddToolCall("call_1", tools.SessionDirectoryToolName, map[string]any{"query": "managing session with voice"}).
		AddToolCall("call_2", tools.SessionDirectoryToolName, map[string]any{"query": "voice"}).
		AddToolCall("call_3", tools.SessionDirectoryToolName, map[string]any{"query": ""}).
		AddTextResponse("")

	h.provider.events <- live.Event{
		Kind:         live.EventDelegationCreated,
		DelegationID: "control-budget",
		Text:         "Do you mind switching back to the session... uh... where I was managing session with voice",
	}

	waitForLiveCondition(t, "the failure to reach the voice model", func() bool {
		return strings.Contains(delegationStates(h.delegationEvents()), "failed")
	})
	h.delegator.assertSessionLaneUntouched(t)
	if after := h.messageCount(t, "budget-source"); after != before {
		t.Fatalf("a control request became a chat turn: %d messages, want %d", after, before)
	}
	if run := h.server.ensureResponseRuns().latestRun("budget-source"); run != nil {
		t.Fatal("a control request started a response run")
	}
	if states := delegationStates(h.delegationEvents()); states != "queued,running,failed" {
		t.Fatalf("delegation updates = %v", states)
	}
}

// TestLiveControlBudgetSurvivesWideningLookups guards the turn budget. A router that
// does not find the session on its first guess broadens the query and searches
// again, and with a budget of four that left nothing for the answer: the searches
// used every turn and the turn ended silent. Three lookups and a sentence is the
// realistic worst case, so the budget has to sit above it.
func TestLiveControlBudgetSurvivesWideningLookups(t *testing.T) {
	h := newControlLaneHarness(t, "budget-ok")
	h.agent.
		AddToolCall("call_1", tools.SessionDirectoryToolName, map[string]any{"query": "managing session with voice"}).
		AddToolCall("call_2", tools.SessionDirectoryToolName, map[string]any{"query": "voice"}).
		AddToolCall("call_3", tools.SessionDirectoryToolName, map[string]any{"query": ""}).
		AddTextResponse("That is the Manage sessions with voice session.")

	h.provider.events <- live.Event{
		Kind:         live.EventDelegationCreated,
		DelegationID: "control-widening",
		Text:         "which session was I managing sessions with voice in?",
	}

	if answer := h.commentary(t); !strings.Contains(answer, "Manage sessions with voice") {
		t.Fatalf("answer = %q", answer)
	}
	h.delegator.assertSessionLaneUntouched(t)
	requests := h.agent.RecordedRequests()
	if len(requests) != 4 {
		t.Fatalf("routing turns = %d, want three lookups and an answer", len(requests))
	}
	// The scripted provider does not enforce MaxTurns, so the turn above would pass
	// at any budget. Tie the constant to the shape instead: this is the worst case
	// measured against a live model, and a budget that only just reaches it is what
	// produced a silent turn in production.
	if liveControlMaxTurns <= len(requests) {
		t.Fatalf("liveControlMaxTurns = %d, want headroom above the %d-turn worst case", liveControlMaxTurns, len(requests))
	}
	// And it has to be sent, not merely declared: the controller falls open on a
	// routing turn that runs long, so a request that silently lost its cap would run
	// to the provider's own limit with the voice model waiting on it.
	if first := requests[0]; first.MaxTurns != liveControlMaxTurns {
		t.Fatalf("the routing request carried max_turns = %d, want %d", first.MaxTurns, liveControlMaxTurns)
	}
}

// TestLiveRouterProseWithoutActionRunsAsWork is the inverse, and the mistake worth
// more than the one above: an answer with no tool call behind it proves nothing, so
// treating it as the outcome would swallow a workspace request and speak an
// improvised reply in its place. Evidence decides, so the request runs as work.
func TestLiveRouterProseWithoutActionRunsAsWork(t *testing.T) {
	h := newControlLaneHarness(t, "prose-source")
	h.agent.AddTextResponse("Sure, I'll take a look at that for you.")

	h.provider.events <- live.Event{
		Kind:         live.EventDelegationCreated,
		DelegationID: "control-prose",
		Text:         "add a session index to the sqlite store",
	}

	waitForLiveCondition(t, "the session lane run", func() bool { return len(h.delegator.inputs()) == 1 })
	if got := h.delegator.inputs()[0]; got != "add a session index to the sqlite store" {
		t.Fatalf("the session lane ran %q, want the user's original request", got)
	}
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	for _, chunk := range h.provider.delegated {
		if strings.Contains(chunk.Text, "take a look") {
			t.Fatalf("an unsupported answer was spoken instead of the work running: %q", chunk.Text)
		}
	}
}

// TestLiveRouterInvalidControlArgumentsFallOpenToTheSessionLane pins the boundary
// between "the model made a control request" and "the model did not manage to make
// one". Every control tool decodes with DisallowUnknownFields, so a single
// hallucinated argument key returns ErrInvalidParams — and recording that as an
// attempt would make the request a control failure that never falls open, so the
// user's workspace request would be lost rather than run.
func TestLiveRouterInvalidControlArgumentsFallOpenToTheSessionLane(t *testing.T) {
	h := newControlLaneHarness(t, "invalid-args-source")
	// One invented key is enough: the call never becomes a request the host could act
	// on, whatever the router says afterwards.
	h.agent.
		AddToolCall("call_bad", tools.SessionDirectoryToolName, map[string]any{"running_only": true, "how_many": 3}).
		AddTextResponse("Sure, taking a look.")

	const spoken = "add a session index to the sqlite store"
	h.provider.events <- live.Event{
		Kind:         live.EventDelegationCreated,
		DelegationID: "control-invalid",
		Text:         spoken,
	}

	waitForLiveCondition(t, "the session lane run", func() bool { return len(h.delegator.inputs()) == 1 })
	if got := h.delegator.inputs()[0]; got != spoken {
		t.Fatalf("the session lane ran %q, want the user's original request", got)
	}
	// Run as work, and reported as work: a `failed` here would be the host telling the
	// user their request was a control request the assistant could not complete.
	waitForLiveCondition(t, "the terminal update", func() bool {
		return strings.HasSuffix(delegationStates(h.delegationEvents()), "done")
	})
	if states := delegationStates(h.delegationEvents()); strings.Contains(states, "failed") || strings.Contains(states, "refused") {
		t.Fatalf("a workspace request was reported as a control failure: %v", states)
	}
	// The prose that followed the bad call proves nothing and is not spoken in its
	// place, exactly as for any other answer with no tool call behind it.
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	if len(h.provider.delegated) != 0 {
		t.Fatalf("the router spoke over the work instead: %+v", h.provider.delegated)
	}
}

// TestLiveRouterControlToolThenHandoffStaysAControlRequest pins the precedence of the
// two things a routing turn can leave behind. A turn that reads the session directory
// and then passes the same request to the workspace agent anyway is asking for the one
// thing this lane exists to prevent: the control text arriving in the bound session's
// transcript as a chat turn. Evidence wins, and the handoff is ignored — including
// when, as here, the text the model hands over is the control request itself.
func TestLiveRouterControlToolThenHandoffStaysAControlRequest(t *testing.T) {
	h := newControlLaneHarness(t, "mixed-source")
	before := h.messageCount(t, "mixed-source")
	h.agent.
		AddToolCall("call_dir", tools.SessionDirectoryToolName, map[string]any{}).
		AddToolCall("call_pass", livePassToWorkspaceToolName, map[string]any{"request": "switch to the reflow one"})

	h.provider.events <- live.Event{
		Kind:         live.EventDelegationCreated,
		DelegationID: "control-mixed",
		Text:         "switch to the reflow one",
	}

	waitForLiveCondition(t, "the delegation to settle", func() bool {
		states := delegationStates(h.delegationEvents())
		return strings.Contains(states, "failed") || strings.Contains(states, "done")
	})
	h.delegator.assertSessionLaneUntouched(t)
	if after := h.messageCount(t, "mixed-source"); after != before {
		t.Fatalf("the control text became a chat turn: %d messages, want %d", after, before)
	}
	// No answer came with it, so it is a failure the voice model hears about rather
	// than a silent success.
	if states := delegationStates(h.delegationEvents()); states != "queued,running,failed" {
		t.Fatalf("delegation updates = %v", states)
	}
}

// TestLiveControlProviderWiringReachesTheConfiguredProvider is the only test that
// reaches llm.NewProviderByName: every other harness injects
// liveControlProviderFactory, so the wiring from live.control_provider/control_model
// into the provider constructor would otherwise be untested and could be handed its
// two arguments the wrong way round without a single failure.
func TestLiveControlProviderWiringReachesTheConfiguredProvider(t *testing.T) {
	cfg := &config.Config{Live: config.LiveConfig{ControlProvider: "debug", ControlModel: "debug-fast"}}
	s := &serveServer{cfgRef: cfg}

	provider, err := s.newLiveControlProvider(context.Background(), "wiring-session")
	if err != nil {
		t.Fatalf("resolve the configured routing provider: %v", err)
	}
	// The debug provider carries its variant in its name, so this one assertion covers
	// both arguments: the provider named in live.control_provider received the model
	// named in live.control_model, and neither was dropped.
	if provider == nil || provider.Name() != "debug:debug-fast" {
		t.Fatalf("routing provider = %v, want the configured provider and model", provider)
	}

	// A provider named on its own is resolved through the same dispatch, and the error
	// below can only come from the model argument: claude-bin rejects an effort level
	// used as a model, so arguments handed over the wrong way round would instead
	// report the model as a provider that is not configured.
	cfg.Live.ControlProvider, cfg.Live.ControlModel = "claude-bin", "high"
	_, err = s.newLiveControlProvider(context.Background(), "wiring-session")
	if err == nil || !strings.Contains(err.Error(), "effort level") {
		t.Fatalf("the configured model did not reach the provider constructor: %v", err)
	}
}

// TestLiveControlTargetChoosesTheRoutingModel pins the resolution behind
// live.control_provider / live.control_model. The lane must be tunable on its own,
// because the fast model it otherwise follows is shared with auto-titling and the
// other short host turns, and this is the only one that has to call tools reliably.
func TestLiveControlTargetChoosesTheRoutingModel(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg                              config.LiveConfig
		sessionProvider, defaultProvider string
		wantProvider, wantModel          string
		wantFast                         bool
	}{
		"unset follows the shared fast model": {
			sessionProvider: "chatgpt", defaultProvider: "chatgpt", wantFast: true,
		},
		"blank is unset": {
			cfg:             config.LiveConfig{ControlProvider: "  ", ControlModel: "\t"},
			sessionProvider: "chatgpt", wantFast: true,
		},
		"a model alone keeps the conversation's provider": {
			cfg:             config.LiveConfig{ControlModel: "gpt-5.6-luna"},
			sessionProvider: "chatgpt", defaultProvider: "openai",
			wantProvider: "chatgpt", wantModel: "gpt-5.6-luna",
		},
		"a model alone falls back to the default provider": {
			cfg:             config.LiveConfig{ControlModel: "gpt-5.6-luna"},
			defaultProvider: "openai",
			wantProvider:    "openai", wantModel: "gpt-5.6-luna",
		},
		"both are honoured": {
			cfg:             config.LiveConfig{ControlProvider: "anthropic", ControlModel: "haiku"},
			sessionProvider: "chatgpt", defaultProvider: "chatgpt",
			wantProvider: "anthropic", wantModel: "haiku",
		},
		"a provider alone uses its own default model": {
			cfg:             config.LiveConfig{ControlProvider: "anthropic"},
			sessionProvider: "chatgpt",
			wantProvider:    "anthropic",
		},
	} {
		t.Run(name, func(t *testing.T) {
			provider, model, useFast := liveControlTarget(tc.cfg, tc.sessionProvider, tc.defaultProvider)
			if useFast != tc.wantFast {
				t.Fatalf("useFast = %v, want %v", useFast, tc.wantFast)
			}
			if useFast {
				return
			}
			if provider != tc.wantProvider || model != tc.wantModel {
				t.Fatalf("target = %q/%q, want %q/%q", provider, model, tc.wantProvider, tc.wantModel)
			}
		})
	}
}

// TestLiveRouterCannotForgeItsOwnAuthority is the other half of the tool grant. The
// control tools are call-scoped, so a context that merely names a session — exactly
// what a model-authored or ordinary turn could carry — must deny each of them, while
// the context the host installs for the same turn must not.
func TestLiveRouterCannotForgeItsOwnAuthority(t *testing.T) {
	h := newControlLaneHarness(t, "authority-source")
	// The probe runs the tools the routing engine actually registers, not the raw
	// implementations behind them: the registered surface is what a model can call, so
	// that is where the denial has to hold.
	probe := &controlAuthorityProbe{inner: h.agent, sessionID: "authority-source", probed: controlRegisteredTools(t, h.agent)}
	h.server.liveControlProviderFactory = func(string) (llm.Provider, error) { return probe, nil }
	// The turn has to reach for a session tool, because that is what now proves the
	// request was session management: a router that only talks is not treated as
	// having handled anything.
	h.agent.
		AddToolCall("call_dir", tools.SessionDirectoryToolName, map[string]any{"running_only": true}).
		AddTextResponse("nothing to do")

	h.provider.events <- live.Event{
		Kind:         live.EventDelegationCreated,
		DelegationID: "control-authority",
		Text:         "what sessions are running?",
	}
	h.commentary(t)

	probe.mu.Lock()
	defer probe.mu.Unlock()
	// The probe runs once per provider request, and a turn that calls a tool makes
	// more than one, so assert the shape rather than a fixed count: every tool is
	// exercised in both contexts on every pass.
	names := controlToolNames()
	if len(probe.hostErrs) != len(probe.forgedErrs) || len(probe.hostErrs) == 0 || len(probe.hostErrs)%len(names) != 0 {
		t.Fatalf("probe ran %d host and %d forged calls, want equal non-zero multiples of %d", len(probe.hostErrs), len(probe.forgedErrs), len(names))
	}
	for i := range probe.hostErrs {
		name := names[i%len(names)]
		if err := probe.hostErrs[i]; err != nil {
			t.Fatalf("%s denied the host's own authority: %v", name, err)
		}
		var toolErr *tools.ToolError
		if err := probe.forgedErrs[i]; !errors.As(err, &toolErr) || toolErr.Type != tools.ErrPermissionDenied {
			t.Fatalf("%s accepted a forged context: %v", name, err)
		}
	}
}

// controlAuthorityProbe runs the call-scoped tools the routing engine registers twice
// per routing turn: once with the context the engine handed the provider (the host's
// authority) and once with a context that only names the session, which is all a model
// could construct for itself. Probing the raw implementations instead would leave the
// registered surface — the wrappers and whatever else the engine puts in front of them
// — outside the claim, and that is the surface a model calls.
type controlAuthorityProbe struct {
	inner     llm.Provider
	sessionID string
	probed    []llm.Tool

	mu         sync.Mutex
	hostErrs   []error
	forgedErrs []error
}

// controlRegisteredTools returns one routing turn's call-scoped tools exactly as the
// engine registers them.
func controlRegisteredTools(t *testing.T, provider llm.Provider) []llm.Tool {
	t.Helper()
	engine, _ := newLiveControlEngine(provider, &workspaceHandoff{cancel: func() {}}, &controlActivity{})
	tools := make([]llm.Tool, 0, len(controlToolNames()))
	for _, name := range controlToolNames() {
		tool, ok := engine.Tools().Get(name)
		if !ok {
			t.Fatalf("the routing engine did not register %s", name)
		}
		tools = append(tools, tool)
	}
	return tools
}

func (p *controlAuthorityProbe) Name() string                   { return p.inner.Name() }
func (p *controlAuthorityProbe) Credential() string             { return p.inner.Credential() }
func (p *controlAuthorityProbe) Capabilities() llm.Capabilities { return p.inner.Capabilities() }
func (p *controlAuthorityProbe) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	hostErrs := make([]error, 0, len(p.probed))
	forgedErrs := make([]error, 0, len(p.probed))
	for _, tool := range p.probed {
		args := controlAuthorityProbeArgs(tool.Spec().Name, p.sessionID)
		_, hostErr := tool.Execute(ctx, args)
		forged := llm.ContextWithSessionID(context.Background(), p.sessionID)
		_, forgedErr := tool.Execute(forged, args)
		hostErrs = append(hostErrs, hostErr)
		forgedErrs = append(forgedErrs, forgedErr)
	}
	p.mu.Lock()
	p.hostErrs = append(p.hostErrs, hostErrs...)
	p.forgedErrs = append(p.forgedErrs, forgedErrs...)
	p.mu.Unlock()
	return p.inner.Stream(ctx, req)
}

// controlAuthorityProbeArgs builds arguments that would succeed if authority were
// granted, so a denial can only come from the missing binding.
func controlAuthorityProbeArgs(name, sessionID string) json.RawMessage {
	switch name {
	case tools.LiveSwitchSessionToolName:
		return json.RawMessage(`{"session":"` + sessionID + `"}`)
	default:
		return json.RawMessage(`{}`)
	}
}

// TestLiveSessionLaneInstallsNoSessionControlBindings pins the boundary by binding
// rather than by registration. A delegated chat turn is offered no session-control
// schema, and that has to be a property of the code as well: the three tools are not
// registry tools at all, so no configuration can ask for them, and their authority is
// installed only for a routing turn. A chat turn that reaches one must be denied
// instead of answering a control request as work and writing it into the transcript.
func TestLiveSessionLaneInstallsNoSessionControlBindings(t *testing.T) {
	s := newTestServeServer()
	record := newLiveSession("call-bindings", "bindings-session")
	record.capabilities = live.ConfigCapabilities(config.LiveConfig{})
	record.voiceSession = &liveVoiceControlStub{voice: "cove"}
	ctx := s.withLiveSettingsContext(llm.ContextWithSessionID(context.Background(), "bindings-session"), record, "bindings-session")

	for name, tool := range map[string]llm.Tool{
		tools.SessionDirectoryToolName:  &tools.SessionDirectoryTool{},
		tools.LiveNewSessionToolName:    &tools.LiveNewSessionTool{},
		tools.LiveSwitchSessionToolName: &tools.LiveSwitchSessionTool{},
	} {
		if _, err := tool.Execute(ctx, controlAuthorityProbeArgs(name, "bindings-session")); !isPermissionDenied(err) {
			t.Fatalf("%s executed in a delegated turn: %v", name, err)
		}
	}
	// live_settings is the exception, and deliberately so: it is a registry tool a
	// conversation can be configured with, so its binding is what lets such a turn act
	// on the call driving it — the behaviour that existed before the control plane.
	out, err := (&tools.LiveSettingsTool{}).Execute(ctx, json.RawMessage(`{"voice":"maple"}`))
	if err != nil {
		t.Fatalf("the session lane lost its own call's settings: %v", err)
	}
	if !strings.Contains(out.Content, "maple") {
		t.Fatalf("settings result = %q", out.Content)
	}
}

// TestLiveControlAuthorityIsInstalledOnlyWithTheControlPlane pins the flag as a
// property of the authority itself. The wiring already only builds a router when the
// control plane is on, so this is the second lock on the same door: a host that
// reached routing some other way still cannot hand a model the tools that move the
// call, and the refusal degrades to a workspace request instead of a lost one.
func TestLiveControlAuthorityIsInstalledOnlyWithTheControlPlane(t *testing.T) {
	for name, plane := range map[string]bool{"off": false, "on": true} {
		t.Run(name, func(t *testing.T) {
			s := newTestServeServer()
			s.cfgRef = &config.Config{Live: config.LiveConfig{ControlPlane: plane}}
			s.store = newSessionDirectoryTestStore(t)
			record := newLiveSession("call-authority-gate", "gate-session")
			record.capabilities = live.ConfigCapabilities(config.LiveConfig{})

			ctx := s.withLiveControlAuthority(context.Background(), record, "gate-session")
			_, err := (&tools.SessionDirectoryTool{}).Execute(ctx, json.RawMessage(`{}`))
			if plane {
				if err != nil {
					t.Fatalf("the control plane could not read the directory: %v", err)
				}
				return
			}
			if !isPermissionDenied(err) {
				t.Fatalf("control authority was installed with live.control_plane off: %v", err)
			}
			for toolName, tool := range map[string]llm.Tool{
				tools.LiveNewSessionToolName:    &tools.LiveNewSessionTool{},
				tools.LiveSwitchSessionToolName: &tools.LiveSwitchSessionTool{},
			} {
				if _, err := tool.Execute(ctx, controlAuthorityProbeArgs(toolName, "gate-session")); !isPermissionDenied(err) {
					t.Fatalf("%s was installed with live.control_plane off: %v", toolName, err)
				}
			}
		})
	}
}

// TestLiveSessionOptionsPromisesHostRoutingOnlyWithTheControlPlane drives both
// directions of the conditional sentence. The voice prompt is static so providers can
// cache it, which means the claim that a routing model reads delegations can only come
// from host context — and a host without the flag must not make it, because there
// those requests are ordinary work in the bound session.
func TestLiveSessionOptionsPromisesHostRoutingOnlyWithTheControlPlane(t *testing.T) {
	for name, plane := range map[string]bool{"off": false, "on": true} {
		t.Run(name, func(t *testing.T) {
			s := newTestServeServer()
			s.cfgRef = &config.Config{Live: config.LiveConfig{ControlPlane: plane}}

			opts := s.liveSessionOptions(context.Background(), "options-session", live.ConfigCapabilities(config.LiveConfig{}))
			if promised := strings.Contains(opts.Context, live.ControlPlaneContext); promised != plane {
				t.Fatalf("host routing promised = %v with live.control_plane = %v: %s", promised, plane, opts.Context)
			}
			if !strings.Contains(opts.Context, live.CapabilityContext(live.ConfigCapabilities(config.LiveConfig{}))) {
				t.Fatalf("the host facts were lost: %s", opts.Context)
			}
		})
	}
}

// isPermissionDenied reports whether err is the tool layer's refusal, which is how a
// missing call-scoped binding surfaces.
func isPermissionDenied(err error) bool {
	var toolErr *tools.ToolError
	return errors.As(err, &toolErr) && toolErr.Type == tools.ErrPermissionDenied
}

// TestLiveRequestWithoutARouterRunsInTheSessionLane is the fail-open guarantee at the
// level that matters: no fast model to route with, or a fast model that errors, must
// not refuse or lose the user's request. Both cases run the ORIGINAL text in the bound
// session, exactly as they would have before any routing existed.
func TestLiveRequestWithoutARouterRunsInTheSessionLane(t *testing.T) {
	for name, factory := range map[string]func(string) (llm.Provider, error){
		"unresolved": func(string) (llm.Provider, error) { return nil, nil },
		"error":      func(string) (llm.Provider, error) { return nil, errors.New("no fast model configured") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newControlLaneHarness(t, "no-router-"+name)
			h.server.liveControlProviderFactory = factory

			const spoken = "which session is about the reflow decision?"
			h.provider.events <- live.Event{
				Kind:         live.EventDelegationCreated,
				DelegationID: "no-router",
				Text:         spoken,
			}

			waitForLiveCondition(t, "the request in the session lane", func() bool {
				return len(h.delegator.inputs()) == 1
			})
			if got := h.delegator.inputs()[0]; got != spoken {
				t.Fatalf("session lane received %q, want the original request", got)
			}
			// Fail open, not fail closed: nothing was refused, and the browser was not
			// told about an outage it cannot act on.
			events := h.delegationEvents()
			waitForLiveCondition(t, "the completed delegation", func() bool {
				states := delegationStates(events)
				return strings.HasSuffix(states, "done")
			})
			if states := delegationStates(h.delegationEvents()); strings.Contains(states, "refused") || strings.Contains(states, "failed") {
				t.Fatalf("a router outage was reported as a failure: %v", states)
			}
			assertNoTerminalDelegationText(t, h.delegationEvents())
		})
	}
}

// TestLiveRouterRequestWithoutABoundSessionFallsOpen covers the other unusable-router
// shape: the call is not bound to a chat session, so there is nothing to pin authority
// to. The request still runs in the session lane rather than being refused.
func TestLiveRouterRequestWithoutABoundSessionFallsOpen(t *testing.T) {
	ctx := context.Background()
	s := newTestServeServer()
	s.store = newSessionDirectoryTestStore(t)
	unbound := newLiveSession("call-unbound", "")
	router := &serveLiveControlExecutor{server: s, live: unbound}
	if _, err := router.Route(ctx, live.RouteRequest{Input: "list the sessions"}); err == nil {
		t.Fatal("an unbound call routed a request")
	}
	// A missing router configuration is an error, not a nil-pointer success.
	var absent *serveLiveControlExecutor
	if _, err := absent.Route(ctx, live.RouteRequest{Input: "list the sessions"}); err == nil {
		t.Fatal("a missing router routed a request")
	}
}

// TestLiveRouterAnswerIsClippedForSpeech keeps a runaway answer inside the
// transport's append budget, so the voice model is not handed a transcript of tool
// output to read aloud.
func TestLiveRouterAnswerIsClippedForSpeech(t *testing.T) {
	long := strings.Repeat("a very long spoken answer ", 40)
	clipped := liveControlClipAnswer(long)
	if len(clipped) > liveControlAnswerLimitBytes+len("…") {
		t.Fatalf("clipped answer is %d bytes", len(clipped))
	}
	if !strings.HasSuffix(clipped, "…") {
		t.Fatalf("clipped answer does not read as truncated: %q", clipped)
	}
	if short := liveControlClipAnswer("Switched to session #42."); short != "Switched to session #42." {
		t.Fatalf("short answer changed: %q", short)
	}
	// A multi-byte answer must not be cut mid-rune: the transport would send invalid
	// UTF-8 to the voice model.
	multibyte := liveControlAnswerClippingKeepsRunes()
	if !utf8.ValidString(multibyte) {
		t.Fatalf("clipped answer is not valid UTF-8: %q", multibyte)
	}
}

// liveControlAnswerClippingKeepsRunes is the multi-byte clipping case, split out so
// the assertion above reads as one claim.
func liveControlAnswerClippingKeepsRunes() string {
	return liveControlClipAnswer(strings.Repeat("é", liveControlAnswerLimitBytes))
}

// TestLiveRequestSwitchesWhileATaskRuns pins the second bug the lane fixes: a switch
// spoken while a task is running is performed by the router, not injected as steering
// guidance, and it does not disturb the running session.
func TestLiveRequestSwitchesWhileATaskRuns(t *testing.T) {
	h := newControlLaneHarness(t, "control-busy")
	createDirectorySession(t, h.store, &session.Session{ID: "control-target", GeneratedShortTitle: "Discourse Backport"})
	target, err := h.store.Get(context.Background(), "control-target")
	if err != nil || target == nil {
		t.Fatal(err)
	}
	before := h.messageCount(t, "control-busy")
	// The bound session has a task running, which is exactly the case that used to
	// be offered to Steer first.
	manager := h.server.ensureResponseRuns()
	manager.setActiveRun("control-busy", "resp_busy")
	t.Cleanup(func() { manager.clearActiveRun("control-busy", "resp_busy") })
	h.agent.
		AddToolCall("call_switch", tools.LiveSwitchSessionToolName, map[string]any{"session": target.Number}).
		AddTextResponse("Switched to Discourse Backport.")

	h.provider.events <- live.Event{
		Kind:         live.EventDelegationCreated,
		DelegationID: "control-switch",
		Text:         "switch to the discourse backport session",
	}
	answer := h.answer(t)

	if !strings.Contains(answer, "Discourse Backport") {
		t.Fatalf("switch answer = %q", answer)
	}
	if h.record.boundSession() != "control-target" {
		t.Fatalf("binding = %q", h.record.boundSession())
	}
	h.delegator.assertSessionLaneUntouched(t)
	if after := h.messageCount(t, "control-busy"); after != before {
		t.Fatalf("the switch dirtied the session it left: %d messages, want %d", after, before)
	}
	if active := h.server.ensureResponseRuns().activeRunID("control-busy"); active != "resp_busy" {
		t.Fatalf("the running task was disturbed by the switch: active run = %q", active)
	}
}

// TestLiveHandledDelegationIsAnsweredWithoutAChatTurn drives the production call path
// end to end: a real `startLiveController` call, a real controller, and the router it
// wires. A spoken session-management request must come back as commentary on the
// sideband and must not create a session runtime or a response run, which is what a
// chat turn would do.
func TestLiveHandledDelegationIsAnsweredWithoutAChatTurn(t *testing.T) {
	harness := newLiveTestHarness(t, "the model must never run")
	store := newSessionDirectoryTestStore(t)
	harness.server.store = store
	// The control plane is opt-in, so the production path this test drives only
	// wires a router when it is switched on.
	harness.server.cfgRef.Live.ControlPlane = true
	stopLiveHarnessLifecycle(t, harness.server)
	createDirectorySession(t, store, &session.Session{ID: "control-e2e", GeneratedShortTitle: "Reflow decisions"})
	addDirectoryMessage(t, store, "control-e2e", "the reflow decision was to keep the pane resizable")
	agent := llm.NewMockProvider("mock-agent")
	harness.server.liveControlProviderFactory = func(string) (llm.Provider, error) { return agent, nil }
	agent.
		AddToolCall("call_dir", tools.SessionDirectoryToolName, map[string]any{"query": "reflow"}).
		AddTextResponse("The Reflow decisions session has that.")

	status, body := harness.startLive(t, "control-e2e")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	conn := <-harness.conns
	delegation := `{"type":"delegation.created","item":{"id":"item_control","type":"delegation","target":"client","content":[{"type":"input_text","text":"which session is about the reflow decision?"}]}}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(delegation)); err != nil {
		t.Fatal(err)
	}

	message := harness.waitForInbound(t, "the answer", func(message string) bool {
		return strings.Contains(message, "delegation.context.append") && strings.Contains(message, "Reflow decisions")
	})
	for _, want := range []string{`"delegation_item_id":"item_control"`, `"channel":"commentary"`} {
		if !strings.Contains(message, want) {
			t.Fatalf("the answer lost %s: %s", want, message)
		}
	}
	// The request is answered by the host's router, so nothing in the chat session
	// moved: no runtime was materialised, no run was registered, and no message was
	// persisted.
	if runtime, ok := harness.server.sessionMgr.Get("control-e2e"); ok {
		t.Fatalf("a handled request created a chat runtime: %+v", runtime)
	}
	if run := harness.server.ensureResponseRuns().latestRun("control-e2e"); run != nil {
		t.Fatal("a handled request started a response run")
	}
	messages, err := store.GetMessages(context.Background(), "control-e2e", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("a handled request wrote to the transcript: %d messages", len(messages))
	}
}

// TestLiveControlPlaneOffRunsEverythingInTheSessionLane pins what the opt-in
// actually buys. With live.control_plane unset — the default — no router is wired,
// so a request that the control plane would have recognised and answered is
// ordinary work for the bound session instead. That is the behaviour the voice
// surface had before triage existed, and it is what a host gets until it opts in.
func TestLiveControlPlaneOffRunsEverythingInTheSessionLane(t *testing.T) {
	harness := newLiveTestHarness(t, "I cannot move this call.")
	store := newSessionDirectoryTestStore(t)
	harness.server.store = store
	stopLiveHarnessLifecycle(t, harness.server)
	createDirectorySession(t, store, &session.Session{ID: "control-off", GeneratedShortTitle: "Reflow decisions"})
	if harness.server.cfgRef.Live.ControlPlane {
		t.Fatal("live.control_plane must be off unless a host opts in")
	}
	// A router that fails the test if it is ever consulted: "off" has to mean no
	// fast-model turn at all, not a turn whose answer is discarded.
	harness.server.liveControlProviderFactory = func(string) (llm.Provider, error) {
		t.Error("the control plane was consulted while it was switched off")
		return nil, errors.New("must not be called")
	}

	status, body := harness.startLive(t, "control-off")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	conn := <-harness.conns
	delegation := `{"type":"delegation.created","item":{"id":"item_off","type":"delegation","target":"client","content":[{"type":"input_text","text":"switch to the reflow session"}]}}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(delegation)); err != nil {
		t.Fatal(err)
	}

	// The session lane answers, which is the whole point: the request became a chat
	// turn rather than being triaged away from one.
	harness.waitForInbound(t, "the session lane's answer", func(message string) bool {
		return strings.Contains(message, "delegation.context.append") && strings.Contains(message, "I cannot move this call.")
	})
	waitForLiveCondition(t, "the response run", func() bool {
		return harness.server.ensureResponseRuns().latestRun("control-off") != nil
	})
}

// stopLiveHarnessLifecycle stops the background response-lifecycle sweep a
// harness server starts once it has a store, so a test's temporary database is
// not closed underneath a running goroutine.
func stopLiveHarnessLifecycle(t *testing.T, srv *serveServer) {
	t.Helper()
	t.Cleanup(func() {
		if srv.responseLifecycleCancel != nil {
			srv.responseLifecycleCancel()
			srv.responseLifecycleWG.Wait()
		}
	})
}
