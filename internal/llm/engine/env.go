package engine

import (
	"context"
	"reflect"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// DebugLogger is the request/event sink the engine writes to in debug mode.
type DebugLogger interface {
	LogRequest(provider, model string, req core.Request)
	LogTurnRequest(turn int, provider, model string, req core.Request)
	LogEvent(event core.Event)
}

// Env injects root-package behaviour the turn loop depends on. Every field is
// optional; a nil field falls back to neutral behaviour. The root llm package
// installs its implementation with SetEnv at init time.
type Env struct {
	CompactionEnv

	// ModelLimits resolves model input limits without importing model tables.
	ModelLimits core.ModelLimits
	// UnwrapProvider returns the provider beneath transparent wrappers such as
	// retry (nil = the provider itself).
	UnwrapProvider func(core.Provider) core.Provider
	// IsNonRecoverableStreamError reports provider stream errors that are safe
	// to replay when nothing was committed (nil = false).
	IsNonRecoverableStreamError func(error) bool
	// WithRunContext derives the context for one engine run (nil = unchanged),
	// for provider-specific lifetimes such as websocket continuations.
	WithRunContext func(context.Context) context.Context
	// WithDebugDiagnosticSink attaches a debug logger as the diagnostic sink
	// (nil = unchanged).
	WithDebugDiagnosticSink func(context.Context, DebugLogger) context.Context

	// Debug output helpers (nil = no output).
	DebugToolCall      func(enabled bool, call core.ToolCall)
	DebugToolResult    func(enabled bool, id, name, content string)
	DebugRawRequest    func(enabled bool, providerName, credential string, req core.Request, label string)
	DebugRawEvent      func(enabled bool, event core.Event)
	DebugRawToolResult func(enabled bool, id, name, content string)
}

var env Env

// SetEnv installs the engine environment. It is intended to be called once,
// during package initialisation, before any Engine runs.
func SetEnv(e Env) { env = e }

func (e Env) unwrapProvider(p core.Provider) core.Provider {
	if e.UnwrapProvider == nil {
		return p
	}
	return e.UnwrapProvider(p)
}

func (e Env) isNonRecoverableStreamError(err error) bool {
	return e.IsNonRecoverableStreamError != nil && e.IsNonRecoverableStreamError(err)
}

func (e Env) withRunContext(ctx context.Context) context.Context {
	if e.WithRunContext == nil {
		return ctx
	}
	return e.WithRunContext(ctx)
}

func (e Env) withDebugDiagnosticSink(ctx context.Context, l DebugLogger) context.Context {
	if e.WithDebugDiagnosticSink == nil || isNilDebugLogger(l) {
		return ctx
	}
	return e.WithDebugDiagnosticSink(ctx, l)
}

func (e Env) debugToolCall(enabled bool, call core.ToolCall) {
	if e.DebugToolCall != nil {
		e.DebugToolCall(enabled, call)
	}
}

func (e Env) debugToolResult(enabled bool, id, name, content string) {
	if e.DebugToolResult != nil {
		e.DebugToolResult(enabled, id, name, content)
	}
}

func (e Env) debugRawRequest(enabled bool, providerName, credential string, req core.Request, label string) {
	if e.DebugRawRequest != nil {
		e.DebugRawRequest(enabled, providerName, credential, req, label)
	}
}

func (e Env) debugRawEvent(enabled bool, event core.Event) {
	if e.DebugRawEvent != nil {
		e.DebugRawEvent(enabled, event)
	}
}

func (e Env) debugRawToolResult(enabled bool, id, name, content string) {
	if e.DebugRawToolResult != nil {
		e.DebugRawToolResult(enabled, id, name, content)
	}
}

// isNilDebugLogger reports whether l is nil or a typed nil pointer, so a nil
// *llm.DebugLogger passed through the interface still disables logging.
func isNilDebugLogger(l DebugLogger) bool {
	if l == nil {
		return true
	}
	v := reflect.ValueOf(l)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// RequestContextTool is an optional interface for explicitly configured tools
// that own restorable, session-scoped model context. Engine invokes it only when
// that exact tool survives final request filtering and the provider supports
// tool calls.
type RequestContextTool interface {
	core.Tool
	PrepareRequestContext(ctx context.Context, sessionID string, messages []core.Message) ([]core.Message, error)
	PrepareCompactionContext(ctx context.Context, sessionID string, result *CompactionResult) error
}

// SessionStateResetter clears a tool's cached projection for one durable
// session after an out-of-band transcript mutation such as undo/redo.
type SessionStateResetter interface {
	ResetSessionState(sessionID string)
}
