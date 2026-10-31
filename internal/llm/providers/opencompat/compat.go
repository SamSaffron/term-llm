package opencompat

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/cache"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/core/stream"
	"github.com/samsaffron/term-llm/internal/providerhttp"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

// Local names for the shared provider contracts keep the moved transports
// readable; they are aliases, not new types.
type (
	Capabilities      = core.Capabilities
	Event             = core.Event
	Message           = core.Message
	ModelInfo         = core.ModelInfo
	Part              = core.Part
	Provider          = core.Provider
	Request           = core.Request
	Stream            = core.Stream
	ToolCall          = core.ToolCall
	ToolChoice        = core.ToolChoice
	ToolSpec          = core.ToolSpec
	Usage             = core.Usage
	Role              = core.Role
	HTTPStatusError   = providerhttp.StatusError
	eventSender       = stream.Sender
	sseDecoderOptions = stream.DecoderOptions
)

const (
	EventTextDelta           = core.EventTextDelta
	EventReasoningDelta      = core.EventReasoningDelta
	EventToolCall            = core.EventToolCall
	EventToolExecStart       = core.EventToolExecStart
	EventToolExecEnd         = core.EventToolExecEnd
	EventUsage               = core.EventUsage
	EventDone                = core.EventDone
	ReasoningKindRaw         = core.ReasoningKindRaw
	RoleSystem               = core.RoleSystem
	RoleDeveloper            = core.RoleDeveloper
	RoleUser                 = core.RoleUser
	RoleAssistant            = core.RoleAssistant
	RoleTool                 = core.RoleTool
	PartText                 = core.PartText
	PartImage                = core.PartImage
	PartFile                 = core.PartFile
	PartToolCall             = core.PartToolCall
	PartToolResult           = core.PartToolResult
	ToolChoiceAuto           = core.ToolChoiceAuto
	ToolChoiceNone           = core.ToolChoiceNone
	ToolChoiceRequired       = core.ToolChoiceRequired
	ToolChoiceName           = core.ToolChoiceName
	ToolContentPartImageData = core.ToolContentPartImageData
)

type StreamIncompleteError = stream.StreamIncompleteError

// OutputLimitError is returned when the provider's finish_reason is "length"
// and either tool calls were being generated (truncated JSON, never safe to
// execute) or the response produced no usable visible text (e.g. the entire
// budget was consumed by reasoning tokens).
type OutputLimitError struct {
	Provider  string
	Limit     int  // max_tokens sent in the request, 0 if unknown
	TruncTool bool // true when tool-call JSON was in progress
}

func (e *OutputLimitError) Error() string {
	var detail string
	if e.TruncTool {
		detail = "response truncated, tool call not executed"
	} else {
		detail = "response truncated with no usable output"
	}
	if e.Limit > 0 {
		return fmt.Sprintf("%s: output token limit (%d) reached — %s", e.Provider, e.Limit, detail)
	}
	return fmt.Sprintf("%s: output token limit reached — %s", e.Provider, detail)
}

// Hooks supplies behaviour owned by the parent llm package's model registry
// and limit tables, which this package cannot import without a cycle. Nil
// fields fall back to neutral defaults (no limits, no pricing).
type Hooks struct {
	// InputLimitForProviderModel is llm.InputLimitForProviderModel.
	InputLimitForProviderModel func(provider, model string) int
	// InputLimitForModel is llm.InputLimitForModel.
	InputLimitForModel func(model string) int
	// PricingForProviderModel is llm.PricingForProviderModel.
	PricingForProviderModel func(provider, model string) (input, output float64, ok bool)
	// ClampOutputTokens is llm.ClampOutputTokens.
	ClampOutputTokens func(requested int, model string) int
	// HTTPClient returns the parent package's shared streaming client so its
	// tests and callers can inject transports. Nil uses the package client.
	HTTPClient func() *http.Client
}

// ParentHooks is installed by the parent llm package (typically in init).
var ParentHooks Hooks

// InputLimitForProviderModel returns the hooked input limit (0 if unknown).
func InputLimitForProviderModel(provider, model string) int {
	if ParentHooks.InputLimitForProviderModel != nil {
		return ParentHooks.InputLimitForProviderModel(provider, model)
	}
	return 0
}

// InputLimitForModel returns the hooked input limit (0 if unknown).
func InputLimitForModel(model string) int {
	if ParentHooks.InputLimitForModel != nil {
		return ParentHooks.InputLimitForModel(model)
	}
	return 0
}

// PricingForProviderModel returns hooked pricing in USD per million tokens.
func PricingForProviderModel(provider, model string) (float64, float64, bool) {
	if ParentHooks.PricingForProviderModel != nil {
		return ParentHooks.PricingForProviderModel(provider, model)
	}
	return 0, 0, false
}

// ClampOutputTokens caps requested output tokens using the hooked limits.
func ClampOutputTokens(requested int, model string) int {
	if ParentHooks.ClampOutputTokens != nil {
		return ParentHooks.ClampOutputTokens(requested, model)
	}
	return requested
}

func chooseModel(requested, fallback string) string { return core.ChooseModel(requested, fallback) }

func newEventStreamWithCancelHook(ctx context.Context, cancelHook func(), run func(context.Context, eventSender) error) Stream {
	return stream.NewWithCancelHook(ctx, cancelHook, run)
}

func newSSEDecoder(r io.Reader, opts sseDecoderOptions) *stream.Decoder {
	return stream.NewDecoder(r, opts)
}

func sanitizeToolHistory(messages []Message) []Message { return core.SanitizeToolHistory(messages) }

func toolResultTextContent(result *core.ToolResult) string { return core.ToolResultTextContent(result) }

func toolResultContentParts(result *core.ToolResult) []core.ToolContentPart {
	return core.ToolResultContentParts(result)
}

func toolResultImageData(part core.ToolContentPart) (string, string, bool) {
	return core.ToolResultImageData(part)
}

func imageDetail(detail string) string { return core.ImageDetail(detail) }

func imageDetailWithDefault(detail, def string) string {
	return core.ImageDetailWithDefault(detail, def)
}

func newHTTPStatusError(provider string, resp *http.Response, body []byte) *HTTPStatusError {
	return providerhttp.NewStatusError(provider, resp, body)
}

func newHTTPStatusErrorMessageFromResponsef(resp *http.Response, format string) *HTTPStatusError {
	return providerhttp.NewStatusErrorMessageFromResponse(resp, func(body []byte) string {
		return fmt.Sprintf(format, resp.StatusCode, string(body))
	})
}

func newHTTPStatusErrorWithDisplayBody(provider string, resp *http.Response, body []byte, displayBody string) *HTTPStatusError {
	display := providerhttp.NewStatusErrorString(provider, resp.StatusCode, resp.Status, resp.Header, displayBody)
	return providerhttp.NewStatusErrorMessage(display.Error(), resp, body)
}

// DebugRawSection prints a timestamped debug section.
func DebugRawSection(enabled bool, label, body string) {
	if !enabled {
		return
	}
	ts := time.Now().Format(time.RFC3339Nano)
	if body != "" {
		runtimeoutput.Printf("\n[%s] %s\n%s\n[%s] END %s\n\n", ts, label, body, ts, label)
	} else {
		runtimeoutput.Printf("\n[%s] %s\n[%s] END %s\n\n", ts, label, ts, label)
	}
}

// ParseModelEffort extracts effort suffix from model name.
// "gpt-5.2-high" -> ("gpt-5.2", "high").
func ParseModelEffort(model string) (string, string) {
	// Longest suffixes first so "-high" does not match "-xhigh".
	for _, effort := range []string{"minimal", "medium", "xhigh", "high", "low", "max"} {
		suffix := "-" + effort
		if strings.HasSuffix(model, suffix) {
			return strings.TrimSuffix(model, suffix), effort
		}
	}
	return model, ""
}

func isLeadingReasoningWhitespaceArtifact(content, reasoning string, sawVisibleText bool) bool {
	return !sawVisibleText && reasoning != "" && strings.TrimSpace(content) == ""
}

func hasVisibleTextDelta(text string) bool { return strings.TrimSpace(text) != "" }

func cloneEfforts(efforts []string) []string {
	if len(efforts) == 0 {
		return nil
	}
	return append([]string(nil), efforts...)
}

func getCachedModelInfos(apiKey, envName, cacheKey string, refreshInFlight *atomic.Bool, refresh func(string), fetch func(string) []ModelInfo) []ModelInfo {
	cached, err := cache.ReadModelCache(cacheKey)
	if err == nil && cache.IsCacheValid(cached) {
		return ModelInfosFromCache(cached)
	}
	if apiKey == "" {
		apiKey = os.Getenv(envName)
	}
	if apiKey == "" {
		if cached != nil && len(cached.Models) > 0 {
			return ModelInfosFromCache(cached)
		}
		return nil
	}
	if cached != nil && len(cached.Models) > 0 {
		if refreshInFlight.CompareAndSwap(false, true) {
			go func() {
				defer refreshInFlight.Store(false)
				refresh(apiKey)
			}()
		}
		return ModelInfosFromCache(cached)
	}
	return fetch(apiKey)
}

// httpClient returns the client used for provider requests.
func httpClient() *http.Client {
	if ParentHooks.HTTPClient != nil {
		if c := ParentHooks.HTTPClient(); c != nil {
			return c
		}
	}
	return defaultHTTPClient
}
