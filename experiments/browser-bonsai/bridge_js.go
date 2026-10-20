//go:build js && wasm

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall/js"

	"github.com/samsaffron/term-llm/internal/tools"
)

// jsInference calls the JavaScript-owned browser inference function. Go never
// installs a fallback: without termLLMInfer the turn fails truthfully.
type jsInference struct{}

func (j *jsInference) Complete(ctx context.Context, requestJSON string, onDelta func(string)) (string, error) {
	infer := js.Global().Get("termLLMInfer")
	if infer.Type() != js.TypeFunction {
		return "", fmt.Errorf("termLLMInfer is not installed in the page")
	}

	var once sync.Once
	done := make(chan struct{})
	var result js.Value
	var failure string

	delta := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) > 0 && args[0].Type() == js.TypeString && onDelta != nil {
			onDelta(args[0].String())
		}
		return nil
	})
	var then, catch js.Func
	then = js.FuncOf(func(_ js.Value, args []js.Value) any {
		once.Do(func() {
			if len(args) > 0 {
				result = args[0]
			}
			close(done)
		})
		delta.Release()
		then.Release()
		catch.Release()
		return nil
	})
	catch = js.FuncOf(func(_ js.Value, args []js.Value) any {
		once.Do(func() {
			if len(args) > 0 {
				if message := args[0].Get("message"); message.Type() == js.TypeString {
					failure = message.String()
				} else {
					failure = args[0].String()
				}
			}
			if failure == "" {
				failure = "browser inference failed"
			}
			close(done)
		})
		delta.Release()
		then.Release()
		catch.Release()
		return nil
	})

	infer.Invoke(requestJSON, delta).Call("then", then, catch)

	select {
	case <-done:
		if failure != "" {
			return "", fmt.Errorf("%s", failure)
		}
		if result.Type() != js.TypeString {
			return "", fmt.Errorf("termLLMInfer returned %s, want JSON string", result.Type().String())
		}
		return result.String(), nil
	case <-ctx.Done():
		j.CancelInference()
		return "", ctx.Err()
	}
}

func (j *jsInference) CancelInference() {
	cancel := js.Global().Get("termLLMCancelInference")
	if cancel.Type() == js.TypeFunction {
		cancel.Invoke()
	}
}

// bridgeRequest is the JSON shape the page sends for one HTTP request.
type bridgeRequest struct {
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

type requestBridge struct {
	app     *App
	handler http.Handler

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func newRequestBridge(app *App) *requestBridge {
	return &requestBridge{app: app, handler: app.Handler(), cancels: map[string]context.CancelFunc{}}
}

// request returns a JavaScript Promise resolving to {status, headers, body}.
// onEvent(status, headersJSON, chunk, done) streams header readiness and body
// fragments in order, ending with exactly one done=true event.
func (b *requestBridge) request(requestJSON string, onEvent js.Value) js.Value {
	var resolve, reject js.Value
	executor := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) > 0 {
			resolve = args[0]
		}
		if len(args) > 1 {
			reject = args[1]
		}
		return nil
	})
	promise := js.Global().Get("Promise").New(executor)
	executor.Release()

	var req bridgeRequest
	if err := json.Unmarshal([]byte(requestJSON), &req); err != nil {
		reject.Invoke(jsError("invalid bridge request JSON: " + err.Error()))
		return promise
	}
	if req.ID == "" {
		req.ID = "req_" + randomHex(6)
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	target := req.Path
	if target == "" {
		target = "/"
	}
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "http://browser.local" + target
	}

	ctx, cancel := context.WithCancel(context.Background())
	b.mu.Lock()
	b.cancels[req.ID] = cancel
	b.mu.Unlock()

	go func() {
		defer cancel()
		defer func() {
			b.mu.Lock()
			delete(b.cancels, req.ID)
			b.mu.Unlock()
		}()

		writer := newBridgeWriter(ctx, onEvent)
		defer func() {
			if recovered := recover(); recovered != nil {
				// A panic still resolves the promise: the page must not hang.
				if !writer.wroteHeader {
					writer.status = http.StatusInternalServerError
					writer.wroteHeader = true
				}
				writer.body.WriteString(fmt.Sprintf("panic: %v", recovered))
			}
			writer.finish(resolve)
		}()

		httpReq := httptest.NewRequest(method, target, strings.NewReader(req.Body))
		httpReq = httpReq.WithContext(ctx)
		for key, value := range req.Headers {
			httpReq.Header.Set(key, value)
		}
		b.handler.ServeHTTP(writer, httpReq)
	}()

	return promise
}

func (b *requestBridge) abort(id string) {
	b.mu.Lock()
	cancel := b.cancels[id]
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func jsError(message string) js.Value {
	return js.Global().Get("Error").New(message)
}

// bridgeWriter is an http.ResponseWriter that forwards header readiness and
// body writes to JavaScript and buffers the complete body for the promise.
type bridgeWriter struct {
	ctx     context.Context
	onEvent js.Value

	header      http.Header
	status      int
	wroteHeader bool
	finished    bool
	body        strings.Builder
}

func newBridgeWriter(ctx context.Context, onEvent js.Value) *bridgeWriter {
	return &bridgeWriter{ctx: ctx, onEvent: onEvent, header: http.Header{}}
}

func (w *bridgeWriter) Header() http.Header { return w.header }

func (w *bridgeWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.sendHeaders()
}

func (w *bridgeWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	w.body.Write(p)
	if len(p) > 0 {
		w.event(w.status, string(p), false)
	}
	return len(p), nil
}

func (w *bridgeWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
}

func (w *bridgeWriter) sendHeaders() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.event(w.status, "", false)
}

func (w *bridgeWriter) event(status int, chunk string, done bool) {
	if w.onEvent.Type() != js.TypeFunction || w.finished {
		return
	}
	w.onEvent.Invoke(status, headersJSON(w.header), chunk, done)
}

func (w *bridgeWriter) finish(resolve js.Value) {
	if w.finished {
		return
	}
	w.finished = true
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	if w.onEvent.Type() == js.TypeFunction {
		w.onEvent.Invoke(w.status, headersJSON(w.header), "", true)
	}
	result := js.Global().Get("Object").New()
	result.Set("status", w.status)
	headers := js.Global().Get("Object").New()
	for key := range w.header {
		headers.Set(strings.ToLower(key), w.header.Get(key))
	}
	result.Set("headers", headers)
	result.Set("body", w.body.String())
	resolve.Invoke(result)
}

func headersJSON(header http.Header) string {
	out := make(map[string]string, len(header))
	for key := range header {
		out[strings.ToLower(key)] = header.Get(key)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// installExports publishes the frozen interface. Existing JS inference and
// cancellation functions are never overwritten.
func installExports(app *App, bridge *requestBridge) {
	global := js.Global()
	requestFunc := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) == 0 {
			return jsError("termLLMRequest requires a request JSON string")
		}
		var onEvent js.Value
		if len(args) > 1 {
			onEvent = args[1]
		}
		return bridge.request(args[0].String(), onEvent)
	})
	global.Set("termLLMRequest", requestFunc)
	global.Set("__termLLMRequest", requestFunc)

	abortFunc := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) > 0 {
			bridge.abort(args[0].String())
		}
		return nil
	})
	global.Set("termLLMAbort", abortFunc)
	global.Set("__termLLMAbort", abortFunc)

	infoFunc := js.FuncOf(func(js.Value, []js.Value) any {
		info := js.Global().Get("Object").New()
		info.Set("go", "js/wasm")
		info.Set("workspace", app.workspace)
		info.Set("model", demoModelName)
		info.Set("provider", demoProviderName)
		info.Set("scope", app.instanceID)
		return info
	})
	global.Set("termLLMInfo", infoFunc)

	workspaceFunc := js.FuncOf(func(js.Value, []js.Value) any {
		files := js.Global().Get("Array").New()
		for _, file := range app.workspaceSnapshot() {
			entry := js.Global().Get("Object").New()
			entry.Set("path", file.Path)
			entry.Set("size", file.Size)
			files.Call("push", entry)
		}
		return files
	})
	global.Set("termLLMWorkspace", workspaceFunc)
	global.Set("__termLLMWorkspace", workspaceFunc)

	resolveReady := publishReadyPromise()
	resolveReady()
}

// publishReadyPromise installs termLLMReady and returns its resolver.
func publishReadyPromise() func() {
	var resolve js.Value
	executor := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) > 0 {
			resolve = args[0]
		}
		return nil
	})
	promise := js.Global().Get("Promise").New(executor)
	executor.Release()
	js.Global().Set("termLLMReady", promise)
	return func() {
		if resolve.Type() == js.TypeFunction {
			resolve.Invoke()
		}
	}
}

func publishFatal(err error) {
	var reject js.Value
	executor := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) > 1 {
			reject = args[1]
		}
		return nil
	})
	promise := js.Global().Get("Promise").New(executor)
	executor.Release()
	js.Global().Set("termLLMReady", promise)
	if reject.Type() == js.TypeFunction {
		reject.Invoke(jsError(err.Error()))
	}
	console := js.Global().Get("console")
	if console.Type() == js.TypeObject {
		console.Call("error", "term-llm browser boot failed:", err.Error())
	}
}

// workspaceSnapshot exposes the virtual workspace for the demo footer and smoke.
func (a *App) workspaceSnapshot() []tools.WorkspaceFile {
	files, err := tools.WorkspaceSnapshot(a.workspace)
	if err != nil {
		return nil
	}
	return files
}
