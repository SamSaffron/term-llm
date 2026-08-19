package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

var (
	ollamaChatDefaultModel   = config.DefaultProviderModel("ollama")
	ollamaChatDefaultBaseURL = config.DefaultOllamaBaseURL
	ollamaReasoningEfforts   = []string{"low", "medium", "high", "xhigh"}
)

// OllamaOptions holds Ollama-native generation knobs that have no equivalent
// in the shared Request struct.
type OllamaOptions struct {
	Think           *bool
	ThinkLevel      string
	TopK            *int
	MinP            *float64
	PresencePenalty *float64
	NumCtx          *int
	NumPredict      *int
}

// OllamaProvider implements Provider using the native Ollama /api/chat endpoint.
// It supports the think flag (for extended reasoning models like Qwen3),
// tool calls, and Ollama-native sampling options.
type OllamaProvider struct {
	baseURL string
	model   string
	opts    OllamaOptions

	capabilitiesMu  sync.RWMutex
	thinkingByModel map[string]bool
}

func normalizeOllamaThinkLevel(level string) (string, error) {
	level = strings.ToLower(strings.TrimSpace(level))
	switch level {
	case "", "low", "medium", "high", "max":
		return level, nil
	case "xhigh":
		return "max", nil
	default:
		return "", fmt.Errorf("invalid Ollama think_level %q: expected low, medium, high, xhigh, or max", level)
	}
}

// NewOllamaChatProvider creates a native Ollama chat provider.
// baseURL defaults to the OLLAMA_HOST env var, then http://127.0.0.1:11434.
// model defaults to qwen2.5-coder:7b.
func NewOllamaChatProvider(baseURL, model string, opts OllamaOptions) *OllamaProvider {
	if baseURL == "" {
		if host := strings.TrimSpace(os.Getenv("OLLAMA_HOST")); host != "" {
			if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
				host = "http://" + host
			}
			baseURL = host
		} else {
			baseURL = ollamaChatDefaultBaseURL
		}
	}
	baseURL = strings.TrimSuffix(baseURL, "/")
	// Ollama binds to 127.0.0.1 (IPv4) by default. If the caller passes
	// "localhost", Go may resolve it to ::1 (IPv6) on dual-stack systems,
	// causing "connect: connection refused" even when Ollama is running.
	// Normalise to the explicit IPv4 loopback address to match the default.
	baseURL = strings.Replace(baseURL, "://localhost:", "://127.0.0.1:", 1)
	if model == "" {
		model = ollamaChatDefaultModel
	}
	return &OllamaProvider{
		baseURL:         baseURL,
		model:           model,
		opts:            opts,
		thinkingByModel: make(map[string]bool),
	}
}

func (p *OllamaProvider) Name() string {
	return fmt.Sprintf("Ollama (%s)", p.model)
}

func (p *OllamaProvider) Credential() string {
	return "free"
}

func (p *OllamaProvider) Capabilities() Capabilities {
	return Capabilities{
		ToolCalls:          true,
		SupportsToolChoice: false,
	}
}

// --- Ollama native API types ---

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaChatMsg `json:"messages"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	Think    any             `json:"think,omitempty"`
	Options  *ollamaChatOpts `json:"options,omitempty"`
}

type ollamaChatOpts struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	TopK            *int     `json:"top_k,omitempty"`
	MinP            *float64 `json:"min_p,omitempty"`
	PresencePenalty *float64 `json:"presence_penalty,omitempty"`
	NumCtx          *int     `json:"num_ctx,omitempty"`
	NumPredict      *int     `json:"num_predict,omitempty"`
}

type ollamaChatMsg struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Thinking  string           `json:"thinking,omitempty"`
	Images    []string         `json:"images,omitempty"` // raw base64, no data: prefix
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

type ollamaToolCall struct {
	Function ollamaToolCallFn `json:"function"`
}

type ollamaToolCallFn struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ollamaTool struct {
	Type     string        `json:"type"`
	Function ollamaToolDef `json:"function"`
}

type ollamaToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ollamaChatChunk is one line of the NDJSON streaming response.
type ollamaChatChunk struct {
	Model           string         `json:"model"`
	Message         ollamaMsgChunk `json:"message"`
	Done            bool           `json:"done"`
	DoneReason      string         `json:"done_reason,omitempty"`
	PromptEvalCount int            `json:"prompt_eval_count,omitempty"`
	EvalCount       int            `json:"eval_count,omitempty"`
	Error           string         `json:"error,omitempty"`
}

type ollamaMsgChunk struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Thinking  string           `json:"thinking,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

// buildOllamaMessages converts internal messages to Ollama's native format.
// Ollama tool results use role "tool" with plain text content; tool calls have
// no ID field. Tool-result images are delivered in a following synthetic user
// message because Ollama does not currently accept images on tool messages.
// Developer-role messages are folded into the next user turn.
func buildOllamaMessages(messages []Message) []ollamaChatMsg {
	messages = sanitizeToolHistory(messages)

	var result []ollamaChatMsg
	var pendingDev string

	for i := 0; i < len(messages); i++ {
		msg := messages[i]
		switch msg.Role {
		case RoleDeveloper:
			text, _, _ := splitParts(msg.Parts)
			if text != "" {
				if pendingDev != "" {
					pendingDev += "\n\n"
				}
				pendingDev += text
			}
		case RoleSystem:
			text, _, _ := splitParts(msg.Parts)
			if text != "" {
				result = append(result, ollamaChatMsg{Role: "system", Content: text})
			}
		case RoleUser:
			text, _, _ := splitParts(msg.Parts)
			if pendingDev != "" {
				text = fmt.Sprintf("<developer>\n%s\n</developer>\n\n", pendingDev) + text
				pendingDev = ""
			}
			var images []string
			for _, part := range msg.Parts {
				if part.Type == PartImage && part.ImageData != nil && strings.TrimSpace(part.ImageData.Base64) != "" {
					images = append(images, part.ImageData.Base64)
				}
			}
			if text == "" && len(images) == 0 {
				continue
			}
			result = append(result, ollamaChatMsg{Role: "user", Content: text, Images: images})
		case RoleAssistant:
			text, oaiCalls, reasoning := splitParts(msg.Parts)
			if len(oaiCalls) > 0 {
				var calls []ollamaToolCall
				for _, tc := range oaiCalls {
					args := json.RawMessage(tc.Function.Arguments)
					if !json.Valid(args) {
						args = json.RawMessage("{}")
					}
					calls = append(calls, ollamaToolCall{
						Function: ollamaToolCallFn{Name: tc.Function.Name, Arguments: args},
					})
				}
				result = append(result, ollamaChatMsg{Role: "assistant", Content: text, Thinking: reasoning, ToolCalls: calls})
			} else if text != "" || reasoning != "" {
				result = append(result, ollamaChatMsg{Role: "assistant", Content: text, Thinking: reasoning})
			}
		case RoleTool:
			var toolImages []string
			var imageToolNames []string
			for ; i < len(messages) && messages[i].Role == RoleTool; i++ {
				for _, part := range messages[i].Parts {
					if part.Type != PartToolResult || part.ToolResult == nil {
						continue
					}

					content := toolResultTextContent(part.ToolResult)
					var images []string
					invalidImages := 0
					for _, contentPart := range toolResultContentParts(part.ToolResult) {
						if contentPart.Type != ToolContentPartImageData {
							continue
						}
						_, base64Data, ok := toolResultImageData(contentPart)
						if !ok {
							invalidImages++
							continue
						}
						images = append(images, base64Data)
					}

					if invalidImages > 0 {
						warning := fmt.Sprintf("[Ollama omitted %d invalid or unsupported tool-result image(s).]", invalidImages)
						if content == "" {
							content = warning
						} else {
							content += "\n\n" + warning
						}
					}
					if content == "" && len(images) > 0 {
						content = fmt.Sprintf("[Tool %q returned %d image(s), attached in the following user message.]", part.ToolResult.Name, len(images))
					}
					result = append(result, ollamaChatMsg{Role: "tool", Content: content})
					if len(images) > 0 {
						toolImages = append(toolImages, images...)
						imageToolNames = append(imageToolNames, part.ToolResult.Name)
					}
				}
			}
			i-- // The outer loop advances to the first message after this tool-result run.
			if len(toolImages) > 0 {
				// Ollama currently ignores/rejects images on role=tool. A
				// synthetic user turn is the documented-compatible way to
				// actually place tool-produced images in model context.
				result = append(result, ollamaChatMsg{
					Role:    "user",
					Content: fmt.Sprintf("Image(s) returned by tool(s) %q.", strings.Join(imageToolNames, ", ")),
					Images:  toolImages,
				})
			}
		}
	}

	if pendingDev != "" {
		result = append(result, ollamaChatMsg{
			Role:    "user",
			Content: fmt.Sprintf("<developer>\n%s\n</developer>", pendingDev),
		})
	}
	return result
}

func buildOllamaTools(specs []ToolSpec) ([]ollamaTool, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	tools := make([]ollamaTool, 0, len(specs))
	for _, spec := range specs {
		schema, err := cachedToolSchemaJSON(spec.Schema)
		if err != nil {
			return nil, fmt.Errorf("marshal tool schema %s: %w", spec.Name, err)
		}
		tools = append(tools, ollamaTool{
			Type: "function",
			Function: ollamaToolDef{
				Name:        spec.Name,
				Description: spec.Description,
				Parameters:  schema,
			},
		})
	}
	return tools, nil
}

func hasOllamaCapability(capabilities []string, want string) bool {
	for _, capability := range capabilities {
		if strings.EqualFold(strings.TrimSpace(capability), want) {
			return true
		}
	}
	return false
}

type ollamaShowResponse struct {
	Capabilities []string       `json:"capabilities"`
	Parameters   string         `json:"parameters"`
	ModelInfo    map[string]any `json:"model_info"`
}

func ollamaShowContextLength(show ollamaShowResponse) int {
	for key, value := range show.ModelInfo {
		if !strings.HasSuffix(strings.ToLower(strings.TrimSpace(key)), ".context_length") {
			continue
		}
		switch n := value.(type) {
		case float64:
			if n > 0 {
				return int(n)
			}
		case json.Number:
			if parsed, err := strconv.Atoi(n.String()); err == nil && parsed > 0 {
				return parsed
			}
		}
	}
	return 0
}

func ollamaShowParameterInt(show ollamaShowResponse, name string) int {
	for _, line := range strings.Split(show.Parameters, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != name {
			continue
		}
		if parsed, err := strconv.Atoi(strings.Trim(fields[1], `"`)); err == nil && parsed > 0 {
			return parsed
		}
	}
	return 0
}

func ollamaShowNumCtx(show ollamaShowResponse) int {
	return ollamaShowParameterInt(show, "num_ctx")
}

// effectiveInputLimit returns the request input budget for the model as it is
// actually configured on this Ollama endpoint. GGUF metadata can advertise a
// much larger architectural context than the Modelfile allocates; using that
// number prevents auto-compaction from ever firing on the live runner. Reserve
// num_predict from the configured window because Ollama shares that window
// between prompt and generation.
func (p *OllamaProvider) effectiveInputLimit(ctx context.Context, model string) (int, error) {
	resolved, _, err := p.resolveReasoningSuffix(ctx, model)
	if err != nil {
		return 0, err
	}
	show, err := p.showModel(ctx, resolved)
	if err != nil {
		return 0, err
	}

	limit := ollamaShowContextLength(show)
	if configured := ollamaShowNumCtx(show); configured > 0 && (limit == 0 || configured < limit) {
		limit = configured
	}
	if reserve := ollamaShowParameterInt(show, "num_predict"); reserve > 0 && reserve < limit {
		limit -= reserve
	}
	return limit, nil
}

func (p *OllamaProvider) showModel(ctx context.Context, model string) (ollamaShowResponse, error) {
	body, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return ollamaShowResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/show", bytes.NewReader(body))
	if err != nil {
		return ollamaShowResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := defaultHTTPClient.Do(httpReq)
	if err != nil {
		return ollamaShowResponse{}, fmt.Errorf("Ollama show request failed for %q: %w", model, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return ollamaShowResponse{}, fmt.Errorf("read Ollama show response for %q: %w", model, err)
	}
	if resp.StatusCode != http.StatusOK {
		return ollamaShowResponse{}, newHTTPStatusError("Ollama", resp, raw)
	}
	var show ollamaShowResponse
	if err := json.Unmarshal(raw, &show); err != nil {
		return ollamaShowResponse{}, fmt.Errorf("parse Ollama show response for %q: %w", model, err)
	}
	return show, nil
}

func (p *OllamaProvider) rememberThinkingSupport(model string, supported bool) {
	p.capabilitiesMu.Lock()
	p.thinkingByModel[model] = supported
	p.capabilitiesMu.Unlock()
}

func (p *OllamaProvider) knownThinkingSupport(model string) (bool, bool) {
	p.capabilitiesMu.RLock()
	supported, ok := p.thinkingByModel[model]
	p.capabilitiesMu.RUnlock()
	return supported, ok
}

func (p *OllamaProvider) supportsThinking(ctx context.Context, model string) (bool, error) {
	p.capabilitiesMu.RLock()
	supported, ok := p.thinkingByModel[model]
	p.capabilitiesMu.RUnlock()
	if ok {
		return supported, nil
	}
	show, err := p.showModel(ctx, model)
	if err != nil {
		return false, err
	}
	supported = hasOllamaCapability(show.Capabilities, "thinking")
	p.rememberThinkingSupport(model, supported)
	return supported, nil
}

func splitOllamaReasoningSuffix(model string) (string, string) {
	for _, effort := range []string{"xhigh", "medium", "high", "low"} {
		if strings.HasSuffix(model, "-"+effort) {
			return strings.TrimSuffix(model, "-"+effort), effort
		}
	}
	return model, ""
}

func (p *OllamaProvider) resolveReasoningSuffix(ctx context.Context, model string) (string, string, error) {
	for _, info := range GetCachedOllamaModelInfos(p.baseURL) {
		if strings.EqualFold(strings.TrimSpace(info.ID), strings.TrimSpace(model)) {
			p.rememberThinkingSupport(model, len(info.ReasoningEfforts) > 0)
			return model, "", nil
		}
	}
	if base, effort, _, ok := CachedOllamaModelEffort(p.baseURL, model); ok && effort != "" {
		p.rememberThinkingSupport(base, true)
		return base, effort, nil
	}

	base, effort := splitOllamaReasoningSuffix(model)
	if effort == "" {
		return model, "", nil
	}
	// On a cold cache, prefer a real installed tag whose natural name ends in an
	// effort word. Only reinterpret the suffix when the exact tag does not exist.
	if show, err := p.showModel(ctx, model); err == nil {
		p.rememberThinkingSupport(model, hasOllamaCapability(show.Capabilities, "thinking"))
		return model, "", nil
	}
	supported, err := p.supportsThinking(ctx, base)
	if err != nil {
		return "", "", fmt.Errorf("resolve Ollama reasoning suffix for %q: %w", model, err)
	}
	if !supported {
		return "", "", fmt.Errorf("Ollama model %q does not advertise thinking support", base)
	}
	return base, effort, nil
}

func ollamaWireThinkLevel(effort string) (string, error) {
	return normalizeOllamaThinkLevel(effort)
}

func (p *OllamaProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	model := chooseModel(req.Model, p.model)
	var think any
	if p.opts.Think != nil {
		think = *p.opts.Think
	}

	thinkLevel, err := normalizeOllamaThinkLevel(p.opts.ThinkLevel)
	if err != nil {
		return nil, err
	}
	resolvedModel, suffixEffort, err := p.resolveReasoningSuffix(ctx, model)
	if err != nil {
		return nil, err
	}
	model = resolvedModel
	if suffixEffort != "" {
		thinkLevel, err = ollamaWireThinkLevel(suffixEffort)
		if err != nil {
			return nil, err
		}
	}
	if requestEffort := strings.TrimSpace(req.ReasoningEffort); requestEffort != "" {
		thinkLevel, err = ollamaWireThinkLevel(requestEffort)
		if err != nil {
			return nil, err
		}
	}
	if strings.HasSuffix(model, "-think") {
		model = strings.TrimSuffix(model, "-think")
		if thinkLevel == "" {
			think = true
		}
	}
	if thinkLevel != "" {
		supported, err := p.supportsThinking(ctx, model)
		if err != nil {
			return nil, fmt.Errorf("check Ollama thinking support for %q: %w", model, err)
		}
		if !supported {
			return nil, fmt.Errorf("Ollama model %q does not advertise thinking support", model)
		}
		think = thinkLevel
	} else if p.opts.Think == nil {
		// Explicitly disable thinking when live discovery has already established
		// that this is a non-thinking model. Do not add a blocking /api/show call
		// on the ordinary stream path: offline/custom Ollama-compatible servers
		// may not implement it, and unknown models retain legacy nil semantics.
		if supported, known := p.knownThinkingSupport(model); known && !supported {
			think = false
		}
	}

	messages := buildOllamaMessages(req.Messages)
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages provided")
	}

	tools, err := buildOllamaTools(req.Tools)
	if err != nil {
		return nil, err
	}

	// Build options – merge provider-level defaults with per-request overrides.
	opts := &ollamaChatOpts{
		TopK:            p.opts.TopK,
		MinP:            p.opts.MinP,
		PresencePenalty: p.opts.PresencePenalty,
		NumCtx:          p.opts.NumCtx,
	}
	if req.TemperatureSet || req.Temperature != 0 {
		v := float64(req.Temperature)
		opts.Temperature = &v
	}
	if req.TopPSet || req.TopP != 0 {
		v := float64(req.TopP)
		opts.TopP = &v
	}
	// req.MaxOutputTokens takes precedence over provider-level NumPredict.
	if req.MaxOutputTokens > 0 {
		v := req.MaxOutputTokens
		opts.NumPredict = &v
	} else if p.opts.NumPredict != nil {
		opts.NumPredict = p.opts.NumPredict
	}
	if ollamaOptsEmpty(opts) {
		opts = nil
	}

	chatReq := ollamaChatRequest{
		Model:    model,
		Messages: messages,
		Tools:    tools,
		Stream:   true,
		Think:    think,
		Options:  opts,
	}

	if req.Debug {
		fmt.Fprintf(os.Stderr, "=== DEBUG: Ollama Stream Request ===\n")
		fmt.Fprintf(os.Stderr, "URL: %s/api/chat\n", p.baseURL)
		fmt.Fprintf(os.Stderr, "Model: %s\n", model)
		fmt.Fprintf(os.Stderr, "Messages: %d\n", len(messages))
		fmt.Fprintf(os.Stderr, "Tools: %d\n", len(tools))
		if think != nil {
			fmt.Fprintf(os.Stderr, "Think: %v\n", think)
		}
		fmt.Fprintln(os.Stderr, "====================================")
	}

	body, err := json.Marshal(chatReq)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := defaultHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Ollama request failed (is Ollama running at %s?): %w", p.baseURL, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newOllamaStatusErrorFromResponse(resp)
	}

	return newEventStreamWithCancelHook(ctx, func() { _ = resp.Body.Close() }, func(ctx context.Context, send eventSender) error {
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)

		var pendingToolCalls []ollamaToolCall
		var lastUsage *Usage
		sawVisibleText := false

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			var chunk ollamaChatChunk
			if err := json.Unmarshal([]byte(line), &chunk); err != nil {
				continue
			}

			if chunk.Error != "" {
				return fmt.Errorf("Ollama API error: %s", chunk.Error)
			}

			if chunk.Message.Thinking != "" {
				if err := send.Send(Event{Type: EventReasoningDelta, Text: chunk.Message.Thinking, ReasoningKind: ReasoningKindRaw}); err != nil {
					return err
				}
			}

			if chunk.Message.Content != "" {
				if !isLeadingReasoningWhitespaceArtifact(chunk.Message.Content, chunk.Message.Thinking, sawVisibleText) {
					if hasVisibleTextDelta(chunk.Message.Content) {
						sawVisibleText = true
					}
					if err := send.Send(Event{Type: EventTextDelta, Text: chunk.Message.Content}); err != nil {
						return err
					}
				}
			}

			pendingToolCalls = append(pendingToolCalls, chunk.Message.ToolCalls...)

			if chunk.Done && (chunk.PromptEvalCount > 0 || chunk.EvalCount > 0) {
				lastUsage = &Usage{
					InputTokens:  chunk.PromptEvalCount,
					OutputTokens: chunk.EvalCount,
				}
			}
		}

		if err := scanner.Err(); err != nil {
			return fmt.Errorf("Ollama streaming error: %w", err)
		}

		for _, tc := range pendingToolCalls {
			args := tc.Function.Arguments
			if !json.Valid(args) {
				args = json.RawMessage("{}")
			}
			call := ToolCall{
				// Ollama omits tool-call IDs. Use a collision-resistant internal ID:
				// call history and UI deduplication can outlive this provider instance.
				ID:        newSyntheticToolCallID(),
				Name:      tc.Function.Name,
				Arguments: args,
			}
			if err := send.Send(Event{Type: EventToolCall, Tool: &call}); err != nil {
				return err
			}
		}

		if lastUsage != nil {
			if err := send.Send(Event{Type: EventUsage, Use: lastUsage}); err != nil {
				return err
			}
		}

		return send.Send(Event{Type: EventDone})
	}), nil
}

func newOllamaStatusErrorFromResponse(resp *http.Response) *HTTPStatusError {
	raw := providerhttp.ReadBodyAndClose(resp, 0)
	var errBody struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &errBody) == nil && errBody.Error != "" {
		return newHTTPStatusErrorMessagef(resp, raw, "Ollama API error: %s", errBody.Error)
	}
	return newHTTPStatusError("Ollama", resp, raw)
}

// ListModels returns locally available Ollama models via /api/tags.
func (p *OllamaProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", p.baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}

	resp, err := defaultHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Ollama request failed (is Ollama running at %s?): %w", p.baseURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newHTTPStatusError("Ollama", resp, raw)
	}

	var tagsResp struct {
		Models []struct {
			Name         string   `json:"name"`
			Capabilities []string `json:"capabilities"`
			Details      struct {
				ContextLength int `json:"context_length"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &tagsResp); err != nil {
		return nil, fmt.Errorf("failed to parse tags response: %w", err)
	}

	models := make([]ModelInfo, len(tagsResp.Models))
	for i, m := range tagsResp.Models {
		models[i] = ModelInfo{ID: m.Name, OwnedBy: "ollama", InputLimit: m.Details.ContextLength}
		if hasOllamaCapability(m.Capabilities, "thinking") {
			models[i].ReasoningEfforts = cloneEfforts(ollamaReasoningEfforts)
		}
	}

	// /api/tags currently uses a reduced capability scanner and can omit
	// capabilities that /api/show detects from the model's GGUF chat template.
	// Enrich models concurrently so completion and model pickers do not advertise
	// reasoning controls for models that do not support them.
	var wg sync.WaitGroup
	var enrichmentFailed atomic.Bool
	semaphore := make(chan struct{}, 8)
	for i := range models {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				enrichmentFailed.Store(true)
				return
			}
			show, err := p.showModel(ctx, models[i].ID)
			if err != nil {
				enrichmentFailed.Store(true)
				return
			}
			if contextLength := ollamaShowContextLength(show); contextLength > 0 {
				models[i].InputLimit = contextLength
			}
			models[i].ConfiguredContext = ollamaShowNumCtx(show)
			supported := hasOllamaCapability(show.Capabilities, "thinking")
			p.rememberThinkingSupport(models[i].ID, supported)
			if supported {
				models[i].ReasoningEfforts = cloneEfforts(ollamaReasoningEfforts)
			} else {
				models[i].ReasoningEfforts = nil
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return models, err
	}
	if !enrichmentFailed.Load() {
		refreshOllamaModelCache(p.baseURL, models)
	}
	return models, nil
}

func ollamaOptsEmpty(o *ollamaChatOpts) bool {
	return o.Temperature == nil && o.TopP == nil && o.TopK == nil &&
		o.MinP == nil && o.PresencePenalty == nil &&
		o.NumCtx == nil && o.NumPredict == nil
}
