package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

// Anthropic credential mode constants for the config "credentials" field.
// These control which authentication method is used. "auto" (or empty) uses
// the default cascade; any other value forces that specific method.
const (
	AnthropicCredAuto   = "auto"    // Default cascade: api_key → env
	AnthropicCredAPIKey = "api_key" // Force: explicit api_key from config only
	AnthropicCredEnv    = "env"     // Force: ANTHROPIC_API_KEY env var only
)

// AnthropicProvider implements Provider using the Anthropic API.
type AnthropicProvider struct {
	client          *anthropicClient
	model           string
	thinkingBudget  int64  // 0 = disabled, >0 = enabled with budget
	useAdaptive     bool   // true = adaptive thinking (-thinking on adaptive-capable models)
	use1m           bool   // true = 1M token context window (-1m suffix)
	reasoningEffort string // default output_config.effort from model suffix, if any
	// resolveModelID maps a suffix-stripped model name to its wire ID (Bedrock
	// model_map / geographic prefix). Nil sends the name unchanged.
	resolveModelID func(string) string
	credential     string // "api_key" or "env"
}

// isAdaptiveModel reports whether -thinking uses adaptive thinking for model:
// the live catalog decides when cached, otherwise every model newer than the
// budget-only Claude 4.5 generation is treated as adaptive.
func isAdaptiveModel(model string) bool {
	return anthropicSupportsAdaptiveThinking(model)
}

// parseModelThinking extracts -thinking suffix from model name.
// For adaptive-capable models, -thinking uses adaptive thinking
// (budget_tokens is deprecated). For older models, -thinking uses
// budget_tokens as before.
//
// "claude-sonnet-4-6-thinking" -> ("claude-sonnet-4-6", 0, true)
// "claude-haiku-4-5-thinking"  -> ("claude-haiku-4-5", 10000, false)
// "claude-sonnet-4-6"          -> ("claude-sonnet-4-6", 0, false)
func parseModelThinking(model string) (string, int64, bool) {
	if strings.HasSuffix(model, "-thinking") {
		base := strings.TrimSuffix(model, "-thinking")
		if isAdaptiveModel(base) {
			return base, 0, true
		}
		return base, 10000, false
	}
	return model, 0, false
}

// the1mBetaHeader is the beta header that enables the 1M token context window.
// Available for claude-sonnet-4-6, claude-sonnet-4-5, claude-sonnet-4, claude-opus-4-6, claude-opus-4-7.
// Requires Anthropic usage tier 4 or custom rate limits.
const the1mBetaHeader = "context-1m-2025-08-07"

// parseModel1m extracts the -1m suffix from a model name.
// Returns the base model name and whether 1M context is requested.
//
// "claude-sonnet-4-6-1m"         -> ("claude-sonnet-4-6", true)
// "claude-sonnet-4-6-1m-thinking" is handled upstream (thinking stripped first)
// "claude-sonnet-4-6"            -> ("claude-sonnet-4-6", false)
func parseModel1m(model string) (string, bool) {
	if strings.HasSuffix(model, "-1m") {
		return strings.TrimSuffix(model, "-1m"), true
	}
	return model, false
}

// NewAnthropicProvider creates a new Anthropic provider using Anthropic's default API endpoint.
func NewAnthropicProvider(apiKey, model, credentialMode string) (*AnthropicProvider, error) {
	return NewAnthropicProviderWithBaseURL(apiKey, model, credentialMode, "")
}

// NewAnthropicProviderWithBaseURL creates a new Anthropic provider for baseURL
// (empty uses ANTHROPIC_BASE_URL, then Anthropic's public endpoint).
func NewAnthropicProviderWithBaseURL(apiKey, model, credentialMode, baseURL string) (*AnthropicProvider, error) {
	return NewAnthropicProviderWithOptions(apiKey, model, credentialMode, AnthropicProviderOptions{BaseURL: baseURL})
}

// AnthropicProviderOptions holds optional transport settings for the Anthropic provider.
type AnthropicProviderOptions struct {
	BaseURL string
	// Headers are extra HTTP headers sent with every request (for example
	// anthropic-workspace-id). Entries with empty values are omitted.
	Headers map[string]string
}

// NewAnthropicProviderWithOptions creates a new Anthropic provider.
// The credentialMode parameter controls which authentication method is used:
//   - "" or "auto": try the cascade (api_key → env)
//   - "api_key":    use only the explicit apiKey parameter
//   - "env":        use only the ANTHROPIC_API_KEY environment variable
func NewAnthropicProviderWithOptions(apiKey, model, credentialMode string, opts AnthropicProviderOptions) (*AnthropicProvider, error) {
	// Strip provider-aware reasoning effort first, then -thinking (may leave -1m), then -1m.
	// This means claude-sonnet-4-6-1m-thinking works correctly:
	//   step 1: strip effort   -> "claude-sonnet-4-6-1m-thinking"
	//   step 2: strip thinking -> "claude-sonnet-4-6-1m", adaptive=true
	//   step 3: strip -1m      -> "claude-sonnet-4-6",    use1m=true
	spec := parseAnthropicModelSpec(model, nil)

	// Normalize empty credential mode to "auto"
	if credentialMode == "" {
		credentialMode = AnthropicCredAuto
	}

	baseURL := strings.TrimSpace(opts.BaseURL)
	if baseURL != "" {
		u, err := url.Parse(baseURL)
		if err != nil {
			return nil, fmt.Errorf("invalid Anthropic base_url %q: %w", baseURL, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("invalid Anthropic base_url %q: must include scheme and host", baseURL)
		}
	}
	headers, err := newAnthropicExtraHeaders(opts.Headers)
	if err != nil {
		return nil, err
	}
	mkClient := func(key string) (*anthropicClient, error) {
		client, err := newAnthropicClient(key, baseURL, nil)
		if client != nil {
			client.headers = headers
		}
		return client, err
	}

	mkProvider := func(client *anthropicClient, err error, cred string) (*AnthropicProvider, error) {
		if err != nil {
			return nil, err
		}
		return &AnthropicProvider{
			client:          client,
			model:           spec.model,
			thinkingBudget:  spec.thinkingBudget,
			useAdaptive:     spec.adaptive,
			use1m:           spec.use1m,
			reasoningEffort: spec.effort,
			credential:      cred,
		}, nil
	}

	// When a specific mode is forced, only try that one source.
	switch credentialMode {
	case AnthropicCredAPIKey:
		if apiKey == "" {
			return nil, fmt.Errorf("credentials mode %q requires an explicit api_key in provider config", credentialMode)
		}
		client, err := mkClient(apiKey)
		return mkProvider(client, err, "api_key")

	case AnthropicCredEnv:
		envKey := os.Getenv("ANTHROPIC_API_KEY")
		if envKey == "" {
			return nil, fmt.Errorf("credentials mode %q requires ANTHROPIC_API_KEY environment variable", credentialMode)
		}
		client, err := mkClient(envKey)
		return mkProvider(client, err, "env")

	case AnthropicCredAuto:
		// Fall through to the cascade below.

	default:
		return nil, fmt.Errorf("unknown Anthropic credentials mode: %q (valid: auto, api_key, env)", credentialMode)
	}

	// Auto mode: full credential cascade.

	// 1. Explicit API key provided (from config)
	if apiKey != "" {
		client, err := mkClient(apiKey)
		return mkProvider(client, err, "api_key")
	}

	// 2. ANTHROPIC_API_KEY environment variable
	if envKey := os.Getenv("ANTHROPIC_API_KEY"); envKey != "" {
		client, err := mkClient(envKey)
		return mkProvider(client, err, "env")
	}

	return nil, fmt.Errorf("no Anthropic credentials found. Set ANTHROPIC_API_KEY or configure api_key in provider config")
}

func (p *AnthropicProvider) Name() string {
	suffix := ""
	if p.use1m {
		suffix = ", 1m"
	}
	if p.useAdaptive {
		return fmt.Sprintf("Anthropic (%s, adaptive%s)", p.model, suffix)
	}
	if p.thinkingBudget > 0 {
		return fmt.Sprintf("Anthropic (%s, thinking=%dk%s)", p.model, p.thinkingBudget/1000, suffix)
	}
	if p.use1m {
		return fmt.Sprintf("Anthropic (%s, 1m)", p.model)
	}
	return fmt.Sprintf("Anthropic (%s)", p.model)
}

func (p *AnthropicProvider) Credential() string {
	return p.credential
}

func (p *AnthropicProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch:    true,
		NativeWebFetch:     true,
		ToolCalls:          true,
		SupportsToolChoice: true,
	}
}

func (p *AnthropicProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	spec := p.requestSpec(req)
	if p.client.bedrock == nil {
		req.MaxOutputTokens = anthropicClampOutputTokens(req.MaxOutputTokens, spec.model)
	} else {
		// Bedrock limits can differ from the direct API catalog.
		req.MaxOutputTokens = ClampOutputTokens(req.MaxOutputTokens, spec.model)
	}
	if req.Search {
		return p.streamWithSearch(ctx, req, spec)
	}
	return p.streamStandardForModel(ctx, req, spec, true, nil)
}

// anthropicModelSpec is a term-llm model string decoded into the wire model
// ID and the options its suffixes select.
type anthropicModelSpec struct {
	model          string
	effort         string
	thinkingBudget int64 // 0 = disabled
	adaptive       bool
	use1m          bool
}

// parseAnthropicModelSpec strips the effort suffix, then -thinking (which may
// leave -1m), then -1m, e.g. claude-sonnet-4-6-1m-thinking-high. resolveID
// maps the stripped name to its wire ID; the thinking mode is decided on the
// resolved ID because an alias says nothing about the target's capabilities.
func parseAnthropicModelSpec(model string, resolveID func(string) string) anthropicModelSpec {
	base, effort := BaseModelAndEffortForProvider("anthropic", model)
	if base == "" {
		base = model
	}
	afterThinking, thinking := strings.CutSuffix(base, "-thinking")
	id, use1m := parseModel1m(afterThinking)
	if resolveID != nil {
		id = resolveID(id)
	}
	spec := anthropicModelSpec{model: id, effort: effort, use1m: use1m}
	if thinking {
		_, spec.thinkingBudget, spec.adaptive = parseModelThinking(id + "-thinking")
	}
	return spec
}

// requestSpec resolves the model and options for one request. A request
// model (the engine passes the configured name, or a runtime switch target)
// is decoded exactly like the configured one so suffixes and Bedrock aliases
// never reach the API verbatim.
func (p *AnthropicProvider) requestSpec(req Request) anthropicModelSpec {
	spec := anthropicModelSpec{model: p.model, effort: strings.TrimSpace(p.reasoningEffort), thinkingBudget: p.thinkingBudget, adaptive: p.useAdaptive, use1m: p.use1m}
	if requested := strings.TrimSpace(req.Model); requested != "" {
		parsed := parseAnthropicModelSpec(requested, p.resolveModelID)
		if parsed.model == p.model {
			// Same model: explicit request suffixes add to the configured ones.
			if parsed.thinkingBudget > 0 || parsed.adaptive {
				spec.thinkingBudget, spec.adaptive = parsed.thinkingBudget, parsed.adaptive
			}
			spec.use1m = spec.use1m || parsed.use1m
		} else {
			spec.model, spec.thinkingBudget, spec.adaptive, spec.use1m = parsed.model, parsed.thinkingBudget, parsed.adaptive, parsed.use1m
		}
		if parsed.effort != "" {
			spec.effort = parsed.effort
		}
	}
	if reqEffort := strings.TrimSpace(req.ReasoningEffort); reqEffort != "" {
		spec.effort = reqEffort
	}
	return spec
}

// requestModelAndEffort returns the wire model and effort for req.
func (p *AnthropicProvider) requestModelAndEffort(req Request) (model string, effort string) {
	spec := p.requestSpec(req)
	return spec.model, spec.effort
}

// streamStandardForModel streams a Messages request. extraHeaders lets callers
// attach per-request transport concerns (for example a gateway's session
// attribution headers) that are not known when the client is built.
func (p *AnthropicProvider) streamStandardForModel(ctx context.Context, req Request, spec anthropicModelSpec, includeOutputEffort bool, extraHeaders http.Header) (Stream, error) {
	model, reasoningEffort, thinkingBudget, useAdaptive := spec.model, spec.effort, spec.thinkingBudget, spec.adaptive
	if req.MaxOutputTokens > 0 && thinkingBudget >= int64(req.MaxOutputTokens) {
		thinkingBudget = int64(req.MaxOutputTokens - 1)
	}
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		system, messages := buildAnthropicMessages(req.Messages)
		applyLastMessageCacheControl(messages)

		params := &anthropicMessagesRequest{
			Model:     model,
			MaxTokens: maxTokens(req.MaxOutputTokens, 4096),
			Messages:  messages,
			System:    anthropicSystemBlocks(system),
		}
		if len(req.Tools) > 0 {
			params.Tools = buildAnthropicTools(req.Tools, false)
			if thinkingBudget == 0 && !useAdaptive {
				params.ToolChoice = buildAnthropicToolChoice(req.ToolChoice, req.ParallelToolCalls)
			}
		}
		applyAnthropicThinking(params, req.MaxOutputTokens, thinkingBudget, useAdaptive, includeOutputEffort && anthropicSupportsEffort(model, "max"), true)
		applyAnthropicEffort(params, reasoningEffort)

		if req.Debug {
			runtimeoutput.Printf("%s\n", "=== DEBUG: Anthropic Stream Request ===")
			runtimeoutput.Printf("Provider: %s\n", p.Name())
			runtimeoutput.Printf("System: %s\n", truncate(system, 200))
			runtimeoutput.Printf("Messages: %d\n", len(messages))
			runtimeoutput.Printf("Tools: %d\n", len(req.Tools))
			runtimeoutput.Printf("%s\n", "======================================")
		}

		call := anthropicCall{headers: extraHeaders}
		if spec.use1m {
			call.betas = append(call.betas, the1mBetaHeader)
		}
		return p.runMessagesStream(ctx, send, params, call, false)
	}), nil
}

func (p *AnthropicProvider) streamWithSearch(ctx context.Context, req Request, spec anthropicModelSpec) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		system, messages := buildAnthropicMessages(req.Messages)
		applyLastMessageCacheControl(messages)

		tools := append([]anthropicTool{
			{Type: "web_search_20250305", Name: "web_search", MaxUses: 5},
			{Type: "web_fetch_20250910", Name: "web_fetch", MaxUses: 3},
		}, buildAnthropicTools(req.Tools, true)...)

		betas := []string{"web-search-2025-03-05", "web-fetch-2025-09-10"}
		if spec.use1m {
			betas = append(betas, the1mBetaHeader)
		}
		model, reasoningEffort := spec.model, spec.effort
		params := &anthropicMessagesRequest{
			Model:     model,
			MaxTokens: maxTokens(req.MaxOutputTokens, 4096),
			Messages:  messages,
			System:    anthropicSystemBlocks(system),
			Tools:     tools,
		}
		// In search mode, use auto tool choice so model can call web_search first
		// The model will call the user's requested tool after searching
		if len(req.Tools) > 0 && spec.thinkingBudget == 0 && !spec.adaptive {
			disableParallel := !req.ParallelToolCalls
			params.ToolChoice = &anthropicToolChoice{Type: "auto", DisableParallelToolUse: &disableParallel}
		}
		applyAnthropicThinking(params, req.MaxOutputTokens, spec.thinkingBudget, spec.adaptive, anthropicSupportsEffort(model, "max"), false)
		applyAnthropicEffort(params, reasoningEffort)

		if req.Debug {
			runtimeoutput.Printf("%s\n", "=== DEBUG: Anthropic Stream Request (search) ===")
			runtimeoutput.Printf("Provider: %s\n", p.Name())
			runtimeoutput.Printf("System: %s\n", truncate(system, 200))
			runtimeoutput.Printf("Messages: %d\n", len(messages))
			runtimeoutput.Printf("Tools: %d (includes web_search, web_fetch)\n", len(tools))
			runtimeoutput.Printf("%s\n", "================================================")
		}

		return p.runMessagesStream(ctx, send, params, anthropicCall{beta: true, betas: betas}, true)
	}), nil
}

func anthropicSystemBlocks(system string) []*anthropicTextBlock {
	if system == "" {
		return nil
	}
	block := newAnthropicTextBlock(system)
	block.CacheControl = newAnthropicEphemeralCache()
	return []*anthropicTextBlock{block}
}

// anthropicSupportsEffort reports whether model accepts output_config.effort
// level. Budget thinking pairs with effort "max" only where the model accepts
// it; budget-only Claude 4.5 models reject the field (or the level) outright.
func anthropicSupportsEffort(model, level string) bool {
	return slices.Contains(anthropicReasoningEfforts(model), level)
}

// applyAnthropicThinking configures extended thinking. budgetFallbackFromBudget
// mirrors the standard endpoint, which raises the default max_tokens above a
// large thinking budget; the search path always uses a 16000 default.
func applyAnthropicThinking(params *anthropicMessagesRequest, requestedMax int, thinkingBudget int64, useAdaptive, includeOutputEffort, budgetFallbackFromBudget bool) {
	switch {
	case useAdaptive:
		params.MaxTokens = maxTokens(requestedMax, 16000)
		params.Thinking = &anthropicThinking{Type: "adaptive"}
	case thinkingBudget > 0:
		fallbackMaxTokens := 16000
		if budgetFallbackFromBudget && thinkingBudget >= int64(fallbackMaxTokens) {
			fallbackMaxTokens = int(thinkingBudget + 1)
		}
		params.MaxTokens = maxTokens(requestedMax, fallbackMaxTokens)
		params.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: thinkingBudget}
		if includeOutputEffort {
			params.OutputConfig = &anthropicOutputConfig{Effort: "max"}
		}
	}
}

func applyAnthropicEffort(params *anthropicMessagesRequest, reasoningEffort string) {
	if eff := strings.TrimSpace(reasoningEffort); eff != "" {
		params.OutputConfig = &anthropicOutputConfig{Effort: strings.ToLower(eff)}
	}
}

// anthropicStreamState translates Messages stream events into term-llm events.
type anthropicStreamState struct {
	send        eventSender
	accumulator *toolCallAccumulator
	// serverTools enables server_tool_use tracking (beta web search/fetch).
	serverTools            bool
	currentServerTool      string
	currentServerToolIndex int64
	lastUsage              *Usage
	sawMessageStop         bool
}

// errAnthropicStream marks a protocol-level stream failure (as opposed to a
// failure to deliver an event to the consumer).
type errAnthropicStream struct{ err error }

func (e errAnthropicStream) Error() string { return e.err.Error() }

func (p *AnthropicProvider) runMessagesStream(ctx context.Context, send eventSender, params *anthropicMessagesRequest, call anthropicCall, serverTools bool) error {
	state := &anthropicStreamState{send: send, accumulator: newToolCallAccumulator(), serverTools: serverTools, currentServerToolIndex: -1}
	streamErr := p.consumeMessagesStream(ctx, state, params, call)
	var protocolErr errAnthropicStream
	if streamErr != nil && !errors.As(streamErr, &protocolErr) {
		return streamErr
	}
	if protocolErr.err != nil {
		streamErr = protocolErr.err
	}
	if err := validateAnthropicStreamEnd(streamErr, state.sawMessageStop); err != nil {
		return finishAnthropicServerToolFailure(send, state.currentServerTool, err)
	}
	if err := state.endServerTool(); err != nil {
		return err
	}
	if state.lastUsage != nil {
		if err := send.Send(Event{Type: EventUsage, Use: state.lastUsage}); err != nil {
			return err
		}
	}
	return send.Send(Event{Type: EventDone})
}

// consumeMessagesStream returns errAnthropicStream for transport/protocol
// failures and plain errors for consumer delivery failures.
func (p *AnthropicProvider) consumeMessagesStream(ctx context.Context, state *anthropicStreamState, params *anthropicMessagesRequest, call anthropicCall) error {
	events, err := p.client.streamMessages(ctx, params, call)
	if err != nil {
		return errAnthropicStream{err: err}
	}
	defer events.Close()
	for {
		name, data, err := events.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errAnthropicStream{err: err}
		}
		if err := state.handleRaw(name, data); err != nil {
			return err
		}
	}
}

func (s *anthropicStreamState) handleRaw(name string, data []byte) error {
	var ev anthropicStreamEvent
	if name == "" {
		// Unnamed events are dispatched on their JSON type; undecodable ones
		// are ignored like any other unknown event.
		if json.Unmarshal(data, &ev) != nil {
			return nil
		}
		name = ev.Type
	} else if name == "error" || isAnthropicMessageEvent(name) {
		if err := json.Unmarshal(data, &ev); err != nil {
			return errAnthropicStream{err: fmt.Errorf("decode %s event: %w", name, err)}
		}
	}
	switch {
	case name == "error":
		return errAnthropicStream{err: newAnthropicStreamError(ev.Error.Type, ev.Error.Message)}
	case isAnthropicMessageEvent(name):
		return s.handle(ev)
	}
	return nil // ping and unknown events
}

func isAnthropicMessageEvent(name string) bool {
	switch name {
	case "message_start", "message_delta", "message_stop", "content_block_start", "content_block_delta", "content_block_stop":
		return true
	}
	return false
}

func (s *anthropicStreamState) handle(ev anthropicStreamEvent) error {
	switch ev.Type {
	case "content_block_delta":
		return s.handleDelta(ev.Index, ev.Delta)
	case "content_block_start":
		return s.handleBlockStart(ev.Index, ev.ContentBlock)
	case "content_block_stop":
		if s.currentServerTool != "" && ev.Index == s.currentServerToolIndex {
			if err := s.endServerTool(); err != nil {
				return err
			}
		}
		if toolCall, ok := s.accumulator.Finish(ev.Index); ok {
			return s.send.Send(Event{Type: EventToolCall, Tool: &toolCall})
		}
	case "message_start":
		usage := ev.Message.Usage
		s.lastUsage = &Usage{
			InputTokens:       int(usage.InputTokens),
			CachedInputTokens: int(usage.CacheReadInputTokens),
			CacheWriteTokens:  int(usage.CacheCreationInputTokens),
		}
	case "message_delta":
		s.mergeDeltaUsage(ev.Usage)
	case "message_stop":
		s.sawMessageStop = true
	}
	return nil
}

func (s *anthropicStreamState) mergeDeltaUsage(usage anthropicUsage) {
	if usage.OutputTokens <= 0 {
		return
	}
	if s.lastUsage == nil {
		s.lastUsage = &Usage{}
	}
	s.lastUsage.OutputTokens = int(usage.OutputTokens)
	if s.lastUsage.InputTokens == 0 && usage.InputTokens > 0 {
		s.lastUsage.InputTokens = int(usage.InputTokens)
	}
	if s.lastUsage.CachedInputTokens == 0 {
		s.lastUsage.CachedInputTokens = int(usage.CacheReadInputTokens)
	}
	if s.lastUsage.CacheWriteTokens == 0 {
		s.lastUsage.CacheWriteTokens = int(usage.CacheCreationInputTokens)
	}
}

func (s *anthropicStreamState) handleDelta(index int64, delta anthropicDelta) error {
	switch delta.Type {
	case "input_json_delta":
		s.accumulator.Append(index, delta.PartialJSON)
	case "text_delta":
		if delta.Text == "" {
			return nil
		}
		if err := s.endServerTool(); err != nil {
			return err
		}
		return s.send.Send(Event{Type: EventTextDelta, Text: delta.Text})
	case "thinking_delta":
		return emitReasoningDelta(s.send, delta.Thinking, "")
	case "signature_delta":
		return emitReasoningDelta(s.send, "", delta.Signature)
	}
	return nil
}

func (s *anthropicStreamState) handleBlockStart(index int64, block anthropicResponseBlock) error {
	if s.serverTools && block.Type == "server_tool_use" {
		// Server tool (web_search, etc.) is starting
		s.currentServerTool = block.Name
		s.currentServerToolIndex = index
		return s.send.Send(Event{Type: EventToolExecStart, ToolName: block.Name})
	}
	return handleAnthropicStartBlockContent(s.send, block, index, s.accumulator)
}

// endServerTool reports successful completion of the active server tool, if any.
func (s *anthropicStreamState) endServerTool() error {
	if s.currentServerTool == "" {
		return nil
	}
	name := s.currentServerTool
	s.currentServerTool = ""
	s.currentServerToolIndex = -1
	return s.send.Send(Event{Type: EventToolExecEnd, ToolName: name, ToolSuccess: true})
}

func validateAnthropicStreamEnd(streamErr error, sawMessageStop bool) error {
	// The Anthropic HTTP decoder reports a clean terminal read as nil, while
	// the shared Bedrock eventstream decoder reports io.EOF after message_stop.
	// Normalize that transport difference only after validating the protocol's
	// required terminal event so a truncated Bedrock stream cannot become Done.
	if streamErr != nil && !errors.Is(streamErr, io.EOF) {
		// A body cut mid-frame is a truncated stream, not a protocol error, and
		// is as retryable as a clean EOF without message_stop.
		if errors.Is(streamErr, io.ErrUnexpectedEOF) && !sawMessageStop {
			return &StreamIncompleteError{Transport: "Anthropic SSE", Terminal: "message_stop"}
		}
		return fmt.Errorf("anthropic streaming error: %w", streamErr)
	}
	if !sawMessageStop {
		return &StreamIncompleteError{Transport: "Anthropic SSE", Terminal: "message_stop"}
	}
	return nil
}

func finishAnthropicServerToolFailure(send eventSender, toolName string, streamErr error) error {
	if toolName == "" {
		return streamErr
	}
	if sendErr := send.Send(Event{Type: EventToolExecEnd, ToolName: toolName, ToolSuccess: false}); sendErr != nil {
		return errors.Join(streamErr, sendErr)
	}
	return streamErr
}

func buildAnthropicMessages(messages []Message) (string, []anthropicMessage) {
	messages = prepareAnthropicMessages(messages)

	var systemParts []string
	var out []anthropicMessage
	var pendingDev string

	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			systemParts = append(systemParts, collectTextParts(msg.Parts))
		case RoleDeveloper:
			// Anthropic has no native developer role. Buffer the text and prepend
			// it into the next user turn wrapped in <developer> tags.
			pendingDev = appendAnthropicDeveloperText(pendingDev, collectTextParts(msg.Parts))
		case RoleUser, RoleTool:
			parts := msg.Parts
			if pendingDev != "" {
				parts = prependTextToParts(fmt.Sprintf("<developer>\n%s\n</developer>\n\n", pendingDev), parts)
				pendingDev = ""
			}
			blocks := buildAnthropicBlocks(parts, false)
			if len(blocks) > 0 {
				if msg.Role == RoleUser && msg.CacheAnchor {
					applyCacheControlToLastBlock(blocks)
				}
				out = append(out, anthropicMessage{Role: "user", Content: blocks})
			}
		case RoleAssistant:
			out = flushAnthropicDeveloper(out, pendingDev)
			pendingDev = ""
			blocks := buildAnthropicBlocks(msg.Parts, true)
			if len(blocks) > 0 {
				out = append(out, anthropicMessage{Role: "assistant", Content: blocks})
			}
		}
	}
	out = flushAnthropicDeveloper(out, pendingDev)

	return strings.Join(systemParts, "\n\n"), out
}

func appendAnthropicDeveloperText(pending, text string) string {
	if text == "" {
		return pending
	}
	if pending == "" {
		return text
	}
	return pending + "\n\n" + text
}

func flushAnthropicDeveloper(messages []anthropicMessage, pending string) []anthropicMessage {
	if pending == "" {
		return messages
	}
	block := newAnthropicTextBlock(fmt.Sprintf("<developer>\n%s\n</developer>", pending))
	if len(messages) > 0 && messages[len(messages)-1].Role == "user" {
		messages[len(messages)-1].Content = append(messages[len(messages)-1].Content, block)
		return messages
	}
	return append(messages, anthropicMessage{Role: "user", Content: []anthropicBlock{block}})
}

// prependTextToParts prepends prefix to the first PartText part in parts,
// or inserts a new text part at the front if none exists.
func prependTextToParts(prefix string, parts []Part) []Part {
	for i, p := range parts {
		if p.Type == PartText {
			out := make([]Part, len(parts))
			copy(out, parts)
			out[i].Text = prefix + out[i].Text
			return out
		}
	}
	return append([]Part{{Type: PartText, Text: prefix}}, parts...)
}

func prepareAnthropicMessages(messages []Message) []Message {
	messages = sanitizeToolHistory(messages)
	if len(messages) == 0 {
		return nil
	}

	lastRole := messages[len(messages)-1].Role
	if lastRole != RoleAssistant {
		return messages
	}

	normalized := append([]Message(nil), messages...)
	// Anthropic treats a trailing assistant turn as response prefill. That is
	// deprecated and unsupported on newer Claude models, so convert assistant-
	// ended histories into a normal assistant->user continuation turn.
	normalized = append(normalized, UserText("Continue from the conversation state above."))
	return normalized
}

func buildAnthropicBlocks(parts []Part, allowToolUse bool) []anthropicBlock {
	blocks := make([]anthropicBlock, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case PartText, PartFile:
			if allowToolUse && part.ReasoningEncryptedContent != "" {
				blocks = append(blocks, &anthropicThinkingBlock{Type: "thinking", Signature: part.ReasoningEncryptedContent, Thinking: part.ReasoningContent})
			}
			if part.Text != "" {
				blocks = append(blocks, newAnthropicTextBlock(part.Text))
			}
		case PartImage:
			if part.ImageData != nil && strings.TrimSpace(part.ImageData.Base64) != "" {
				blocks = append(blocks, newAnthropicBase64ImageBlock(part.ImageData.MediaType, part.ImageData.Base64))
				if part.ImagePath != "" {
					blocks = append(blocks, newAnthropicTextBlock("[image saved at: "+part.ImagePath+"]"))
				}
			}
		case PartToolCall:
			if allowToolUse && part.ToolCall != nil {
				blocks = append(blocks, &anthropicToolUseBlock{Type: "tool_use", ID: part.ToolCall.ID, Name: part.ToolCall.Name, Input: anthropicToolInput(part.ToolCall.Arguments)})
			}
		case PartToolResult:
			if part.ToolResult != nil {
				blocks = append(blocks, toolResultBlock(part.ToolResult))
			}
		}
	}
	return blocks
}

type anthropicResultContent struct {
	text       string
	mimeType   string
	base64Data string
}

func normalizedAnthropicResultContent(result *ToolResult) []anthropicResultContent {
	var content []anthropicResultContent
	for _, part := range toolResultContentParts(result) {
		switch part.Type {
		case ToolContentPartText:
			if part.Text != "" {
				content = append(content, anthropicResultContent{text: part.Text})
			}
		case ToolContentPartImageData:
			mimeType, base64Data, ok := toolResultImageData(part)
			if ok {
				content = append(content, anthropicResultContent{mimeType: mimeType, base64Data: base64Data})
			}
		}
	}
	if len(content) == 0 {
		content = append(content, anthropicResultContent{text: toolResultTextContent(result)})
	}
	return content
}

// anthropicToolInput returns the replayed tool_use input; a call that streamed
// no (or null) arguments is sent as {} because the API rejects a null input.
func anthropicToolInput(args json.RawMessage) json.RawMessage {
	if trimmed := bytes.TrimSpace(args); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage("{}")
	}
	return args
}

// toolResultBlock creates a tool result block with structured image support.
func toolResultBlock(result *ToolResult) *anthropicToolResultBlock {
	content := normalizedAnthropicResultContent(result)
	blocks := make([]anthropicBlock, 0, len(content))
	for _, part := range content {
		switch {
		case part.text != "":
			blocks = append(blocks, newAnthropicTextBlock(part.text))
		case part.base64Data != "":
			blocks = append(blocks, newAnthropicBase64ImageBlock(part.mimeType, part.base64Data))
		}
		// Empty output yields a tool_result without content; the API rejects
		// both empty text blocks and data-less image blocks.
	}
	if len(blocks) == 0 {
		blocks = nil // omit "content" entirely
	}
	return &anthropicToolResultBlock{Type: "tool_result", ToolUseID: result.ID, IsError: result.IsError, Content: blocks}
}

// buildAnthropicTools converts tool specs. The beta endpoint always sends a
// description (possibly empty); the standard endpoint omits empty ones.
func buildAnthropicTools(specs []ToolSpec, beta bool) []anthropicTool {
	if len(specs) == 0 {
		return nil
	}
	tools := make([]anthropicTool, 0, len(specs))
	for _, spec := range specs {
		tool := anthropicTool{
			Name: spec.Name,
			InputSchema: &anthropicInputSchema{
				Type:       "object",
				Properties: spec.Schema["properties"],
				Required:   schemaRequired(spec.Schema),
			},
		}
		if beta || spec.Description != "" {
			description := spec.Description
			tool.Description = &description
		}
		tools = append(tools, tool)
	}
	tools[len(tools)-1].CacheControl = newAnthropicEphemeralCache()
	return tools
}

// applyLastMessageCacheControl marks the last content block of the last message
// for caching. This enables incremental conversation caching: each turn, the
// prior conversation becomes a cache hit and only the new turn is processed fresh.
func applyLastMessageCacheControl(messages []anthropicMessage) {
	if len(messages) == 0 {
		return
	}
	applyCacheControlToLastBlock(messages[len(messages)-1].Content)
}

// applyCacheControlToLastBlock applies cache_control: ephemeral to the last block
// in a slice of Anthropic content blocks when that block type supports it. Used
// for both the rolling per-turn breakpoint and the stable summary anchor.
func applyCacheControlToLastBlock(blocks []anthropicBlock) {
	if len(blocks) == 0 {
		return
	}
	blocks[len(blocks)-1].setCacheControl(newAnthropicEphemeralCache())
}

func buildAnthropicToolChoice(choice ToolChoice, parallel bool) *anthropicToolChoice {
	switch choice.Mode {
	case ToolChoiceNone:
		return &anthropicToolChoice{Type: "none"}
	case ToolChoiceRequired:
		return &anthropicToolChoice{Type: "any"}
	case ToolChoiceName:
		return &anthropicToolChoice{Type: "tool", Name: choice.Name}
	default:
		disableParallel := !parallel
		return &anthropicToolChoice{Type: "auto", DisableParallelToolUse: &disableParallel}
	}
}

func emitAnthropicTextStart(send eventSender, text string) error {
	if text == "" {
		return nil
	}
	return send.Send(Event{Type: EventTextDelta, Text: text})
}

func handleAnthropicStartBlockContent(send eventSender, block anthropicResponseBlock, index int64, accumulator *toolCallAccumulator) error {
	switch block.Type {
	case "text":
		return emitAnthropicTextStart(send, block.Text)
	case "thinking":
		return emitReasoningDelta(send, block.Thinking, block.Signature)
	case "tool_use":
		accumulator.Start(index, anthropicToolCall(block))
	}
	return nil
}

func anthropicToolCall(block anthropicResponseBlock) ToolCall {
	return ToolCall{ID: block.ID, Name: block.Name, Arguments: toolInputToRaw(block.Input)}
}

func emitReasoningDelta(send eventSender, text, encrypted string) error {
	if text == "" && encrypted == "" {
		return nil
	}
	kind := ReasoningKindRaw
	if text == "" && encrypted != "" {
		kind = ReasoningKindEncrypted
	}
	return send.Send(Event{
		Type:                      EventReasoningDelta,
		Text:                      text,
		ReasoningKind:             kind,
		ReasoningEncryptedContent: encrypted,
	})
}

func toolInputToRaw(input any) json.RawMessage {
	switch v := input.(type) {
	case json.RawMessage:
		return v
	case []byte:
		return json.RawMessage(v)
	case string:
		return json.RawMessage(v)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return json.RawMessage(data)
	}
}

type toolCallAccumulator struct {
	calls    map[int64]ToolCall
	fallback map[int64]json.RawMessage
	partial  map[int64]*strings.Builder
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{
		calls:    make(map[int64]ToolCall),
		fallback: make(map[int64]json.RawMessage),
		partial:  make(map[int64]*strings.Builder),
	}
}

func (a *toolCallAccumulator) Start(index int64, call ToolCall) {
	if len(call.Arguments) > 0 {
		a.fallback[index] = call.Arguments
	}
	call.Arguments = nil
	a.calls[index] = call
}

func (a *toolCallAccumulator) Append(index int64, partial string) {
	if partial == "" {
		return
	}
	builder := a.partial[index]
	if builder == nil {
		builder = &strings.Builder{}
		a.partial[index] = builder
	}
	builder.WriteString(partial)
}

func (a *toolCallAccumulator) Finish(index int64) (ToolCall, bool) {
	call, ok := a.calls[index]
	if !ok {
		return ToolCall{}, false
	}
	if builder := a.partial[index]; builder != nil && builder.Len() > 0 {
		call.Arguments = json.RawMessage(builder.String())
	} else if fallback, ok := a.fallback[index]; ok {
		call.Arguments = fallback
	}
	delete(a.calls, index)
	delete(a.partial, index)
	delete(a.fallback, index)
	return call, true
}

func maxTokens(requested, fallback int) int64 {
	if requested > 0 {
		return int64(requested)
	}
	return int64(fallback)
}
