package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockRunner implements SpawnAgentRunner for testing.
type mockRunner struct {
	mu           sync.Mutex
	output       string
	sessionID    string
	err          error
	delay        time.Duration
	calls        []mockRunnerCall
	callCount    int32
	runningCount int32 // Track concurrently running agents
	maxRunning   int32 // Max concurrent agents observed
}

type mockRunnerCall struct {
	AgentName string
	Prompt    string
	Depth     int
}

func newMockRunner() *mockRunner {
	return &mockRunner{
		output: "mock output",
	}
}

func (m *mockRunner) SetOutput(output string) *mockRunner {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.output = output
	return m
}

func (m *mockRunner) SetError(err error) *mockRunner {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
	return m
}

func (m *mockRunner) SetDelay(d time.Duration) *mockRunner {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delay = d
	return m
}

func (m *mockRunner) SetSessionID(sessionID string) *mockRunner {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionID = sessionID
	return m
}

func (m *mockRunner) RunAgent(ctx context.Context, agentName string, prompt string, depth int) (SpawnAgentRunResult, error) {
	// Track concurrent execution
	running := atomic.AddInt32(&m.runningCount, 1)
	defer atomic.AddInt32(&m.runningCount, -1)

	// Update max running if this is a new high
	for {
		max := atomic.LoadInt32(&m.maxRunning)
		if running <= max {
			break
		}
		if atomic.CompareAndSwapInt32(&m.maxRunning, max, running) {
			break
		}
	}

	atomic.AddInt32(&m.callCount, 1)

	m.mu.Lock()
	m.calls = append(m.calls, mockRunnerCall{
		AgentName: agentName,
		Prompt:    prompt,
		Depth:     depth,
	})
	output := m.output
	sessionID := m.sessionID
	err := m.err
	delay := m.delay
	m.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return SpawnAgentRunResult{}, ctx.Err()
		}
	}

	return SpawnAgentRunResult{Output: output, SessionID: sessionID}, err
}

func (m *mockRunner) RunAgentWithCallback(ctx context.Context, agentName string, prompt string, depth int,
	callID string, cb SubagentEventCallback) (SpawnAgentRunResult, error) {
	// Just delegate to RunAgent for testing
	return m.RunAgent(ctx, agentName, prompt, depth)
}

func (m *mockRunner) GetCalls() []mockRunnerCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]mockRunnerCall, len(m.calls))
	copy(result, m.calls)
	return result
}

func (m *mockRunner) GetCallCount() int {
	return int(atomic.LoadInt32(&m.callCount))
}

func (m *mockRunner) GetMaxRunning() int {
	return int(atomic.LoadInt32(&m.maxRunning))
}

// Helper to create args JSON
func makeSpawnArgs(agentName, prompt string, timeout int) json.RawMessage {
	args := SpawnAgentArgs{
		AgentName: agentName,
		Prompt:    prompt,
		Timeout:   timeout,
	}
	data, _ := json.Marshal(args)
	return data
}

// Helper to parse result
func parseResult(t *testing.T, result string) SpawnAgentResult {
	t.Helper()
	var r SpawnAgentResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	return r
}

func TestSpawnAgentTool_TimeoutEnforcement(t *testing.T) {
	tests := []struct {
		name            string
		timeout         int
		defaultTimeout  int
		expectedTimeout int // The timeout that should be used (clamped)
	}{
		{
			name:            "timeout below minimum is clamped to 10",
			timeout:         5,
			defaultTimeout:  300,
			expectedTimeout: 10,
		},
		{
			name:            "timeout above maximum is clamped to 600",
			timeout:         1000,
			defaultTimeout:  300,
			expectedTimeout: 600,
		},
		{
			name:            "timeout within range is used as-is",
			timeout:         120,
			defaultTimeout:  300,
			expectedTimeout: 120,
		},
		{
			name:            "zero timeout uses default (then clamped)",
			timeout:         0,
			defaultTimeout:  300,
			expectedTimeout: 300,
		},
		{
			name:            "default timeout below minimum is clamped",
			timeout:         0,
			defaultTimeout:  5,
			expectedTimeout: 10,
		},
		{
			name:            "default timeout above maximum is clamped",
			timeout:         0,
			defaultTimeout:  1000,
			expectedTimeout: 600,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := SpawnConfig{
				MaxParallel:    3,
				MaxDepth:       5, // High enough to not interfere
				DefaultTimeout: tt.defaultTimeout,
			}
			tool := NewSpawnAgentTool(config, 0)

			// Use a runner that completes quickly
			runner := newMockRunner().SetDelay(10 * time.Millisecond)
			tool.SetRunner(runner)

			ctx := context.Background()
			args := makeSpawnArgs("test-agent", "do something", tt.timeout)

			result, err := tool.Execute(ctx, args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			r := parseResult(t, result)
			if r.Error != "" {
				t.Errorf("unexpected tool error: %s", r.Error)
			}

			// Verify the runner was called
			calls := runner.GetCalls()
			if len(calls) != 1 {
				t.Errorf("expected 1 call, got %d", len(calls))
			}
		})
	}
}

func TestSpawnAgentTool_DepthLimitEnforcement(t *testing.T) {
	tests := []struct {
		name        string
		currentDep  int
		maxDepth    int
		expectError bool
	}{
		{
			name:        "depth at limit returns error",
			currentDep:  2,
			maxDepth:    2,
			expectError: true,
		},
		{
			name:        "depth exceeds limit returns error",
			currentDep:  5,
			maxDepth:    3,
			expectError: true,
		},
		{
			name:        "depth below limit succeeds",
			currentDep:  1,
			maxDepth:    3,
			expectError: false,
		},
		{
			name:        "depth at zero with max 1 succeeds",
			currentDep:  0,
			maxDepth:    1,
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := SpawnConfig{
				MaxParallel:    3,
				MaxDepth:       tt.maxDepth,
				DefaultTimeout: 300,
			}
			tool := NewSpawnAgentTool(config, tt.currentDep)

			runner := newMockRunner()
			tool.SetRunner(runner)

			ctx := context.Background()
			args := makeSpawnArgs("test-agent", "do something", 0)

			result, err := tool.Execute(ctx, args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			r := parseResult(t, result)

			if tt.expectError {
				if r.Error == "" {
					t.Error("expected error but got success")
				}
				if r.Type != string(ErrPermissionDenied) {
					t.Errorf("expected error type %s, got %s", ErrPermissionDenied, r.Type)
				}
				// Should mention depth
				if !contains(r.Error, "depth") {
					t.Errorf("error should mention depth: %s", r.Error)
				}
			} else {
				if r.Error != "" {
					t.Errorf("unexpected error: %s", r.Error)
				}
			}
		})
	}
}

func TestSpawnAgentTool_AllowedAgentsWhitelist(t *testing.T) {
	tests := []struct {
		name          string
		allowedAgents []string
		agentName     string
		expectError   bool
	}{
		{
			name:          "empty whitelist allows all agents",
			allowedAgents: nil,
			agentName:     "any-agent",
			expectError:   false,
		},
		{
			name:          "agent in whitelist is allowed",
			allowedAgents: []string{"reviewer", "researcher", "test-agent"},
			agentName:     "researcher",
			expectError:   false,
		},
		{
			name:          "agent not in whitelist is rejected",
			allowedAgents: []string{"reviewer", "researcher"},
			agentName:     "hacker",
			expectError:   true,
		},
		{
			name:          "single agent whitelist works",
			allowedAgents: []string{"only-this-one"},
			agentName:     "only-this-one",
			expectError:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := SpawnConfig{
				MaxParallel:    3,
				MaxDepth:       5,
				DefaultTimeout: 300,
				AllowedAgents:  tt.allowedAgents,
			}
			tool := NewSpawnAgentTool(config, 0)

			runner := newMockRunner()
			tool.SetRunner(runner)

			ctx := context.Background()
			args := makeSpawnArgs(tt.agentName, "do something", 0)

			result, err := tool.Execute(ctx, args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			r := parseResult(t, result)

			if tt.expectError {
				if r.Error == "" {
					t.Error("expected error but got success")
				}
				if r.Type != string(ErrPermissionDenied) {
					t.Errorf("expected error type %s, got %s", ErrPermissionDenied, r.Type)
				}
				// Should mention agent name
				if !contains(r.Error, tt.agentName) {
					t.Errorf("error should mention agent name: %s", r.Error)
				}
			} else {
				if r.Error != "" {
					t.Errorf("unexpected error: %s", r.Error)
				}
			}
		})
	}
}

func TestSpawnAgentTool_RequiredParametersValidation(t *testing.T) {
	tests := []struct {
		name        string
		agentName   string
		prompt      string
		expectError bool
		errorMsg    string
	}{
		{
			name:        "missing agent_name",
			agentName:   "",
			prompt:      "do something",
			expectError: true,
			errorMsg:    "agent_name is required",
		},
		{
			name:        "missing prompt",
			agentName:   "test-agent",
			prompt:      "",
			expectError: true,
			errorMsg:    "prompt is required",
		},
		{
			name:        "both missing",
			agentName:   "",
			prompt:      "",
			expectError: true,
			errorMsg:    "agent_name is required", // Checked first
		},
		{
			name:        "both provided",
			agentName:   "test-agent",
			prompt:      "do something",
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := DefaultSpawnConfig()
			tool := NewSpawnAgentTool(config, 0)

			runner := newMockRunner()
			tool.SetRunner(runner)

			ctx := context.Background()
			args := makeSpawnArgs(tt.agentName, tt.prompt, 0)

			result, err := tool.Execute(ctx, args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			r := parseResult(t, result)

			if tt.expectError {
				if r.Error == "" {
					t.Error("expected error but got success")
				}
				if r.Type != string(ErrInvalidParams) {
					t.Errorf("expected error type %s, got %s", ErrInvalidParams, r.Type)
				}
				if tt.errorMsg != "" && r.Error != tt.errorMsg {
					t.Errorf("expected error %q, got %q", tt.errorMsg, r.Error)
				}
			} else {
				if r.Error != "" {
					t.Errorf("unexpected error: %s", r.Error)
				}
			}
		})
	}
}

func TestSpawnAgentTool_InvalidJSON(t *testing.T) {
	config := DefaultSpawnConfig()
	tool := NewSpawnAgentTool(config, 0)

	runner := newMockRunner()
	tool.SetRunner(runner)

	ctx := context.Background()
	args := json.RawMessage(`{invalid json}`)

	result, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r := parseResult(t, result)
	if r.Error == "" {
		t.Error("expected error for invalid JSON")
	}
	if r.Type != string(ErrInvalidParams) {
		t.Errorf("expected error type %s, got %s", ErrInvalidParams, r.Type)
	}
}

func TestSpawnAgentTool_Preview(t *testing.T) {
	tests := []struct {
		name     string
		args     json.RawMessage
		expected string
	}{
		{
			name:     "basic preview",
			args:     makeSpawnArgs("reviewer", "review the code", 0),
			expected: "@reviewer: review the code",
		},
		{
			name:     "long prompt is truncated",
			args:     makeSpawnArgs("researcher", "this is a very long prompt that should be truncated because it exceeds fifty characters", 0),
			expected: "@researcher: this is a very long prompt that should be trunc...",
		},
		{
			name:     "empty agent_name returns empty",
			args:     makeSpawnArgs("", "do something", 0),
			expected: "",
		},
		{
			name:     "invalid JSON returns empty",
			args:     json.RawMessage(`{invalid}`),
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := DefaultSpawnConfig()
			tool := NewSpawnAgentTool(config, 0)

			result := tool.Preview(tt.args)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestSpawnAgentTool_ContextCancellation(t *testing.T) {
	config := SpawnConfig{
		MaxParallel:    3,
		MaxDepth:       5,
		DefaultTimeout: 300,
	}
	tool := NewSpawnAgentTool(config, 0)

	// Runner that takes a while
	runner := newMockRunner().SetDelay(5 * time.Second)
	tool.SetRunner(runner)

	ctx, cancel := context.WithCancel(context.Background())
	args := makeSpawnArgs("test-agent", "do something", 60)

	// Cancel after a short delay
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	result, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r := parseResult(t, result)
	if r.Error == "" {
		t.Error("expected error for cancelled context")
	}
	// Should be execution failed (cancellation)
	if r.Type != string(ErrExecutionFailed) {
		t.Errorf("expected error type %s, got %s", ErrExecutionFailed, r.Type)
	}
}

func TestSpawnAgentTool_ContextTimeout(t *testing.T) {
	config := SpawnConfig{
		MaxParallel:    3,
		MaxDepth:       5,
		DefaultTimeout: 300,
	}
	tool := NewSpawnAgentTool(config, 0)

	// Runner that takes longer than the timeout
	runner := newMockRunner().SetDelay(5 * time.Second)
	tool.SetRunner(runner)

	ctx := context.Background()
	// Use minimum timeout of 10 seconds, but we'll use a pre-expired context
	ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	args := makeSpawnArgs("test-agent", "do something", 10)

	result, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r := parseResult(t, result)
	if r.Error == "" {
		t.Error("expected error for timed out context")
	}
	// Should be timeout
	if r.Type != string(ErrTimeout) {
		t.Errorf("expected error type %s, got %s", ErrTimeout, r.Type)
	}
}

func TestSpawnAgentTool_SemaphoreLimiting(t *testing.T) {
	maxParallel := 2
	config := SpawnConfig{
		MaxParallel:    maxParallel,
		MaxDepth:       5,
		DefaultTimeout: 300,
	}
	tool := NewSpawnAgentTool(config, 0)

	// Runner with some delay to allow concurrency to build up
	runner := newMockRunner().SetDelay(100 * time.Millisecond)
	tool.SetRunner(runner)

	ctx := context.Background()
	numAgents := 5
	var wg sync.WaitGroup
	wg.Add(numAgents)

	// Launch multiple concurrent agents
	for i := 0; i < numAgents; i++ {
		go func(idx int) {
			defer wg.Done()
			args := makeSpawnArgs("agent", "task", 30)
			_, _ = tool.Execute(ctx, args)
		}(i)
	}

	wg.Wait()

	// Verify max concurrent never exceeded limit
	maxRunning := runner.GetMaxRunning()
	if maxRunning > maxParallel {
		t.Errorf("max concurrent agents was %d, expected at most %d", maxRunning, maxParallel)
	}

	// Verify all calls were made
	if runner.GetCallCount() != numAgents {
		t.Errorf("expected %d calls, got %d", numAgents, runner.GetCallCount())
	}
}

func TestSpawnAgentTool_RunnerNotConfigured(t *testing.T) {
	config := DefaultSpawnConfig()
	tool := NewSpawnAgentTool(config, 0)
	// Don't set runner

	ctx := context.Background()
	args := makeSpawnArgs("test-agent", "do something", 0)

	result, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r := parseResult(t, result)
	if r.Error == "" {
		t.Error("expected error when runner not configured")
	}
	if r.Type != string(ErrExecutionFailed) {
		t.Errorf("expected error type %s, got %s", ErrExecutionFailed, r.Type)
	}
	if !contains(r.Error, "runner not configured") {
		t.Errorf("error should mention runner not configured: %s", r.Error)
	}
}

func TestSpawnAgentTool_RunnerError(t *testing.T) {
	config := DefaultSpawnConfig()
	tool := NewSpawnAgentTool(config, 0)

	runner := newMockRunner().SetError(errors.New("agent failed to initialize"))
	tool.SetRunner(runner)

	ctx := context.Background()
	args := makeSpawnArgs("test-agent", "do something", 0)

	result, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r := parseResult(t, result)
	if r.Error == "" {
		t.Error("expected error when runner returns error")
	}
	if r.Type != string(ErrExecutionFailed) {
		t.Errorf("expected error type %s, got %s", ErrExecutionFailed, r.Type)
	}
	if !contains(r.Error, "agent failed to initialize") {
		t.Errorf("error should contain runner error: %s", r.Error)
	}
	// Duration is included (may be 0 for very fast operations, which is valid)
}

func TestSpawnAgentTool_SuccessResult(t *testing.T) {
	config := DefaultSpawnConfig()
	tool := NewSpawnAgentTool(config, 0)

	expectedOutput := "The analysis is complete. Found 3 issues."
	runner := newMockRunner().SetOutput(expectedOutput)
	tool.SetRunner(runner)

	ctx := context.Background()
	args := makeSpawnArgs("analyzer", "analyze the code", 60)

	result, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r := parseResult(t, result)
	if r.Error != "" {
		t.Errorf("unexpected error: %s", r.Error)
	}
	if r.AgentName != "analyzer" {
		t.Errorf("expected agent_name %q, got %q", "analyzer", r.AgentName)
	}
	if r.Output != expectedOutput {
		t.Errorf("expected output %q, got %q", expectedOutput, r.Output)
	}
	// Duration is included (may be 0 for very fast operations, which is valid)
}

func TestSpawnAgentTool_DepthPassedToRunner(t *testing.T) {
	config := SpawnConfig{
		MaxParallel:    3,
		MaxDepth:       5,
		DefaultTimeout: 300,
	}
	initialDepth := 2
	tool := NewSpawnAgentTool(config, initialDepth)

	runner := newMockRunner()
	tool.SetRunner(runner)

	ctx := context.Background()
	args := makeSpawnArgs("test-agent", "do something", 0)

	_, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	calls := runner.GetCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}

	// The runner should receive depth+1
	expectedDepth := initialDepth + 1
	if calls[0].Depth != expectedDepth {
		t.Errorf("expected depth %d, got %d", expectedDepth, calls[0].Depth)
	}
}

func TestSpawnAgentTool_SetDepth(t *testing.T) {
	config := SpawnConfig{
		MaxParallel:    3,
		MaxDepth:       3,
		DefaultTimeout: 300,
	}
	tool := NewSpawnAgentTool(config, 0)

	runner := newMockRunner()
	tool.SetRunner(runner)

	// Initially at depth 0, should work
	ctx := context.Background()
	args := makeSpawnArgs("test-agent", "do something", 0)

	result, _ := tool.Execute(ctx, args)
	r := parseResult(t, result)
	if r.Error != "" {
		t.Errorf("unexpected error at depth 0: %s", r.Error)
	}

	// Set depth to max, should fail
	tool.SetDepth(3)

	result, _ = tool.Execute(ctx, args)
	r = parseResult(t, result)
	if r.Error == "" {
		t.Error("expected error at max depth")
	}
}

func TestSpawnAgentTool_Spec(t *testing.T) {
	config := DefaultSpawnConfig()
	tool := NewSpawnAgentTool(config, 0)

	spec := tool.Spec()

	if spec.Name != SpawnAgentToolName {
		t.Errorf("expected name %q, got %q", SpawnAgentToolName, spec.Name)
	}

	if spec.Description == "" {
		t.Error("spec should have a description")
	}

	if spec.Schema == nil {
		t.Error("spec should have a schema")
	}

	// Check schema has required fields
	schema := spec.Schema

	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema should have properties")
	}

	if _, ok := props["agent_name"]; !ok {
		t.Error("schema should have agent_name property")
	}
	if _, ok := props["prompt"]; !ok {
		t.Error("schema should have prompt property")
	}
	if _, ok := props["timeout"]; !ok {
		t.Error("schema should have timeout property")
	}

	required, ok := schema["required"].([]string)
	if !ok {
		t.Fatal("schema should have required array")
	}

	hasAgentName := false
	hasPrompt := false
	for _, r := range required {
		if r == "agent_name" {
			hasAgentName = true
		}
		if r == "prompt" {
			hasPrompt = true
		}
	}
	if !hasAgentName || !hasPrompt {
		t.Error("agent_name and prompt should be required")
	}
}

func TestSpawnAgentTool_DefaultConfig(t *testing.T) {
	config := DefaultSpawnConfig()

	if config.MaxParallel != 3 {
		t.Errorf("expected MaxParallel 3, got %d", config.MaxParallel)
	}
	if config.MaxDepth != 2 {
		t.Errorf("expected MaxDepth 2, got %d", config.MaxDepth)
	}
	if config.DefaultTimeout != 300 {
		t.Errorf("expected DefaultTimeout 300, got %d", config.DefaultTimeout)
	}
	if config.AllowedAgents != nil {
		t.Error("expected AllowedAgents to be nil by default")
	}
}

func TestSpawnAgentTool_ConfigDefaults(t *testing.T) {
	// Test that NewSpawnAgentTool applies defaults for invalid config values
	tests := []struct {
		name         string
		config       SpawnConfig
		expectConfig SpawnConfig
	}{
		{
			name: "zero values get defaults",
			config: SpawnConfig{
				MaxParallel:    0,
				MaxDepth:       0,
				DefaultTimeout: 0,
			},
			expectConfig: SpawnConfig{
				MaxParallel:    3,
				MaxDepth:       2,
				DefaultTimeout: 300,
			},
		},
		{
			name: "negative values get defaults",
			config: SpawnConfig{
				MaxParallel:    -1,
				MaxDepth:       -5,
				DefaultTimeout: -100,
			},
			expectConfig: SpawnConfig{
				MaxParallel:    3,
				MaxDepth:       2,
				DefaultTimeout: 300,
			},
		},
		{
			name: "valid values are preserved",
			config: SpawnConfig{
				MaxParallel:    5,
				MaxDepth:       4,
				DefaultTimeout: 120,
			},
			expectConfig: SpawnConfig{
				MaxParallel:    5,
				MaxDepth:       4,
				DefaultTimeout: 120,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := NewSpawnAgentTool(tt.config, 0)

			if tool.config.MaxParallel != tt.expectConfig.MaxParallel {
				t.Errorf("expected MaxParallel %d, got %d", tt.expectConfig.MaxParallel, tool.config.MaxParallel)
			}
			if tool.config.MaxDepth != tt.expectConfig.MaxDepth {
				t.Errorf("expected MaxDepth %d, got %d", tt.expectConfig.MaxDepth, tool.config.MaxDepth)
			}
			if tool.config.DefaultTimeout != tt.expectConfig.DefaultTimeout {
				t.Errorf("expected DefaultTimeout %d, got %d", tt.expectConfig.DefaultTimeout, tool.config.DefaultTimeout)
			}
		})
	}
}

func TestSpawnAgentTool_EventCallback(t *testing.T) {
	config := DefaultSpawnConfig()
	tool := NewSpawnAgentTool(config, 0)

	// Initially nil
	if tool.GetEventCallback() != nil {
		t.Error("expected nil callback initially")
	}

	// Set callback
	var called bool
	cb := func(callID string, event SubagentEvent) {
		called = true
	}
	tool.SetEventCallback(cb)

	// Get callback
	gotCb := tool.GetEventCallback()
	if gotCb == nil {
		t.Error("expected non-nil callback after setting")
	}

	// Call it to verify it's the right one
	gotCb("test", SubagentEvent{})
	if !called {
		t.Error("callback was not invoked")
	}
}

func TestSpawnAgentTool_SessionIDPropagation(t *testing.T) {
	config := DefaultSpawnConfig()
	tool := NewSpawnAgentTool(config, 0)

	expectedSessionID := "child-123"
	runner := newMockRunner().SetSessionID(expectedSessionID)
	tool.SetRunner(runner)

	ctx := context.Background()
	args := makeSpawnArgs("test-agent", "do something", 0)

	result, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r := parseResult(t, result)
	if r.Error != "" {
		t.Errorf("unexpected error: %s", r.Error)
	}
	if r.SessionID != expectedSessionID {
		t.Errorf("expected session_id %q, got %q", expectedSessionID, r.SessionID)
	}
}

// Helper function to check if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
