// Package responsesproviders holds the provider transports built on the
// Responses API client (OpenAI, ChatGPT, Copilot, Grok) and the OpenCode
// Zen / OpenCode Go gateways that share its catalogs and helpers.
package responsesproviders

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/cache"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/core/stream"
	"github.com/samsaffron/term-llm/internal/llm/providers/opencompat"
	"github.com/samsaffron/term-llm/internal/llm/providers/responses"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

// Local names for shared contracts; these are aliases, not new types.
type (
	Capabilities                = core.Capabilities
	Event                       = core.Event
	Message                     = core.Message
	ModelInfo                   = core.ModelInfo
	ModelServiceTier            = core.ModelServiceTier
	NativeToolDiscoverySupport  = core.NativeToolDiscoverySupport
	Part                        = core.Part
	Provider                    = core.Provider
	ProviderReplayItem          = core.ProviderReplayItem
	Request                     = core.Request
	Role                        = core.Role
	Stream                      = core.Stream
	ToolCall                    = core.ToolCall
	ToolChoice                  = core.ToolChoice
	ToolDiscoveryCall           = core.ToolDiscoveryCall
	ToolDiscoveryOutput         = core.ToolDiscoveryOutput
	ToolSpec                    = core.ToolSpec
	Usage                       = core.Usage
	HTTPStatusError             = providerhttp.StatusError
	FileUploadPolicy            = responses.FileUploadPolicy
	ResponsesClient             = responses.ResponsesClient
	ResponsesMultiAgent         = responses.ResponsesMultiAgent
	ResponsesNamespace          = responses.ResponsesNamespace
	ResponsesOptions            = core.ResponsesOptions
	ResponsesPromptCacheOptions = responses.ResponsesPromptCacheOptions
	ResponsesReasoning          = responses.ResponsesReasoning
	ResponsesRequest            = responses.ResponsesRequest
	ResponsesText               = responses.ResponsesText
	ResponsesToolSearchTool     = responses.ResponsesToolSearchTool
	ResponsesWebSearchTool      = responses.ResponsesWebSearchTool

	ResponsesNamespaceFunctionTool = responses.ResponsesNamespaceFunctionTool

	OpenAICompatProvider = opencompat.OpenAICompatProvider
	oaiChatRequest       = opencompat.OAIChatRequest
	oaiChatResponse      = opencompat.OAIChatResponse
	oaiStreamOptions     = opencompat.OAIStreamOptions
	oaiModelsResponse    = opencompat.OAIModelsResponse
	sseDecoderOptions    = stream.DecoderOptions
	eventSender          = stream.Sender
)

const (
	EventTextDelta      = core.EventTextDelta
	EventReasoningDelta = core.EventReasoningDelta
	EventToolCall       = core.EventToolCall
	EventUsage          = core.EventUsage
	EventDone           = core.EventDone
	ReasoningKindRaw    = core.ReasoningKindRaw
	RoleSystem          = core.RoleSystem
	RoleUser            = core.RoleUser
	PartText            = core.PartText
	PartProviderReplay  = core.PartProviderReplay
	PartDiscoveryCall   = core.PartDiscoveryCall
	PartDiscoveryOutput = core.PartDiscoveryOutput
	ToolChoiceName      = core.ToolChoiceName
)

// Service tier values mirror llm.ServiceTierFast and llm.ServiceTierFastAlias.
const (
	ServiceTierFast      = "priority"
	ServiceTierFastAlias = "fast"
)

// StaticModelEntry is the static catalog shape the providers need from the
// parent model registry.
type StaticModelEntry struct {
	ID               string
	InputLimit       int
	OutputLimit      int
	ReasoningEfforts []string
}

// Hooks supplies behaviour owned by the parent llm package's model registry
// and config-derived effort tables. Nil fields fall back to neutral defaults.
type Hooks struct {
	// StaticModelEntries returns llm.ProviderModels[providerType].
	StaticModelEntries func(providerType string) []StaticModelEntry
	// BaseModelAndEffortForProvider is llm.BaseModelAndEffortForProvider.
	BaseModelAndEffortForProvider func(provider, model string) (base, effort string)
	// ConfigBaseModelAndEffortForProvider resolves config-declared effort suffixes.
	ConfigBaseModelAndEffortForProvider func(provider, model string) (base, effort string, efforts []string)
}

// ParentHooks is installed by the parent llm package (typically in init).
var ParentHooks Hooks

func staticModelEntries(providerType string) []StaticModelEntry {
	if ParentHooks.StaticModelEntries != nil {
		return ParentHooks.StaticModelEntries(providerType)
	}
	return nil
}

// BaseModelAndEffortForProvider returns the hooked base model and effort.
func BaseModelAndEffortForProvider(provider, model string) (string, string) {
	if ParentHooks.BaseModelAndEffortForProvider != nil {
		return ParentHooks.BaseModelAndEffortForProvider(provider, model)
	}
	return model, ""
}

func configBaseModelAndEffortForProvider(provider, model string) (string, string, []string) {
	if ParentHooks.ConfigBaseModelAndEffortForProvider != nil {
		return ParentHooks.ConfigBaseModelAndEffortForProvider(provider, model)
	}
	return "", "", nil
}

// RateLimitError is shared with retry through the cycle-free core contract.
type RateLimitError = core.RateLimitError

// defaultHTTPClient is the shared streaming client. http.Client.Timeout is
// intentionally unset: it would abort legitimately long-running streams.
var defaultHTTPClient = newStreamingHTTPClient()

func newStreamingHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 15 * time.Second
	transport.ResponseHeaderTimeout = 2 * time.Minute
	transport.IdleConnTimeout = 90 * time.Second
	transport.MaxIdleConnsPerHost = 100
	return &http.Client{Transport: transport}
}

// NormalizeServiceTier maps user-facing aliases to API values. Unknown values
// are returned trimmed so future service tiers can pass through.
func NormalizeServiceTier(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return ""
	case ServiceTierFastAlias, ServiceTierFast:
		return ServiceTierFast
	default:
		return strings.TrimSpace(value)
	}
}

// ModelSupportsFast reports whether model metadata advertises fast mode.
func ModelSupportsFast(model ModelInfo) bool {
	for _, tier := range model.ServiceTiers {
		if NormalizeServiceTier(tier.ID) == ServiceTierFast {
			return true
		}
	}
	for _, speedTier := range model.AdditionalSpeedTiers {
		if NormalizeServiceTier(speedTier) == ServiceTierFast {
			return true
		}
	}
	return false
}

// knownEffortSuffixes mirrors the root parser/legacy-dedup suffix list.
var knownEffortSuffixes = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

func trimKnownEffortSuffix(model string) (string, bool) {
	for _, suffix := range knownEffortSuffixes {
		if strings.HasSuffix(model, "-"+suffix) {
			return strings.TrimSuffix(model, "-"+suffix), true
		}
	}
	return model, false
}

func normalizeReasoningEfforts(efforts []string) []string {
	seen := make(map[string]bool, len(efforts))
	out := make([]string, 0, len(efforts))
	for _, effort := range efforts {
		effort = strings.ToLower(strings.TrimSpace(effort))
		if effort == "" || seen[effort] {
			continue
		}
		seen[effort] = true
		out = append(out, effort)
	}
	return out
}

func cloneEfforts(efforts []string) []string {
	if len(efforts) == 0 {
		return nil
	}
	return append([]string(nil), efforts...)
}

func cloneStrings(values []string) []string { return cloneEfforts(values) }

func cloneFileUploadPolicy(policy *FileUploadPolicy) *FileUploadPolicy {
	if policy == nil {
		return nil
	}
	clone := *policy
	clone.NativeMimeTypes = cloneStrings(policy.NativeMimeTypes)
	clone.TextEmbedMimeTypes = cloneStrings(policy.TextEmbedMimeTypes)
	return &clone
}

func defaultOpenAIResponsesFileUploadPolicyPtr() *FileUploadPolicy {
	var policy FileUploadPolicy
	if fn := responses.ParentHooks.DefaultFileUploadPolicy; fn != nil {
		policy = fn()
	}
	return &policy
}

func (p *OpenAIProvider) effectiveFileUploadPolicy() *FileUploadPolicy {
	if p != nil && p.fileUploadPolicy != nil {
		return p.fileUploadPolicy
	}
	return defaultOpenAIResponsesFileUploadPolicyPtr()
}

func (p *ChatGPTProvider) effectiveFileUploadPolicy() *FileUploadPolicy {
	if p != nil && p.fileUploadPolicy != nil {
		return p.fileUploadPolicy
	}
	return defaultOpenAIResponsesFileUploadPolicyPtr()
}

func (p *CopilotProvider) effectiveFileUploadPolicy() *FileUploadPolicy {
	if p != nil && p.fileUploadPolicy != nil {
		return p.fileUploadPolicy
	}
	return defaultOpenAIResponsesFileUploadPolicyPtr()
}

func boolPtr(v bool) *bool { return &v }

func chooseModel(requested, fallback string) string { return core.ChooseModel(requested, fallback) }

func truncate(s string, maxLen int) string { return core.Truncate(s, maxLen) }

func InputLimitForModel(model string) int { return opencompat.InputLimitForModel(model) }

func ClampOutputTokens(requested int, model string) int {
	return opencompat.ClampOutputTokens(requested, model)
}

func DebugRawSection(enabled bool, label, body string) {
	opencompat.DebugRawSection(enabled, label, body)
}

func debugRawHTTPErrorResponse(enabled bool, label string, resp *http.Response, body []byte) {
	if !enabled {
		return
	}
	var debugInfo strings.Builder
	fmt.Fprintf(&debugInfo, "Status: %d %s\n", resp.StatusCode, resp.Status)
	debugInfo.WriteString("Headers:\n")
	for key, values := range resp.Header {
		for _, value := range values {
			fmt.Fprintf(&debugInfo, "  %s: %s\n", key, value)
		}
	}
	debugInfo.WriteString("Body:\n")
	var prettyBody bytes.Buffer
	if json.Indent(&prettyBody, body, "", "  ") == nil {
		debugInfo.WriteString(prettyBody.String())
	} else {
		debugInfo.Write(body)
	}
	DebugRawSection(enabled, label, debugInfo.String())
}

func newEventStream(ctx context.Context, run func(context.Context, eventSender) error) Stream {
	return stream.New(ctx, run)
}

func newSSEDecoder(r io.Reader, opts sseDecoderOptions) *stream.Decoder {
	return stream.NewDecoder(r, opts)
}

func newHTTPStatusError(provider string, resp *http.Response, body []byte) *HTTPStatusError {
	return providerhttp.NewStatusError(provider, resp, body)
}

func newHTTPStatusErrorMessage(message string, resp *http.Response, body []byte) *HTTPStatusError {
	return providerhttp.NewStatusErrorMessage(message, resp, body)
}

func newHTTPStatusErrorMessageString(message string, statusCode int, status string, header http.Header, body string) *HTTPStatusError {
	return providerhttp.NewStatusErrorMessageString(message, statusCode, status, header, body)
}

func buildCompatMessages(messages []Message) []opencompat.OAIMessage {
	return opencompat.BuildCompatMessages(messages)
}

func buildCompatTools(specs []ToolSpec) ([]opencompat.OAITool, error) {
	return opencompat.BuildCompatTools(specs)
}

func buildCompatToolChoice(choice ToolChoice) interface{} {
	return opencompat.BuildCompatToolChoice(choice)
}

func newCompatToolState() *opencompat.CompatToolState { return opencompat.NewCompatToolState() }

func isLeadingReasoningWhitespaceArtifact(content, reasoning string, sawVisibleText bool) bool {
	return !sawVisibleText && reasoning != "" && strings.TrimSpace(content) == ""
}

func hasVisibleTextDelta(text string) bool { return strings.TrimSpace(text) != "" }

func setNonEmptyHeaders(header http.Header, values map[string]string) {
	for key, value := range values {
		if value != "" {
			header.Set(key, value)
		}
	}
}

// waitForEnterOrInterrupt waits for the user to press Enter or Ctrl+C.
func waitForEnterOrInterrupt() error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	doneCh := make(chan struct{})
	go func() {
		reader := bufio.NewReader(os.Stdin)
		reader.ReadString('\n')
		close(doneCh)
	}()

	select {
	case <-doneCh:
		return nil
	case <-sigCh:
		fmt.Println()
		// Exit rather than return: the stdin goroutine has no cancellation
		// path and would otherwise consume later user input.
		os.Exit(1)
		return nil
	}
}

func modelInfosFromCache(cached *cache.ModelCache) []core.ModelInfo {
	if cached == nil {
		return nil
	}
	if len(cached.ModelInfos) > 0 {
		models := make([]core.ModelInfo, 0, len(cached.ModelInfos))
		for _, m := range cached.ModelInfos {
			if m.ID == "" {
				continue
			}
			models = append(models, core.ModelInfo{
				ID:                m.ID,
				DisplayName:       m.DisplayName,
				Created:           m.Created,
				OwnedBy:           m.OwnedBy,
				InputLimit:        m.InputLimit,
				OutputLimit:       m.OutputLimit,
				ConfiguredContext: m.ConfiguredContext,
				InputPrice:        m.InputPrice,
				OutputPrice:       m.OutputPrice,
				ReasoningEfforts:  cloneEfforts(m.ReasoningEfforts),
				ReasoningModes:    cloneEfforts(m.ReasoningModes),
			})
		}
		return models
	}
	models := make([]core.ModelInfo, 0, len(cached.Models))
	for _, id := range cached.Models {
		if id != "" {
			models = append(models, core.ModelInfo{ID: id})
		}
	}
	return models
}

func modelInfosToCache(models []core.ModelInfo) []cache.CachedModel {
	cached := make([]cache.CachedModel, 0, len(models))
	for _, m := range models {
		if m.ID == "" {
			continue
		}
		cached = append(cached, cache.CachedModel{
			ID:                m.ID,
			DisplayName:       m.DisplayName,
			Created:           m.Created,
			OwnedBy:           m.OwnedBy,
			InputLimit:        m.InputLimit,
			OutputLimit:       m.OutputLimit,
			ConfiguredContext: m.ConfiguredContext,
			InputPrice:        m.InputPrice,
			OutputPrice:       m.OutputPrice,
			ReasoningEfforts:  cloneEfforts(m.ReasoningEfforts),
			ReasoningModes:    cloneEfforts(m.ReasoningModes),
		})
	}
	return cached
}

func collectTextParts(parts []Part) string { return core.CollectTextParts(parts) }

var NewOpenAICompatProvider = opencompat.NewOpenAICompatProvider

func openAIParametersFromToolSchema(schema map[string]interface{}, strict bool) map[string]interface{} {
	if fn := responses.ParentHooks.OpenAIParametersFromToolSchema; fn != nil {
		return fn(schema, strict)
	}
	return schema
}
func deepCopyMap(m map[string]interface{}) map[string]interface{} { return core.DeepCopyMap(m) }

// SetFileUploadPolicy overrides the provider's native-file forwarding policy.
func (p *CopilotProvider) SetFileUploadPolicy(policy *FileUploadPolicy) {
	p.fileUploadPolicy = cloneFileUploadPolicy(policy)
}
