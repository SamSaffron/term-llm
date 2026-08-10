package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
)

const taskTypeName = "term_llm_workflow_task"

// Engine executes one Lua workflow in-process.
type Engine struct {
	Executor AgentExecutor
}

// ExecuteOptions defines one workflow execution.
type ExecuteOptions struct {
	Inputs           map[string]any
	Agent            string
	Provider         string
	Concurrency      int
	AgentTimeout     time.Duration
	CWD              string
	AllowedTools     []string
	AllowedRead      []string
	AllowedWrite     []string
	AllowedShell     []string
	AllowedWorkspace []string
}

// Execute evaluates source and returns the value from its top-level return.
func (e *Engine) Execute(ctx context.Context, source string, options ExecuteOptions) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if e.Executor == nil {
		return nil, fmt.Errorf("workflow agent executor is required")
	}
	if options.Concurrency <= 0 {
		options.Concurrency = 4
	}
	if options.Inputs == nil {
		options.Inputs = map[string]any{}
	}

	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer L.Close()
	if err := openWorkflowLibraries(L); err != nil {
		return nil, err
	}
	L.SetContext(ctx)

	runtime := &luaRuntime{
		engine:  e,
		options: options,
	}
	runtime.install(L)

	chunk, err := L.LoadString(source)
	if err != nil {
		return nil, fmt.Errorf("parse Lua: %w", err)
	}
	L.Push(chunk)
	if err := L.PCall(0, 1, nil); err != nil {
		return nil, fmt.Errorf("execute Lua: %w", err)
	}
	value := L.Get(-1)
	result, err := luaValueToGo(value)
	if err != nil {
		return nil, fmt.Errorf("convert workflow result: %w", err)
	}
	return result, nil
}

type luaRuntime struct {
	engine  *Engine
	options ExecuteOptions
}

func openWorkflowLibraries(L *lua.LState) error {
	libraries := []struct {
		name string
		open lua.LGFunction
	}{
		{lua.BaseLibName, lua.OpenBase},
		{lua.TabLibName, lua.OpenTable},
		{lua.StringLibName, lua.OpenString},
		{lua.MathLibName, lua.OpenMath},
	}
	for _, library := range libraries {
		if err := L.CallByParam(lua.P{
			Fn:      L.NewFunction(library.open),
			NRet:    0,
			Protect: true,
		}, lua.LString(library.name)); err != nil {
			return fmt.Errorf("open Lua %s library: %w", library.name, err)
		}
	}

	// Workflows orchestrate agents; they do not get ambient filesystem, process,
	// package-loading, clock, or randomness capabilities. Agents hold those tools.
	L.SetGlobal("dofile", lua.LNil)
	L.SetGlobal("loadfile", lua.LNil)
	L.SetGlobal("load", lua.LNil)
	L.SetGlobal("loadstring", lua.LNil)
	if mathTable, ok := L.GetGlobal(lua.MathLibName).(*lua.LTable); ok {
		mathTable.RawSetString("random", lua.LNil)
		mathTable.RawSetString("randomseed", lua.LNil)
	}
	return nil
}

func (r *luaRuntime) install(L *lua.LState) {
	L.SetGlobal("workflow", L.NewFunction(func(L *lua.LState) int {
		L.CheckTable(1)
		return 0
	}))
	L.SetGlobal("input", L.NewFunction(r.input))
	L.SetGlobal("agent", L.NewFunction(r.agent))
	L.SetGlobal("run_agent", L.NewFunction(r.runAgent))
	L.SetGlobal("create_workspace", L.NewFunction(r.createWorkspace))
	L.SetGlobal("await", L.NewFunction(r.await))
	L.SetGlobal("parallel", L.NewFunction(r.parallel))
	L.SetGlobal("parallel_settled", L.NewFunction(r.parallelSettled))
	L.SetGlobal("join", L.NewFunction(luaJoin))

	metatable := L.NewTypeMetatable(taskTypeName)
	L.SetField(metatable, "__index", L.SetFuncs(L.NewTable(), map[string]lua.LGFunction{}))
}

func (r *luaRuntime) createWorkspace(L *lua.LState) int {
	table := L.CheckTable(1)
	source := tableString(L, table, "source")
	if !pathWithinAny(source, r.options.AllowedRead) {
		L.RaiseError("workspace source %q exceeds the workflow read ceiling", source)
	}
	root := tableString(L, table, "root")
	if root == "" {
		if len(r.options.AllowedWorkspace) == 0 {
			L.RaiseError("workspace root is required")
		}
		root = r.options.AllowedWorkspace[0]
	}
	if !pathWithinAny(root, r.options.AllowedWorkspace) {
		L.RaiseError("workspace root %q exceeds the workspace ceiling", root)
	}
	if pathWithinAny(root, []string{source}) {
		L.RaiseError("workspace root %q must not be inside source %q", root, source)
	}
	name := tableString(L, table, "name")
	if name == "" {
		name = "workflow"
	}
	name = sanitizeWorkspaceName(name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		L.RaiseError("create workspace root: %v", err)
	}
	destination, err := os.MkdirTemp(root, name+"-")
	if err != nil {
		L.RaiseError("create workspace: %v", err)
	}
	if err := os.CopyFS(destination, os.DirFS(source)); err != nil {
		_ = os.RemoveAll(destination)
		L.RaiseError("copy workspace: %v", err)
	}
	L.Push(lua.LString(destination))
	return 1
}

func sanitizeWorkspaceName(name string) string {
	var result strings.Builder
	for _, character := range name {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
			result.WriteRune(character)
		} else {
			result.WriteByte('-')
		}
	}
	if result.Len() == 0 {
		return "workflow"
	}
	return result.String()
}

func (r *luaRuntime) input(L *lua.LState) int {
	name := L.CheckString(1)
	if value, ok := r.options.Inputs[name]; ok {
		converted, err := goValueToLua(L, value)
		if err != nil {
			L.RaiseError("input %q: %v", name, err)
		}
		L.Push(converted)
		return 1
	}
	if L.GetTop() >= 2 {
		L.Push(L.Get(2))
		return 1
	}
	L.RaiseError("required input %q was not provided", name)
	return 0
}

func (r *luaRuntime) agent(L *lua.LState) int {
	return r.makeAgentTask(L, false)
}

func (r *luaRuntime) runAgent(L *lua.LState) int {
	return r.makeAgentTask(L, true)
}

func (r *luaRuntime) makeAgentTask(L *lua.LState, dynamic bool) int {
	table := L.CheckTable(1)
	prompt := tableString(L, table, "prompt")
	if strings.TrimSpace(prompt) == "" {
		L.RaiseError("agent prompt is required")
	}
	request := AgentRequest{
		Prompt:   prompt,
		Agent:    tableString(L, table, "agent"),
		Provider: tableString(L, table, "provider"),
		Label:    tableString(L, table, "label"),
		CWD:      r.options.CWD,
	}
	if dynamic {
		request.Dynamic = true
		request.SystemMessage = tableString(L, table, "system")
		request.Tools = tableStringSlice(L, table, "tools")
		request.ReadDirs = tableStringSlice(L, table, "read_dirs")
		request.WriteDirs = tableStringSlice(L, table, "write_dirs")
		request.ShellAllow = tableStringSlice(L, table, "shell_allow")
		request.MaxTurns = tableInt(L, table, "max_turns")
		request.Require = tableRequirement(L, table, "require")
		if cwd := tableString(L, table, "cwd"); cwd != "" {
			request.CWD = cwd
		}
		if err := r.authorizeAgentRequest(&request); err != nil {
			L.RaiseError("authorize run_agent: %v", err)
		}
	}
	if r.options.Agent != "" {
		request.Agent = r.options.Agent
	}
	if r.options.Provider != "" {
		request.Provider = r.options.Provider
	}

	task := &agentTask{runtime: r, request: request}
	userData := L.NewUserData()
	userData.Value = task
	L.SetMetatable(userData, L.GetTypeMetatable(taskTypeName))
	L.Push(userData)
	return 1
}

func (r *luaRuntime) await(L *lua.LState) int {
	task := checkAgentTask(L, 1)
	result, err := task.run(L.Context())
	if err != nil {
		L.RaiseError("agent task failed: %v", err)
	}
	if task.request.Dynamic {
		value, conversionErr := goValueToLua(L, agentResultMap(result))
		if conversionErr != nil {
			L.RaiseError("convert agent result: %v", conversionErr)
		}
		L.Push(value)
	} else {
		L.Push(lua.LString(result.Stdout))
	}
	return 1
}

type taskOutcome struct {
	result AgentResult
	err    error
}

func (r *luaRuntime) parallel(L *lua.LState) int {
	tasks, outcomes := r.executeParallel(L)
	values := L.NewTable()
	for index, outcome := range outcomes {
		if outcome.err != nil {
			L.RaiseError("parallel agent task %d failed: %v", index+1, outcome.err)
		}
		if tasks[index].request.Dynamic {
			value, err := goValueToLua(L, agentResultMap(outcome.result))
			if err != nil {
				L.RaiseError("convert parallel agent result: %v", err)
			}
			values.Append(value)
		} else {
			values.Append(lua.LString(outcome.result.Stdout))
		}
	}
	L.Push(values)
	return 1
}

func (r *luaRuntime) parallelSettled(L *lua.LState) int {
	_, outcomes := r.executeParallel(L)
	values := L.NewTable()
	for _, outcome := range outcomes {
		item := map[string]any{"ok": outcome.err == nil, "result": agentResultMap(outcome.result)}
		if outcome.err != nil {
			item["error"] = outcome.err.Error()
		}
		value, err := goValueToLua(L, item)
		if err != nil {
			L.RaiseError("convert settled agent result: %v", err)
		}
		values.Append(value)
	}
	L.Push(values)
	return 1
}

func (r *luaRuntime) executeParallel(L *lua.LState) ([]*agentTask, []taskOutcome) {
	table := L.CheckTable(1)
	tasks := make([]*agentTask, table.Len())
	for index := range tasks {
		value := table.RawGetInt(index + 1)
		userData, ok := value.(*lua.LUserData)
		if !ok {
			L.ArgError(1, fmt.Sprintf("item %d is not an agent task", index+1))
		}
		task, ok := userData.Value.(*agentTask)
		if !ok {
			L.ArgError(1, fmt.Sprintf("item %d is not an agent task", index+1))
		}
		tasks[index] = task
	}

	outcomes := make([]taskOutcome, len(tasks))
	if len(tasks) == 0 {
		return tasks, outcomes
	}
	workerCount := min(r.options.Concurrency, len(tasks))
	indices := make(chan int)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	ctx := L.Context()
	for range workerCount {
		go func() {
			defer workers.Done()
			for index := range indices {
				outcomes[index].result, outcomes[index].err = tasks[index].run(ctx)
			}
		}()
	}
	for index := range tasks {
		indices <- index
	}
	close(indices)
	workers.Wait()
	return tasks, outcomes
}

func agentResultMap(result AgentResult) map[string]any {
	encoded, _ := json.Marshal(result)
	var value map[string]any
	_ = json.Unmarshal(encoded, &value)
	return value
}

type agentTask struct {
	runtime *luaRuntime
	request AgentRequest
	once    sync.Once
	result  AgentResult
	err     error
}

func (t *agentTask) run(ctx context.Context) (AgentResult, error) {
	t.once.Do(func() {
		executeCtx := ctx
		cancel := func() {}
		if t.runtime.options.AgentTimeout > 0 {
			executeCtx, cancel = context.WithTimeout(ctx, t.runtime.options.AgentTimeout)
		}
		defer cancel()
		t.result, t.err = t.runtime.engine.Executor.Execute(executeCtx, t.request)
	})
	return t.result, t.err
}

func checkAgentTask(L *lua.LState, index int) *agentTask {
	userData := L.CheckUserData(index)
	if task, ok := userData.Value.(*agentTask); ok {
		return task
	}
	L.ArgError(index, "agent task expected")
	return nil
}

func (r *luaRuntime) authorizeAgentRequest(request *AgentRequest) error {
	if err := requireSubset("tool", request.Tools, r.options.AllowedTools); err != nil {
		return err
	}
	if err := requireSubset("shell pattern", request.ShellAllow, r.options.AllowedShell); err != nil {
		return err
	}
	for _, directory := range request.ReadDirs {
		if !pathWithinAny(directory, r.options.AllowedRead) {
			return fmt.Errorf("read directory %q exceeds the workflow capability ceiling", directory)
		}
	}
	for _, directory := range request.WriteDirs {
		if !pathWithinAny(directory, r.options.AllowedWrite) {
			return fmt.Errorf("write directory %q exceeds the workflow capability ceiling", directory)
		}
	}
	if request.CWD != r.options.CWD && !pathWithinAny(request.CWD, append(append([]string{}, r.options.AllowedRead...), r.options.AllowedWrite...)) {
		return fmt.Errorf("working directory %q exceeds the workflow capability ceiling", request.CWD)
	}
	return nil
}

func requireSubset(kind string, requested, allowed []string) error {
	ceiling := make(map[string]struct{}, len(allowed))
	for _, value := range allowed {
		ceiling[value] = struct{}{}
	}
	for _, value := range requested {
		if _, ok := ceiling[value]; !ok {
			return fmt.Errorf("%s %q exceeds the workflow capability ceiling", kind, value)
		}
	}
	return nil
}

func pathWithinAny(path string, roots []string) bool {
	path = canonicalCapabilityPath(path)
	for _, root := range roots {
		root = canonicalCapabilityPath(root)
		relative, err := filepath.Rel(root, path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func canonicalCapabilityPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	absolute = filepath.Clean(absolute)
	for existing := absolute; ; existing = filepath.Dir(existing) {
		if evaluated, evalErr := filepath.EvalSymlinks(existing); evalErr == nil {
			remainder, relErr := filepath.Rel(existing, absolute)
			if relErr == nil {
				return filepath.Clean(filepath.Join(evaluated, remainder))
			}
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
	}
	return absolute
}

func tableStringSlice(L *lua.LState, table *lua.LTable, key string) []string {
	value := table.RawGetString(key)
	if value == lua.LNil {
		return nil
	}
	items, ok := value.(*lua.LTable)
	if !ok {
		L.RaiseError("agent %s must be an array of strings", key)
	}
	result := make([]string, 0, items.Len())
	for index := 1; index <= items.Len(); index++ {
		item, ok := items.RawGetInt(index).(lua.LString)
		if !ok {
			L.RaiseError("agent %s item %d must be a string", key, index)
		}
		result = append(result, string(item))
	}
	return result
}

func tableRequirement(L *lua.LState, table *lua.LTable, key string) *CommandRequirement {
	value := table.RawGetString(key)
	if value == lua.LNil {
		return nil
	}
	requirementTable, ok := value.(*lua.LTable)
	if !ok {
		L.RaiseError("agent %s must be a table", key)
	}
	return &CommandRequirement{
		Command:        tableString(L, requirementTable, "command"),
		ExitCode:       tableInt(L, requirementTable, "exit_code"),
		Repetitions:    tableInt(L, requirementTable, "repetitions"),
		OutputContains: tableString(L, requirementTable, "output_contains"),
		ArtifactGlob:   tableString(L, requirementTable, "artifact_glob"),
	}
}

func tableInt(L *lua.LState, table *lua.LTable, key string) int {
	value := table.RawGetString(key)
	if value == lua.LNil {
		return 0
	}
	number, ok := value.(lua.LNumber)
	if !ok || number < 0 || number != lua.LNumber(int(number)) {
		L.RaiseError("agent %s must be a non-negative integer", key)
	}
	return int(number)
}

func tableString(L *lua.LState, table *lua.LTable, key string) string {
	value := table.RawGetString(key)
	if value == lua.LNil {
		return ""
	}
	if text, ok := value.(lua.LString); ok {
		return string(text)
	}
	L.RaiseError("agent %s must be a string", key)
	return ""
}

func luaJoin(L *lua.LState) int {
	values := L.CheckTable(1)
	separator := L.OptString(2, "")
	parts := make([]string, 0, values.Len())
	for index := 1; index <= values.Len(); index++ {
		parts = append(parts, values.RawGetInt(index).String())
	}
	L.Push(lua.LString(strings.Join(parts, separator)))
	return 1
}

func goValueToLua(L *lua.LState, value any) (lua.LValue, error) {
	switch value := value.(type) {
	case nil:
		return lua.LNil, nil
	case bool:
		return lua.LBool(value), nil
	case string:
		return lua.LString(value), nil
	case float64:
		return lua.LNumber(value), nil
	case float32:
		return lua.LNumber(value), nil
	case int:
		return lua.LNumber(value), nil
	case int64:
		return lua.LNumber(value), nil
	case []any:
		table := L.NewTable()
		for _, item := range value {
			converted, err := goValueToLua(L, item)
			if err != nil {
				return nil, err
			}
			table.Append(converted)
		}
		return table, nil
	case map[string]any:
		table := L.NewTable()
		for key, item := range value {
			converted, err := goValueToLua(L, item)
			if err != nil {
				return nil, err
			}
			table.RawSetString(key, converted)
		}
		return table, nil
	default:
		return nil, fmt.Errorf("unsupported Go value %T", value)
	}
}
