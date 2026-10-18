package engine

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"sync"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// serviceTierFast mirrors llm.ServiceTierFast, which engine cannot import.
const serviceTierFast = "priority"

type namedTestTool struct{ name string }

func (t *namedTestTool) Spec() core.ToolSpec {
	return core.ToolSpec{Name: t.name, Description: t.name, Schema: map[string]any{"type": "object"}}
}

func (t *namedTestTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	return core.TextOutput(t.name), nil
}

func (t *namedTestTool) Preview(args json.RawMessage) string { return t.name }

// useTestInputLimit installs a single provider/model input limit for the test,
// standing in for the root package's model-limit tables.
func useTestInputLimit(t testing.TB, providerName, modelName string, limit int) {
	t.Helper()
	prev := env
	t.Cleanup(func() { env = prev })
	next := prev
	next.ModelLimits.InputLimit = func(p, m string) int {
		if p == providerName && m == modelName {
			return limit
		}
		return 0
	}
	next.InputLimitForModel = func(m string) int {
		if m == modelName {
			return limit
		}
		return 0
	}
	env = next
}

// testNonRecoverableStreamError stands in for the responses provider's
// non-recoverable stream error, which the engine recognises through env.
type testNonRecoverableStreamError struct{ Err error }

func (e *testNonRecoverableStreamError) Error() string { return e.Err.Error() }
func (e *testNonRecoverableStreamError) Unwrap() error { return e.Err }

func init() {
	env.IsNonRecoverableStreamError = func(err error) bool {
		var target *testNonRecoverableStreamError
		return errors.As(err, &target)
	}
}

func testWebSearchToolSpec() core.ToolSpec {
	return core.ToolSpec{
		Name:        core.WebSearchToolName,
		Description: "Search the web for current information.",
		Schema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"query": map[string]any{"type": "string"}},
			"required":             []string{"query"},
			"additionalProperties": false,
		},
	}
}

func testReadURLToolSpec() core.ToolSpec {
	return core.ToolSpec{
		Name:        core.ReadURLToolName,
		Description: "Fetch and read a web page.",
		Schema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"url": map[string]any{"type": "string"}},
			"required":             []string{"url"},
			"additionalProperties": false,
		},
	}
}

type testReadURLTool struct{}

func newTestReadURLTool() *testReadURLTool { return &testReadURLTool{} }

func (*testReadURLTool) Spec() core.ToolSpec { return testReadURLToolSpec() }

func (*testReadURLTool) Execute(context.Context, json.RawMessage) (core.ToolOutput, error) {
	return core.TextOutput("page"), nil
}

func (*testReadURLTool) Preview(json.RawMessage) string { return "" }

// lastMessageHasToolResults reports whether the conversation ends with tool
// results, i.e. the request is the follow-up round of the current turn.
func lastMessageHasToolResults(msgs []core.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	last := msgs[len(msgs)-1]
	if last.Role == core.RoleTool {
		return true
	}
	for _, part := range last.Parts {
		if part.Type == core.PartToolResult {
			return true
		}
	}
	return false
}

type dynamicPublishingProvider struct {
	*mockprovider.MockProvider
	mu        sync.Mutex
	published []core.ToolSpec
}

func (p *dynamicPublishingProvider) PublishDynamicTools(tools []core.ToolSpec) error {
	p.mu.Lock()
	p.published = append(p.published, tools...)
	p.mu.Unlock()
	return nil
}
