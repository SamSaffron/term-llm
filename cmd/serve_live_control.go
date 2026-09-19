package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/live"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

const (
	// liveControlMaxTurns bounds the router's agentic turns, each of which is one
	// provider request and one tool round.
	//
	// Eight, because four was measured to be too few. A model that cannot find the
	// session on its first guess broadens the query and searches again — observed
	// live as three `session_directory` calls for "switching back to the session
	// where I was managing session with voice" — and with a budget of four the turn
	// ended with the searches done and no sentence left to say. The realistic worst
	// case is a few widening lookups, one change, and the spoken answer, so the cap
	// has to sit above that rather than exactly on it. Wall-clock is bounded by
	// liveControlTurnTimeout regardless, and a workspace request hands off on its
	// first turn, so this budget only ever applies to session management.
	liveControlMaxTurns = 8
	// liveControlTurnTimeout bounds one routing turn. It is a ceiling on how long the
	// voice model waits for its own answer, so it sits just above the slowest
	// tolerable silence rather than at the providers' own limits; a turn that runs
	// longer is a failure the controller falls open on rather than an unbounded wait.
	liveControlTurnTimeout = 30 * time.Second
	// liveControlAnswerLimitBytes bounds the answer the router speaks back. The
	// transport appends to the voice model in live.MaxAppendBytes (500-byte) chunks,
	// so an answer longer than this arrives split across appends for no gain: the
	// router is asked for one or two spoken sentences, and anything past a few
	// hundred bytes is it transcribing tool output.
	liveControlAnswerLimitBytes = 400
	// liveControlTranscriptLimitBytes bounds the recent conversation the router reads.
	// It exists so a reference like "the attachment icons one" can be resolved to a
	// session that was just read out, and four kilobytes of speech is far more than
	// one such reference needs.
	liveControlTranscriptLimitBytes = 4 << 10
	// livePassToWorkspaceToolName is the host-local tool the router calls to hand a
	// request to the workspace agent. It has no schema in any registry and no
	// authority context: it exists only inside one routing turn's engine, and it
	// writes to that turn's recorder, so a model cannot reach it from anywhere else.
	livePassToWorkspaceToolName = "pass_to_workspace"
)

// liveControlSystemPrompt is deliberately static, so providers can cache it: every
// request travels in the user message, including the current binding.
//
// Almost every delegation is workspace work, so the prompt says so first and asks
// for it to be handed over immediately: the router's cost is a model turn in front
// of every request, and the only thing that makes that affordable is that the common
// path calls one tool and stops.
const liveControlSystemPrompt = `You triage one spoken request for a term-llm voice call.

You can do two things:
1. Manage the call itself — list or search the user's chat sessions, start a new conversation, move this call to a different session, or inspect or change its voice — using the session tools.
2. Hand everything else to the workspace agent with pass_to_workspace.

Almost every request is workspace work: code, files, the shell, investigation, questions about the project. Hand those over immediately, without calling any other tool.

Handle it yourself only when the request is about the user's sessions, this call, or its voice — however it is phrased, including pointing at a session that was just listed ("the attachment icons one", "that one", "go back to the previous one") or asking for a new one ("start a new conversation in the term-llm project"). live_switch_session needs a session number or a full session id from a session_directory result, so look it up first and never invent one; live_new_session takes an optional project and agent and starts an empty conversation.

When you hand over, pass the request through unchanged unless speech recognition garbled it or it is padded with filler; then pass a cleaned-up version that keeps the user's meaning and every detail. Never answer a workspace request yourself.

When you handle it yourself, answer in one or two short sentences that will be read aloud, naming the session you acted on.`

const liveSwitchResolverSystemPrompt = `You resolve one spoken request to move a term-llm voice call to an existing chat session.

You have exactly two tools. Use session_directory to find candidate sessions, then call live_switch_session only with a session number or full session id returned by the directory. Never invent a target. You cannot start sessions, change voice settings, hand work to another agent, or answer unrelated requests.

If one target is clear, switch and briefly confirm it by name. If there is no unique match, ask one short "which one?" question naming the best candidates. Do not claim a switch unless live_switch_session succeeded.`

// serveLiveControlExecutor triages the live call's delegations: one fast-model turn
// per delegation, with no chat session, no transcript write, and no persisted run.
// It is the host's live.Router.
type serveLiveControlExecutor struct {
	server *serveServer
	live   *liveSession
}

// Route decides what happens to one delegation. Every failure it returns is a
// failure the controller falls open on: the request still reaches the session lane,
// with its original text, so an unusable router costs nothing but a slower answer.
// Nothing here refuses a request, because the user made it either way.
func (e *serveLiveControlExecutor) Route(ctx context.Context, request live.RouteRequest) (live.RouteResult, error) {
	if e == nil || e.server == nil || e.live == nil {
		return live.RouteResult{}, errors.New("the live router is unavailable")
	}
	// Routing is traffic like any other: without this an all-control call is reaped
	// as idle while the user is still talking to it.
	e.live.touch()
	sessionID := e.live.boundSession()
	if sessionID == "" {
		return live.RouteResult{}, errors.New("the live call is not bound to a chat session")
	}
	return e.server.runLiveRoute(ctx, e.live, sessionID, request)
}

// runLiveRoute runs one routing turn. There is no runtime, no checkpoint, and no
// session state: the control tools act through the host's own context, the handoff
// tool writes to this turn's recorder, and the answer exists only to be spoken back
// to the voice model.
func (s *serveServer) runLiveRoute(ctx context.Context, record *liveSession, sessionID string, request live.RouteRequest) (live.RouteResult, error) {
	provider, err := s.newLiveControlProvider(ctx, sessionID)
	if err != nil || provider == nil {
		// An unreachable router is not a failed run: the request may be ordinary
		// work, and the controller's answer to a router error is to run it in the
		// session lane. The cause stays in the operator log.
		if err == nil {
			err = errors.New("no fast model is configured")
		}
		log.Printf("[serve] live router unavailable for %s: %v", sessionID, err)
		return live.RouteResult{}, fmt.Errorf("live router unavailable: %w", err)
	}

	turnCtx, cancel := context.WithTimeout(s.withLiveControlAuthority(ctx, record, sessionID), liveControlTurnTimeout)
	defer cancel()
	// The recorder holds the turn's cancel func so the handoff tool can end the turn
	// from inside itself. It is built after the context so the func is never mutated
	// while the turn runs.
	handoff := &workspaceHandoff{cancel: cancel}
	acted := &controlActivity{}
	engine, specs := newLiveControlEngine(provider, handoff, acted)

	// Ephemeral plus a consistent session id keeps the turn out of every session's
	// provider-side state; DisableExternalWebFetch and the zero search flags keep it
	// from acquiring capabilities this lane never grants. Unlike a side question, the
	// router is expected to call tools, so the tool surface and the turn budget stay
	// populated.
	llmRequest := llm.Request{
		Messages: []llm.Message{
			llm.SystemText(liveControlSystemPrompt),
			llm.UserText(s.liveControlUserMessage(ctx, sessionID, request)),
		},
		Ephemeral:               true,
		SessionID:               sessionID,
		Tools:                   specs,
		MaxTurns:                liveControlMaxTurns,
		DisableExternalWebFetch: true,
	}
	// The tools are call-scoped: without this context each control tool denies, so
	// the turn must run under the authority the host builds and never the caller's.
	stream, err := engine.Stream(turnCtx, llmRequest)
	if err != nil {
		return live.RouteResult{}, fmt.Errorf("live router: %w", err)
	}
	defer stream.Close()

	var answer strings.Builder
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// The common path is workspace work, and pass_to_workspace cancels the
			// turn from inside its own Execute, so the cancellation arrives here as an
			// error: paying for another provider round trip to collect a final sentence
			// nobody reads would put a whole model turn of voice latency in front of
			// every work request. A recorded handoff is therefore read as the decision
			// without waiting for the stream to end — unless the turn also ran a control
			// tool, which is the evidence that outranks it (see liveRouteResult).
			return liveRouteResult(handoff, acted, "", fmt.Errorf("live router turn: %w", err))
		}
		if event.Type == llm.EventTextDelta {
			answer.WriteString(event.Text)
		}
		if event.Type == llm.EventError && event.Err != nil {
			return liveRouteResult(handoff, acted, "", fmt.Errorf("live router turn: %w", event.Err))
		}
	}
	return liveRouteResult(handoff, acted, liveControlClipAnswer(strings.TrimSpace(answer.String())), nil)
}

type liveSwitchResolverOutcomeKind string

const (
	liveSwitchResolverSucceeded liveSwitchResolverOutcomeKind = "switched"
	liveSwitchResolverRefused   liveSwitchResolverOutcomeKind = "refused"
	liveSwitchResolverAmbiguous liveSwitchResolverOutcomeKind = "ambiguous"
	liveSwitchResolverFailOpen  liveSwitchResolverOutcomeKind = "fail_open"
	liveSwitchResolverError     liveSwitchResolverOutcomeKind = "provider_error"
)

type liveSwitchResolverOutcome struct {
	Kind   liveSwitchResolverOutcomeKind
	Answer string
}

// resolveLiveSwitch runs the restricted second-stage agent used only after the
// classifier has confidently selected switch_session. Its evidence, not prose,
// determines the outcome.
func (s *serveServer) resolveLiveSwitch(ctx context.Context, record *liveSession, sessionID string, request live.RouteRequest) (liveSwitchResolverOutcome, error) {
	provider, err := s.newLiveControlProvider(ctx, sessionID)
	if err != nil || provider == nil {
		if err == nil {
			err = errors.New("no switch resolver model is configured")
		}
		return liveSwitchResolverOutcome{Kind: liveSwitchResolverError}, fmt.Errorf("live switch resolver unavailable: %w", err)
	}
	turnCtx, cancel := context.WithTimeout(s.withLiveControlAuthority(ctx, record, sessionID), liveControlTurnTimeout)
	defer cancel()
	activity := &liveSwitchResolverActivity{}
	engine, specs := newLiveToolEngine(provider, []llm.Tool{
		&liveSwitchResolverTool{Tool: &tools.SessionDirectoryTool{}, activity: activity},
		&liveSwitchResolverTool{Tool: &tools.LiveSwitchSessionTool{}, activity: activity},
	})
	stream, err := engine.Stream(turnCtx, llm.Request{
		Messages: []llm.Message{
			llm.SystemText(liveSwitchResolverSystemPrompt),
			llm.UserText(s.liveControlUserMessage(ctx, sessionID, request)),
		},
		Ephemeral: true, SessionID: sessionID, Tools: specs,
		MaxTurns: liveControlMaxTurns, DisableExternalWebFetch: true,
	})
	if err != nil {
		return liveSwitchResolverOutcome{Kind: liveSwitchResolverError}, fmt.Errorf("live switch resolver: %w", err)
	}
	defer stream.Close()
	for {
		event, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			outcome := activity.outcome()
			if outcome.Kind != liveSwitchResolverFailOpen {
				return outcome, nil
			}
			return liveSwitchResolverOutcome{Kind: liveSwitchResolverError}, fmt.Errorf("live switch resolver turn: %w", recvErr)
		}
		if event.Type == llm.EventError && event.Err != nil {
			outcome := activity.outcome()
			if outcome.Kind != liveSwitchResolverFailOpen {
				return outcome, nil
			}
			return liveSwitchResolverOutcome{Kind: liveSwitchResolverError}, fmt.Errorf("live switch resolver turn: %w", event.Err)
		}
	}
	return activity.outcome(), nil
}

type liveSwitchResolverActivity struct {
	mu              sync.Mutex
	switchOutput    string
	switchRefusal   error
	directoryOutput string
}

type liveSwitchResolverTool struct {
	llm.Tool
	activity *liveSwitchResolverActivity
}

func (t *liveSwitchResolverTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	output, err := t.Tool.Execute(ctx, args)
	t.activity.mu.Lock()
	defer t.activity.mu.Unlock()
	switch t.Tool.Spec().Name {
	case tools.LiveSwitchSessionToolName:
		if err == nil {
			t.activity.switchOutput = output.Content
		} else if !controlAttemptRejected(err) {
			t.activity.switchRefusal = err
		}
	case tools.SessionDirectoryToolName:
		if err == nil {
			t.activity.directoryOutput = output.Content
		}
	}
	return output, err
}

func (a *liveSwitchResolverActivity) outcome() liveSwitchResolverOutcome {
	if a == nil {
		return liveSwitchResolverOutcome{Kind: liveSwitchResolverFailOpen}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.switchOutput != "" {
		return liveSwitchResolverOutcome{Kind: liveSwitchResolverSucceeded, Answer: liveSwitchSuccessAnswer(a.switchOutput)}
	}
	if a.switchRefusal != nil {
		return liveSwitchResolverOutcome{Kind: liveSwitchResolverRefused, Answer: liveSwitchRefusalAnswer(a.switchRefusal)}
	}
	if a.directoryOutput != "" {
		return liveSwitchResolverOutcome{Kind: liveSwitchResolverAmbiguous, Answer: liveSwitchCandidatesAnswer(a.directoryOutput)}
	}
	return liveSwitchResolverOutcome{Kind: liveSwitchResolverFailOpen}
}

func liveSwitchSuccessAnswer(output string) string {
	var result struct {
		SessionID string `json:"session_id"`
		Number    int64  `json:"session_number"`
		Title     string `json:"title"`
	}
	if json.Unmarshal([]byte(output), &result) != nil {
		return "The call was switched to the requested session."
	}
	label := strings.TrimSpace(result.Title)
	if result.Number > 0 && label != "" {
		return fmt.Sprintf("Switched the call to session #%d, %s.", result.Number, label)
	}
	if label != "" {
		return "Switched the call to " + label + "."
	}
	if result.Number > 0 {
		return fmt.Sprintf("Switched the call to session #%d.", result.Number)
	}
	return "The call was switched to the requested session."
}

func liveSwitchRefusalAnswer(err error) string {
	return liveControlClipAnswer("I couldn't switch the call: " + strings.TrimSpace(err.Error()) + ".")
}

func liveSwitchCandidatesAnswer(output string) string {
	var result struct {
		Sessions []struct {
			Number int64  `json:"number"`
			Title  string `json:"title"`
		} `json:"sessions"`
	}
	if json.Unmarshal([]byte(output), &result) != nil || len(result.Sessions) == 0 {
		return "Which session did you mean? I couldn't find a clear match."
	}
	labels := make([]string, 0, min(4, len(result.Sessions)))
	for _, candidate := range result.Sessions[:min(4, len(result.Sessions))] {
		label := strings.TrimSpace(candidate.Title)
		if candidate.Number > 0 && label != "" {
			label = fmt.Sprintf("#%d %s", candidate.Number, label)
		} else if candidate.Number > 0 {
			label = fmt.Sprintf("#%d", candidate.Number)
		}
		if label != "" {
			labels = append(labels, label)
		}
	}
	if len(labels) == 0 {
		return "Which session did you mean?"
	}
	return liveControlClipAnswer("Which one did you mean: " + strings.Join(labels, ", ") + "?")
}

// liveRouteResult turns a finished turn into a decision. The two things a turn can
// leave behind are evidence and a handoff, and evidence is checked first.
//
// A control tool that ran settles the question: the request was session management,
// so it must never reach the session lane, whatever the turn did next. That includes
// the turn that reaches for a session tool and then hands the same request to the
// workspace agent anyway ("switch to the reflow one" passed through
// pass_to_workspace): the handoff asks for the one thing this lane exists to prevent
// — the control text becoming a chat turn in the bound session's transcript — and a
// switch would additionally be performed and never spoken about. The accepted cost
// of that precedence is a mixed request ("list my sessions and also fix the parser")
// losing its work half.
//
// A turn that neither acted nor handed anything over has produced no verdict at all.
// An answer with no tool call behind it proves nothing, and an empty one is a
// failure rather than a silent success: the request goes to the session lane with its
// original text, because swallowing a workspace request and speaking an improvised
// reply in its place is the worse of the two mistakes.
func liveRouteResult(handoff *workspaceHandoff, acted *controlActivity, answer string, err error) (live.RouteResult, error) {
	if acted.did() {
		if answer != "" {
			return live.RouteResult{Handled: true, Answer: answer}, nil
		}
		if err == nil {
			err = errors.New("the session assistant acted but did not report what it did")
		}
		return live.RouteResult{}, live.FailControl(err)
	}
	if recorded := handoff.recorded(); recorded != "" {
		return live.RouteResult{Input: recorded}, nil
	}
	if err != nil {
		return live.RouteResult{}, err
	}
	return live.RouteResult{}, nil
}

// workspaceHandoff is the per-request recorder behind pass_to_workspace. It exists
// so the handoff survives the turn that made it — the turn is cancelled the moment
// the request is recorded — and so nothing else in the turn can observe or alter it.
type workspaceHandoff struct {
	cancel context.CancelFunc

	mu      sync.Mutex
	request string
}

// record stores the request for the workspace agent and ends the turn that produced
// it. Cancelling here rather than letting the model send a closing remark is what
// keeps the handoff to a single provider request; the router's own turn has nothing
// left to say that anyone will read.
func (h *workspaceHandoff) record(request string) {
	h.mu.Lock()
	h.request = strings.TrimSpace(request)
	h.mu.Unlock()
	if h.cancel != nil {
		h.cancel()
	}
}

func (h *workspaceHandoff) recorded() string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.request
}

// passToWorkspaceTool hands one request to the workspace agent. It is host-local: it
// is not in any tool registry, needs no authority context, and writes to the
// recorder of the single routing turn that built it.
type passToWorkspaceTool struct {
	handoff *workspaceHandoff
}

// IsFinishingTool marks the handoff as the end of the turn. The cancellation in
// record is the primary stop, and this is the one that holds for providers whose
// tool loop runs outside the turn's context: either way the routing turn ends after
// this call instead of paying for a second provider request.
func (*passToWorkspaceTool) IsFinishingTool() bool { return true }

func (*passToWorkspaceTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: livePassToWorkspaceToolName,
		Description: "Hand this request to the workspace agent, which has the files, shell, and project context. Use it for everything that is not " +
			"about the user's sessions, this call, or its voice. Pass the user's request through unchanged, or cleaned up when speech recognition " +
			"garbled it or it is padded with filler.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"request": map[string]any{
					"type":        "string",
					"description": "The request for the workspace agent, keeping the user's meaning and every detail.",
				},
			},
			"required":             []any{"request"},
			"additionalProperties": false,
		},
	}
}

func (t *passToWorkspaceTool) Execute(_ context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	params := struct {
		Request string `json:"request"`
	}{}
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&params); err != nil {
		return llm.ToolOutput{}, tools.NewToolErrorf(tools.ErrInvalidParams, "invalid handoff: %v", err)
	}
	if strings.TrimSpace(params.Request) == "" {
		return llm.ToolOutput{}, tools.NewToolError(tools.ErrInvalidParams, "a request for the workspace agent is required")
	}
	t.handoff.record(params.Request)
	return llm.TextOutput("Handed to the workspace agent."), nil
}

func (*passToWorkspaceTool) Preview(args json.RawMessage) string {
	params := struct {
		Request string `json:"request"`
	}{}
	if err := json.Unmarshal(args, &params); err != nil || strings.TrimSpace(params.Request) == "" {
		return "Hand the request to the workspace agent"
	}
	return "Hand to the workspace agent: " + liveControlPreviewText(strings.TrimSpace(params.Request))
}

// liveControlPreviewText shortens a handoff request for the one-line tool preview
// the host UI and the voice commentary read. It cuts on a rune boundary for the same
// reason every other bound here does: a half rune is not text.
func liveControlPreviewText(text string) string {
	const limit = 60
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return strings.TrimSpace(text[:cut]) + "…"
}

// liveControlScopedTools are the call-scoped tools: they act on the call and the
// user's sessions, and each one requires the host-built authority context, so a
// schema in a registry grants nothing.
func liveControlScopedTools() []llm.Tool {
	return []llm.Tool{
		&tools.SessionDirectoryTool{},
		&tools.LiveSwitchSessionTool{},
		&tools.LiveNewSessionTool{},
		&tools.LiveSettingsTool{},
	}
}

// liveControlTools is the whole surface one routing turn can reach: the four
// call-scoped tools and the host-local handoff. Registration grants nothing on its
// own — each control tool still requires the host-built context — and no other tool
// is registered or allowed.
func liveControlTools(handoff *workspaceHandoff, acted *controlActivity) []llm.Tool {
	tools := make([]llm.Tool, 0, 5)
	for _, tool := range liveControlScopedTools() {
		tools = append(tools, &controlActivityTool{Tool: tool, acted: acted})
	}
	return append(tools, &passToWorkspaceTool{handoff: handoff})
}

// controlActivity records whether the router reached for a session tool. It is the
// evidence the outcome is decided on: a control tool that ran proves the request was
// session management, which is a fact about what happened rather than a reading of
// what the model wrote afterwards.
type controlActivity struct {
	acted atomic.Bool
}

func (a *controlActivity) did() bool { return a != nil && a.acted.Load() }

// controlActivityTool records how the turn acted and then runs the real tool
// unchanged.
//
// A recorded attempt means the model made a control request the host could act on,
// so the outcome is decided on it. That is true of every result the tool returns —
// a switch refused because the target already hosts another call is still a
// session-management request, and reporting that refusal to the voice model is right
// where re-running the request as workspace work would not be.
//
// It is not true of an attempt that never formed a valid request. Every one of these
// tools decodes with DisallowUnknownFields, so a single hallucinated argument key
// returns ErrInvalidParams; treating that as evidence would turn a workspace request
// into a control failure that never falls open, and the user's work would be lost.
// The same holds for ErrPermissionDenied, which means the host never granted the
// tool at all rather than that the model asked for something. Those two are the
// model failing to make a request; everything else, including every domain error,
// is the model having made one.
type controlActivityTool struct {
	llm.Tool
	acted *controlActivity
}

func (t *controlActivityTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	output, err := t.Tool.Execute(ctx, args)
	if !controlAttemptRejected(err) {
		t.acted.acted.Store(true)
	}
	return output, err
}

// controlAttemptRejected reports whether an error means the model did not manage to
// make a control request, as opposed to making one the host declined or could not
// carry out.
func controlAttemptRejected(err error) bool {
	var toolErr *tools.ToolError
	if !errors.As(err, &toolErr) {
		return false
	}
	return toolErr.Type == tools.ErrInvalidParams || toolErr.Type == tools.ErrPermissionDenied
}

// newLiveControlEngine builds the general router's throwaway engine. The engine
// helper takes the complete tool set explicitly so registration, request schemas,
// and the allowlist can never drift apart.
func newLiveControlEngine(provider llm.Provider, handoff *workspaceHandoff, acted *controlActivity) (*llm.Engine, []llm.ToolSpec) {
	return newLiveToolEngine(provider, liveControlTools(handoff, acted))
}

func newLiveToolEngine(provider llm.Provider, registered []llm.Tool) (*llm.Engine, []llm.ToolSpec) {
	engine := llm.NewEngine(provider, nil)
	specs := make([]llm.ToolSpec, 0, len(registered))
	allowed := make([]string, 0, len(registered))
	for _, tool := range registered {
		engine.RegisterTool(tool)
		spec := tool.Spec()
		specs = append(specs, spec)
		allowed = append(allowed, spec.Name)
	}
	engine.SetAllowedToolsFilter(allowed)
	return engine, specs
}

// newLiveControlProvider resolves the fast model behind the router. It follows the
// automatic-title convention rather than adding configuration: the bound session's
// own durable provider first, the configured default second, and the injectable
// factory for tests.
func (s *serveServer) newLiveControlProvider(ctx context.Context, sessionID string) (llm.Provider, error) {
	providerKey, _ := s.persistedRuntimeIdentity(ctx, sessionID)
	if s.liveControlProviderFactory != nil {
		return s.liveControlProviderFactory(providerKey)
	}
	defaultProvider := ""
	if s.cfgRef != nil {
		defaultProvider = s.cfgRef.DefaultProvider
	}
	provider, model, useFast := liveControlTarget(s.liveConfig(), providerKey, defaultProvider)
	if useFast {
		return s.fastProvider(providerKey)
	}
	chosen, err := llm.NewProviderByName(s.cfgRef, provider, model)
	if err != nil {
		return nil, fmt.Errorf("live control provider %q: %w", provider, err)
	}
	return chosen, nil
}

// liveControlTarget decides which provider and model answer one routing turn.
//
// useFast reports that no dedicated choice was made, in which case the lane follows
// the provider's fast model — the dial it shares with auto-titling, interrupt
// classification and memory mining. A dedicated choice exists so this lane can be
// pointed at a model that calls tools reliably without retuning any of those.
//
// A model named on its own stays with the provider that would have answered anyway:
// naming a model must not silently move the lane to another vendor, and the bound
// conversation's own provider is the one whose credentials are known to work.
func liveControlTarget(cfg config.LiveConfig, sessionProvider, defaultProvider string) (provider, model string, useFast bool) {
	provider = strings.TrimSpace(cfg.ControlProvider)
	model = strings.TrimSpace(cfg.ControlModel)
	if provider == "" && model == "" {
		return "", "", true
	}
	if provider == "" {
		provider = strings.TrimSpace(sessionProvider)
		if provider == "" {
			provider = strings.TrimSpace(defaultProvider)
		}
	}
	return provider, model, false
}

// liveControlUserMessage frames one request with the state the router needs to
// answer "which session is this?" and to tell a real switch from a no-op, which is
// exactly the per-request state the static system prompt must not carry. The recent
// conversation is included because the user points at things they just heard ("the
// attachment icons one") far more often than they name them.
func (s *serveServer) liveControlUserMessage(ctx context.Context, sessionID string, request live.RouteRequest) string {
	var b strings.Builder
	b.WriteString("Request: ")
	b.WriteString(strings.TrimSpace(request.Input))
	if delta := strings.TrimSpace(request.TranscriptDelta); delta != "" {
		b.WriteString("\nRecent conversation:\n")
		b.WriteString(liveControlTranscriptTail(delta))
	}
	b.WriteString("\nCurrent call binding: ")
	b.WriteString(s.liveControlBindingLabel(ctx, sessionID))
	b.WriteString(".")
	return b.String()
}

// liveControlTranscriptTail keeps the most recent end of a transcript delta on a
// rune boundary. The recent end is the part a reference points back to.
func liveControlTranscriptTail(delta string) string {
	if len(delta) <= liveControlTranscriptLimitBytes {
		return delta
	}
	cut := len(delta) - liveControlTranscriptLimitBytes
	for cut < len(delta) && !utf8.RuneStart(delta[cut]) {
		cut++
	}
	return "…" + delta[cut:]
}

// liveControlBindingLabel names the session the call drives right now. It degrades
// to the raw id and then to "unknown session" rather than failing: an unreadable
// store must not turn a spoken switch into an error.
func (s *serveServer) liveControlBindingLabel(ctx context.Context, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "unknown session"
	}
	label := sessionID
	if s == nil || s.store == nil {
		return label
	}
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil || sess == nil {
		return label
	}
	if sess.Number > 0 {
		label = fmt.Sprintf("#%d", sess.Number)
	}
	if title := strings.TrimSpace(sess.PreferredShortTitle()); title != "" {
		label += fmt.Sprintf(" %q", title)
	}
	return label
}

// liveControlClipAnswer shortens an over-long answer on a rune boundary. The
// ellipsis matters: a truncated answer that reads as complete would have the voice
// model recite an unfinished sentence.
func liveControlClipAnswer(text string) string {
	if len(text) <= liveControlAnswerLimitBytes {
		return text
	}
	cut := liveControlAnswerLimitBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return strings.TrimSpace(text[:cut]) + "…"
}

// withLiveControlAuthority builds the tool authority for a routing turn: the
// session-control bindings on top of the settings binding every live turn gets. The
// authority is minted here and pinned to the session the call is bound to when the
// turn starts, then installed on the turn's context: only the host constructs it, so
// a model cannot forge it, and the four control tools validate against it rather
// than against the schema they registered. pass_to_workspace is not part of it — it
// needs no authority, because it only reports a request back to the host that built
// the turn.
//
// The flag is checked here as well as at the wiring, which is what keeps "no
// call-scoped control tools outside the control plane" a property of the code rather
// than of which call sites exist today: without it the three session-control tools
// cannot be installed by any caller.
func (s *serveServer) withLiveControlAuthority(ctx context.Context, record *liveSession, sessionID string) context.Context {
	ctx = s.withLiveSettingsContext(llm.ContextWithSessionID(ctx, sessionID), record, sessionID)
	if record == nil || sessionID == "" || !s.liveConfig().ControlAuthorityAllowed() {
		return ctx
	}
	ctx = tools.ContextWithSessionDirectory(ctx, sessionID, s.sessionDirectoryForTool)
	ctx = tools.ContextWithLiveNewSession(ctx, sessionID, s.liveNewSessionForTool(record))
	return tools.ContextWithLiveSwitchSession(ctx, sessionID, s.liveSwitchSessionForTool(record))
}
