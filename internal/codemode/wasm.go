//go:build !js

package codemode

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:embed quickjs.wasm
var wasm []byte

//go:embed prelude.js
var prelude string

var compilationCache = wazero.NewCompilationCache()

// Available reports whether this platform supports the sandbox.
func Available() bool { return true }

type request struct {
	Kind  string          `json:"kind"`
	ID    uint64          `json:"id"`
	Name  string          `json:"name"`
	Args  json.RawMessage `json:"args"`
	Value json.RawMessage `json:"value"`
}
type completion struct {
	id    uint64
	value json.RawMessage
	err   error
}
type machine struct {
	ctx          context.Context
	mod          api.Module
	modules      Modules
	err          error
	queue        []request
	active       time.Duration
	started      time.Time
	depth        int
	settler      uint64
	result       Result
	textBytes    int
	queuedText   int
	queuedImages int
	queuedItems  int
	queuedCalls  int
	policy       machinePolicy
	sink         func(kind string, value json.RawMessage) error
}

// Run owns every WASM operation on the calling goroutine. Workers exchange copied JSON only.
func Run(ctx context.Context, in Input, names []string, host Host, modules Modules) Result {
	return run(ctx, in, names, host, modules, legacyMachinePolicy(), nil, nil)
}

func run(ctx context.Context, in Input, names []string, host Host, modules Modules, policy machinePolicy, sink func(kind string, value json.RawMessage) error, budget *Budget) (result Result) {
	result.Status = "failed"
	if len(in.Code) > policy.code {
		result.Error = "code_limit"
		return
	}
	if len(in.State) == 0 {
		in.State = json.RawMessage(`{}`)
	}
	if err := validateStateObject(in.State, policy.stateTotal, 0, false); err != nil {
		result.Error = "invalid_state"
		return
	}
	timeout := policy.wall
	if in.TimeoutMS != 0 {
		timeout = time.Duration(in.TimeoutMS) * time.Millisecond
		if timeout <= 0 || (policy.maxWall > 0 && timeout > policy.maxWall) {
			result.Error = "invalid_timeout"
			return
		}
	}
	select {
	case vmSlots <- struct{}{}:
		defer func() { <-vmSlots }()
	default:
		result.Error = "vm_capacity: process VM capacity"
		return
	}
	memory := newBudgetMemory(budget)
	if memory == nil {
		result.Error = "memory_capacity: aggregate admission floor unavailable"
		return
	}
	defer memory.Free()
	defer func() {
		if memory.denied.Load() && result.Status != "completed" {
			result.Error = aggregateMemoryError(result.Error)
		}
	}()
	ctx = experimental.WithMemoryAllocator(ctx, memory)
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	// Registered after the timeout cancel so it runs before it. Any start-up or
	// machine error observed after cancellation is a cancellation, not a fresh
	// failure; successful completion is preserved.
	defer func() {
		if ctx.Err() != nil && result.Status != "completed" {
			result.Status = "cancelled"
		}
	}()
	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCompilationCache(compilationCache).WithMemoryLimitPages(uint32(policy.linear/65536)).WithCloseOnContextDone(true))
	defer r.Close(context.Background())
	module, err := withCompilationSlot(ctx, func() (wazero.CompiledModule, error) { return r.CompileModule(ctx, wasm) })
	if err != nil {
		result.Error = err.Error()
		return
	}
	m := &machine{ctx: ctx, modules: modules, result: result, policy: policy, sink: sink}
	if err = m.instantiate(r, module); err != nil {
		result.Error = err.Error()
		return
	}
	defer func() {
		// A context-cancelled instance is already forcibly closed; NEVER call cleanup exports then.
		if !m.mod.IsClosed() && ctx.Err() == nil {
			if m.settler != 0 {
				_, _ = m.call("qjs_free_value", m.settler)
			}
			_, _ = m.mod.ExportedFunction("qjs_destroy").Call(ctx)
		}
	}()
	if err = m.setup(in, names); err != nil {
		m.err = err
	}
	done := make(chan completion, Limits.Calls)
	pending := 0
	calls := 0
	for m.err == nil {
		for len(m.queue) > 0 && m.err == nil {
			q := m.queue[0]
			m.queue = m.queue[1:]
			switch q.Kind {
			case "text", "image":
				m.emit(q)
			case "failed":
				var message string
				if err := json.Unmarshal(q.Value, &message); err != nil {
					m.err = fmt.Errorf("script_error: invalid diagnostic: %w", err)
				} else {
					m.err = fmt.Errorf("script_error: %s", message)
				}
			case "done":
				if pending != 0 {
					m.err = errors.New("unawaited_calls: external effects may still be running")
					break
				}
				if err := validateMachineState(q.Value, m.policy); err != nil {
					m.err = err
					break
				}
				m.result.Status = "completed"
				m.result.StateCommitted = true
				m.result.State = append(json.RawMessage(nil), q.Value...)
				return m.result
			default:
				calls++
				if calls > Limits.Calls {
					m.err = errors.New("call_limit")
					break
				}
				pending++
				go executeRequest(ctx, host, q, done)
			}
		}
		if m.err != nil {
			break
		}
		if m.jobPending() {
			if err = m.job(); err != nil {
				m.err = err
			}
			continue
		}
		if pending == 0 {
			m.err = errors.New("unresolved_promise: no host work or pending jobs")
			break
		}
		select {
		case c := <-done:
			pending--
			if err = m.settle(c); err != nil {
				m.err = err
			}
		case <-ctx.Done():
			m.err = ctx.Err()
		}
	}
	m.result.Error = fmt.Sprint(m.err)
	if ctx.Err() != nil {
		m.result.Status = "cancelled"
	}
	return m.result
}

func executeRequest(ctx context.Context, host Host, q request, done chan<- completion) {
	c := completion{id: q.ID}
	defer func() {
		if v := recover(); v != nil {
			c.err = fmt.Errorf("host_panic: %v", v)
		}
		done <- c
	}()
	switch q.Kind {
	case "tool":
		if host == nil {
			c.err = errors.New("unavailable: no engine dispatcher")
			return
		}
		c.value, c.err = host.Call(ctx, q.Name, append(json.RawMessage(nil), q.Args...))
	case "describe":
		if host == nil {
			c.err = errors.New("unavailable: no engine dispatcher")
			return
		}
		var names []string
		if json.Unmarshal(q.Args, &names) != nil {
			c.err = errors.New("invalid_arguments")
			return
		}
		c.value, c.err = host.Describe(ctx, names)
	case "sleep":
		var ms float64
		if json.Unmarshal(q.Args, &ms) != nil || ms < 0 || ms > 1000 {
			c.err = errors.New("invalid_sleep: max 1000ms")
			return
		}
		timer := time.NewTimer(time.Duration(ms * float64(time.Millisecond)))
		defer timer.Stop()
		select {
		case <-timer.C:
			c.value = json.RawMessage(`null`)
		case <-ctx.Done():
			c.err = ctx.Err()
		}
	default:
		c.err = errors.New("invalid_request")
	}
	if c.err == nil && (len(c.value) > Limits.RawResult || !json.Valid(c.value)) {
		c.err = &Error{Code: "result_limit", Message: "invalid or oversized JSON; dispatched action may have effects"}
	}
}

func (m *machine) emit(q request) {
	if len(m.result.Output) >= m.policy.items {
		m.err = errors.New("output_limit")
		return
	}
	if q.Kind == "text" {
		m.textBytes += len(q.Value)
		if m.textBytes > m.policy.text {
			m.err = errors.New("text_limit")
			return
		}
	}
	m.result.Output = append(m.result.Output, Emission{Kind: q.Kind, Value: append(json.RawMessage(nil), q.Value...)})
}

func (m *machine) instantiate(r wazero.Runtime, module wazero.CompiledModule) error {
	if _, err := wasi_snapshot_preview1.Instantiate(m.ctx, r); err != nil {
		return err
	}
	b := r.NewHostModuleBuilder("env")
	b.NewFunctionBuilder().WithFunc(m.hostCall).Export("host_call")
	b.NewFunctionBuilder().WithFunc(func() uint32 {
		if m.ctx.Err() != nil || m.err != nil || m.active+time.Since(m.started) > m.policy.cpu {
			return 1
		}
		return 0
	}).Export("host_interrupt")
	b.NewFunctionBuilder().WithFunc(func(context.Context, api.Module, uint32, uint32, uint32) {}).Export("host_promise_rejection")
	b.NewFunctionBuilder().WithFunc(m.normalize).Export("host_module_normalize")
	b.NewFunctionBuilder().WithFunc(m.load).Export("host_module_load")
	b.NewFunctionBuilder().WithFunc(func(uint32, uint32) uint32 { return 0 }).Export("host_get_timezone_offset")
	if _, err := b.Instantiate(m.ctx); err != nil {
		return err
	}
	var err error
	m.mod, err = r.InstantiateModule(m.ctx, module, wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize"))
	if err != nil {
		return err
	}
	if v, e := m.call("qjs_init2", 0xffffffff); e != nil || v != 0 {
		return fmt.Errorf("QuickJS init: %d: %w", v, e)
	}
	for _, setting := range []struct {
		name  string
		value uint64
	}{{"qjs_set_memory_limit", uint64(m.policy.heap)}, {"qjs_set_max_stack_size", uint64(m.policy.stack)}, {"qjs_set_interrupt_handler", 1}, {"qjs_set_module_loader", 1}} {
		if _, err = m.call(setting.name, setting.value); err != nil {
			return err
		}
	}
	return nil
}

func (m *machine) call(name string, args ...uint64) (uint64, error) {
	if m.mod.IsClosed() || m.ctx.Err() != nil {
		return 0, errors.New("VM closed")
	}
	if m.depth == 0 {
		m.started = time.Now()
	}
	m.depth++
	values, err := m.mod.ExportedFunction(name).Call(m.ctx, args...)
	m.depth--
	if m.depth == 0 {
		m.active += time.Since(m.started)
	}
	if err != nil {
		return 0, err
	}
	if len(values) == 0 {
		return 0, nil
	}
	return values[0], nil
}
func (m *machine) alloc(s string) (uint64, error) {
	p, err := m.call("wasm_malloc", uint64(len(s)+1))
	if err != nil {
		return 0, err
	}
	if p == 0 || !m.mod.Memory().Write(uint32(p), append([]byte(s), 0)) {
		return 0, errors.New("memory_limit")
	}
	return p, nil
}
func (m *machine) free(p uint64) {
	if !m.mod.IsClosed() {
		_, _ = m.call("wasm_free", p)
	}
}
func (m *machine) eval(s, filename string) (uint64, error) {
	p, err := m.alloc(s)
	if err != nil {
		return 0, err
	}
	defer m.free(p)
	f, err := m.alloc(filename)
	if err != nil {
		return 0, err
	}
	defer m.free(f)
	v, err := m.call("qjs_eval", p, uint64(len(s)), f, 0)
	if err != nil {
		return 0, err
	}
	ex, err := m.call("qjs_is_exception", v)
	if err != nil {
		return 0, err
	}
	if ex != 0 {
		_, _ = m.call("qjs_free_value", v)
		return 0, m.exception()
	}
	return v, nil
}
func (m *machine) exception() error {
	v, err := m.call("qjs_get_exception")
	if err != nil {
		return err
	}
	defer m.call("qjs_free_value", v)
	s, err := m.stringValue(v, m.policy.text)
	if err != nil {
		m.discardException()
		s = "<unprintable exception>"
	}
	stack := m.exceptionStack(v, m.policy.text-len(s)-1)
	if stack != "" {
		s += "\n" + stack
	}
	return fmt.Errorf("script_error: %s", s)
}

// Diagnostic property access can itself throw. Clear that secondary exception
// without formatting it (and potentially recursing into another throwing getter).
func (m *machine) discardException() {
	if v, err := m.call("qjs_get_exception"); err == nil {
		_, _ = m.call("qjs_free_value", v)
	}
}

func (m *machine) exceptionStack(v uint64, limit int) string {
	object, err := m.call("qjs_is_object", v)
	if err != nil || object == 0 || limit <= 0 {
		return ""
	}
	prop, err := m.alloc("stack")
	if err != nil {
		return ""
	}
	defer m.free(prop)
	stack, err := m.call("qjs_get_prop_string", v, prop)
	if err != nil {
		return ""
	}
	defer m.call("qjs_free_value", stack)
	ex, err := m.call("qjs_is_exception", stack)
	if err != nil {
		return ""
	}
	if ex != 0 {
		m.discardException()
		return ""
	}
	isString, err := m.call("qjs_is_string", stack)
	if err != nil || isString == 0 {
		return ""
	}
	s, err := m.stringValue(stack, limit)
	if err != nil {
		m.discardException()
		return ""
	}
	return s
}
func (m *machine) stringValue(v uint64, limit int) (string, error) {
	n, err := m.call("wasm_malloc", 4)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", errors.New("memory_limit")
	}
	defer m.free(n)
	p, err := m.call("qjs_get_string_len", v, n)
	if err != nil {
		return "", err
	}
	if p == 0 {
		return "", errors.New("invalid_string")
	}
	defer m.call("qjs_free_cstring", p)
	size, ok := m.mod.Memory().ReadUint32Le(uint32(n))
	if !ok || size > uint32(limit) {
		return "", errors.New("arguments_limit")
	}
	data, ok := m.mod.Memory().Read(uint32(p), size)
	if !ok {
		return "", errors.New("invalid_pointer")
	}
	return string(data), nil
}
func (m *machine) hostCall(_ context.Context, _ api.Module, _ uint32, _ uint32, _ uint32, argc uint32, argv uint32) uint32 {
	if argc != 1 {
		m.err = errors.New("invalid_bridge_call")
		return 0
	}
	p, ok := m.mod.Memory().ReadUint32Le(argv)
	if !ok {
		m.err = errors.New("invalid_pointer")
		return 0
	}
	isString, err := m.call("qjs_is_string", uint64(p))
	if err != nil || isString == 0 {
		m.err = errors.New("invalid_bridge_string")
		return 0
	}
	s, err := m.stringValue(uint64(p), m.imageMessageLimit())
	if err != nil {
		m.err = err
		return 0
	}
	var q request
	if err = json.Unmarshal([]byte(s), &q); err != nil {
		m.err = err
		return 0
	}
	if m.sink != nil && (q.Kind == "text" || q.Kind == "image") {
		if err = m.sink(q.Kind, append(json.RawMessage(nil), q.Value...)); err != nil {
			m.err = err
			return 0
		}
		v, e := m.call("qjs_get_undefined")
		if e != nil {
			m.err = e
		}
		return uint32(v)
	}
	if len(m.queue) >= Limits.Calls+m.policy.items {
		m.err = errors.New("queue_limit")
		return 0
	}
	if err = m.acceptRequest(q); err != nil {
		m.err = err
		return 0
	}
	m.queue = append(m.queue, q)
	v, err := m.call("qjs_get_undefined")
	if err != nil {
		m.err = err
	}
	return uint32(v)
}

func (m *machine) setup(in Input, names []string) error {
	n, err := m.alloc("__bridge")
	if err != nil {
		return err
	}
	defer m.free(n)
	fn, err := m.call("qjs_new_host_function", n, 8, 1)
	if err != nil {
		return err
	}
	defer m.call("qjs_free_value", fn)
	g, err := m.call("qjs_get_global")
	if err != nil {
		return err
	}
	defer m.call("qjs_free_value", g)
	if _, err = m.call("qjs_set_prop_string", g, n, fn); err != nil {
		return err
	}
	if names == nil {
		names = []string{}
	}
	namesJSON, _ := json.Marshal(names)
	stateJSON, _ := json.Marshal(string(in.State))
	// Construct and capture closure handles before executing any guest source.
	apiValue, err := m.eval("("+prelude+")(__bridge,"+string(namesJSON)+","+string(stateJSON)+","+fmt.Sprint(m.policy.rejectImageState)+")", "prelude.js")
	if err != nil {
		return err
	}
	defer m.call("qjs_free_value", apiValue)
	prop, err := m.alloc("settle")
	if err != nil {
		return err
	}
	defer m.free(prop)
	m.settler, err = m.call("qjs_get_prop_string", apiValue, prop)
	if err != nil {
		return err
	}
	runName, err := m.alloc("run")
	if err != nil {
		return err
	}
	defer m.free(runName)
	run, err := m.call("qjs_get_prop_string", apiValue, runName)
	if err != nil {
		return err
	}
	defer m.call("qjs_free_value", run)
	// Body line one is source line one; no rewriting static imports or retrying effects.
	body, err := m.eval("(async function(){"+in.Code+"\n})", "cell.js")
	if err != nil {
		return err
	}
	defer m.call("qjs_free_value", body)
	return m.invoke(run, []uint64{body})
}

func (m *machine) evalDiscard(s string) (uint64, error) {
	v, e := m.eval(s, "host.js")
	if e == nil {
		_, _ = m.call("qjs_free_value", v)
	}
	return 0, e
}
func (m *machine) jobPending() bool {
	v, e := m.call("qjs_is_job_pending")
	if e != nil {
		m.err = e
	}
	return v != 0
}
func (m *machine) job() error {
	v, e := m.call("qjs_execute_pending_job")
	if e != nil {
		return e
	}
	if int32(v) < 0 {
		return m.exception()
	}
	return nil
}
func (m *machine) settle(c completion) error {
	ok := true
	if c.err != nil {
		ok = false
		var typed *Error
		if !errors.As(c.err, &typed) {
			typed = &Error{Code: "host_error", Message: c.err.Error()}
		}
		c.value, _ = json.Marshal(typed)
	}
	// JSON is passed as a string value, never executable interpolation.
	p, e := m.alloc(string(c.value))
	if e != nil {
		return e
	}
	defer m.free(p)
	data, e := m.call("qjs_new_string", p, uint64(len(c.value)))
	if e != nil {
		return e
	}
	defer m.call("qjs_free_value", data)
	id, e := m.call("qjs_new_number", api.EncodeF64(float64(c.id)))
	if e != nil {
		return e
	}
	defer m.call("qjs_free_value", id)
	flagName := "qjs_get_true"
	if !ok {
		flagName = "qjs_get_false"
	}
	flag, e := m.call(flagName)
	if e != nil {
		return e
	}
	defer m.call("qjs_free_value", flag)
	return m.invoke(m.settler, []uint64{id, flag, data})
}
func (m *machine) invoke(fn uint64, values []uint64) error {
	argv, e := m.call("wasm_malloc", uint64(4*len(values)))
	if e != nil {
		return e
	}
	defer m.free(argv)
	for i, v := range values {
		if !m.mod.Memory().WriteUint32Le(uint32(argv)+uint32(i*4), uint32(v)) {
			return errors.New("invalid_pointer")
		}
	}
	undef, e := m.call("qjs_get_undefined")
	if e != nil {
		return e
	}
	defer m.call("qjs_free_value", undef)
	v, e := m.call("qjs_call", fn, undef, uint64(len(values)), argv)
	if e != nil {
		return e
	}
	defer m.call("qjs_free_value", v)
	ex, e := m.call("qjs_is_exception", v)
	if e != nil {
		return e
	}
	if ex != 0 {
		return m.exception()
	}
	return nil
}
func (m *machine) imageMessageLimit() int { return ((m.policy.imageBytes+2)/3)*4 + 1024 }
func (m *machine) acceptRequest(q request) error {
	switch q.Kind {
	case "text":
		m.queuedItems++
		m.queuedText += len(q.Value)
		if m.queuedText > m.policy.text {
			return errors.New("text_limit")
		}
	case "image":
		m.queuedItems++
		m.queuedImages++
		if m.queuedImages > m.policy.images || len(q.Value) > m.imageMessageLimit() {
			return errors.New("image_limit")
		}
	case "done":
		if len(q.Value) > m.policy.stateTotal {
			return errors.New("state_limit")
		}
	case "failed":
		if len(q.Value) > m.policy.text {
			return errors.New("error_limit")
		}
	default:
		m.queuedCalls++
		if m.queuedCalls > Limits.Calls {
			return errors.New("call_limit")
		}
		if len(q.Args) > Limits.Arguments {
			return errors.New("arguments_limit")
		}
	}
	if m.queuedItems > m.policy.items {
		return errors.New("output_limit")
	}
	return nil
}

// StartCell starts one async cell on its own machine goroutine. The cell's
// context is deliberately detached from request cancellation: only Cancel,
// session teardown or engine policy can stop an admitted cell.
func StartCell(parent context.Context, spec CellSpec) (*Cell, error) {
	if parent == nil {
		parent = context.Background()
	}
	base := context.WithoutCancel(parent)
	if spec.Context != nil {
		base = spec.Context
	}
	ctx, cancel := context.WithCancel(base)
	c := newCell(ctx, cancel)
	go func() {
		var result Result
		func() {
			defer func() {
				if v := recover(); v != nil {
					result = Result{Status: "failed", Error: fmt.Sprintf("panic: %v", v)}
				}
			}()
			result = run(ctx, spec.Input, spec.Names, spec.Host, spec.Modules, sessionMachinePolicy(), spec.Sink, spec.Memory)
		}()
		if spec.OnFinish != nil {
			func() {
				defer func() { _ = recover() }()
				spec.OnFinish(result)
			}()
		}
		c.complete(result)
	}()
	return c, nil
}
