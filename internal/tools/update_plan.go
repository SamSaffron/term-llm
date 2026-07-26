package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/llm"
	planpkg "github.com/samsaffron/term-llm/internal/plan"
	"github.com/samsaffron/term-llm/internal/session"
)

type planControllerState struct {
	snapshot planpkg.Snapshot
	loaded   bool
	present  bool
	modified bool
}

// PlanController owns the lazily loaded current snapshot for configured
// update_plan tools. State is keyed by session because engines used by command
// runners may service more than one non-persisted request over their lifetime.
type PlanController struct {
	mu             sync.Mutex
	store          session.PlanSnapshotStore
	states         map[string]planControllerState
	sessionLocks   map[string]*sync.Mutex
	promptGuidance bool
}

const (
	updatePlanArgumentsExample = `{"explanation":"optional concise reason","plan":[{"step":"Inspect code","status":"in_progress"}]}`
	updatePlanShapeGuidance    = `Expected an object with the required array field "plan" and optional string field "explanation". Use exactly "step" and "status" in each plan item; status must be "pending", "in_progress", or "completed". Example: ` + updatePlanArgumentsExample
	updatePlanPromptGuidance   = `<update_plan_guidance>
Use update_plan for meaningful multi-step, uncertain, or cross-package work. For trivial changes and informational requests, proceed directly. Normally publish 3–7 outcome-oriented steps and update them at meaningful transitions. Keep at most one step in_progress, revise stale plans when the approach changes, complete verification steps only after verification succeeds, and mark every step completed before a successful final response.
Call update_plan with exactly this shape: ` + updatePlanArgumentsExample + `. Set "plan" to an array and use only the optional "explanation" alongside it. In each plan item, use exactly "step" for its text and "status" for its state.
</update_plan_guidance>`
)

// NewPlanController creates a lightweight in-memory plan controller.
func NewPlanController(store session.PlanSnapshotStore) *PlanController {
	return &PlanController{
		store:        store,
		states:       make(map[string]planControllerState),
		sessionLocks: make(map[string]*sync.Mutex),
	}
}

// SetPromptGuidance enables built-in developer guidance. Engine capability
// gating ensures this text is never added when update_plan is unavailable.
func (c *PlanController) SetPromptGuidance(enabled bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.promptGuidance = enabled
	c.mu.Unlock()
}

// SetStore attaches the optional durable latest-snapshot capability.
func (c *PlanController) SetStore(store session.PlanSnapshotStore) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.store = store
	for sessionID, state := range c.states {
		// A state observed before the store was attached may be loaded again, but
		// never discard a successful in-memory update or clear. Registry wiring
		// normally attaches the store before the first request; this protects
		// callers that bind it later.
		if !state.present && !state.modified {
			state.loaded = false
			c.states[sessionID] = state
		}
	}
	c.mu.Unlock()
}

func (c *PlanController) promptGuidanceText() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.promptGuidance {
		return updatePlanPromptGuidance
	}
	return ""
}

func (c *PlanController) sessionLock(sessionID string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	lock := c.sessionLocks[sessionID]
	if lock == nil {
		lock = &sync.Mutex{}
		c.sessionLocks[sessionID] = lock
	}
	return lock
}

func (c *PlanController) load(ctx context.Context, sessionID string) (planControllerState, error) {
	if c == nil {
		return planControllerState{}, nil
	}
	lock := c.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()

	c.mu.Lock()
	state := c.states[sessionID]
	store := c.store
	c.mu.Unlock()
	if state.loaded {
		return state, nil
	}
	state.loaded = true
	if store != nil && strings.TrimSpace(sessionID) != "" {
		snapshot, version, err := store.LoadPlanSnapshot(ctx, sessionID)
		if err != nil {
			return planControllerState{}, err
		}
		if version > 0 {
			if err := snapshot.NormalizeAndValidate(); err != nil {
				return planControllerState{}, fmt.Errorf("validate stored plan snapshot: %w", err)
			}
			state.snapshot = snapshot
			state.present = true
		}
	}
	c.mu.Lock()
	c.states[sessionID] = state
	c.mu.Unlock()
	return state, nil
}

func (c *PlanController) update(ctx context.Context, sessionID string, snapshot planpkg.Snapshot) error {
	if c == nil {
		return fmt.Errorf("plan controller is unavailable")
	}
	if err := snapshot.NormalizeAndValidate(); err != nil {
		return err
	}
	lock := c.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()

	c.mu.Lock()
	store := c.store
	c.mu.Unlock()
	if len(snapshot.Plan) == 0 {
		if store != nil && strings.TrimSpace(sessionID) != "" {
			if err := store.DeletePlanSnapshot(ctx, sessionID); err != nil {
				return fmt.Errorf("persist plan clear: %w", err)
			}
		}
		c.mu.Lock()
		c.states[sessionID] = planControllerState{loaded: true, modified: true}
		c.mu.Unlock()
		return nil
	}

	if store != nil && strings.TrimSpace(sessionID) != "" {
		if _, err := store.SavePlanSnapshot(ctx, sessionID, snapshot); err != nil {
			return fmt.Errorf("persist plan: %w", err)
		}
	}
	c.mu.Lock()
	c.states[sessionID] = planControllerState{snapshot: snapshot, loaded: true, present: true, modified: true}
	c.mu.Unlock()
	return nil
}

func (c *PlanController) adoptHistory(ctx context.Context, sessionID string, snapshot planpkg.Snapshot) error {
	lock := c.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()

	c.mu.Lock()
	state := c.states[sessionID]
	store := c.store
	c.mu.Unlock()
	present := len(snapshot.Plan) > 0
	if state.loaded && state.present == present && (!present || state.snapshot.Equal(snapshot)) {
		state.modified = true
		c.mu.Lock()
		c.states[sessionID] = state
		c.mu.Unlock()
		return nil
	}
	if store != nil && strings.TrimSpace(sessionID) != "" {
		if present {
			if _, err := store.SavePlanSnapshot(ctx, sessionID, snapshot); err != nil {
				return fmt.Errorf("reconcile plan snapshot from history: %w", err)
			}
		} else if err := store.DeletePlanSnapshot(ctx, sessionID); err != nil {
			return fmt.Errorf("reconcile plan clear from history: %w", err)
		}
	}
	state.snapshot = snapshot
	state.present = present
	state.loaded = true
	state.modified = true
	c.mu.Lock()
	c.states[sessionID] = state
	c.mu.Unlock()
	return nil
}

func (c *PlanController) clearTransientState(sessionID string) {
	lock := c.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()
	c.mu.Lock()
	delete(c.states, sessionID)
	c.mu.Unlock()
}

func (c *PlanController) prepareRequestContext(ctx context.Context, sessionID string, messages []llm.Message) ([]llm.Message, error) {
	historySnapshot, historyRepresented := planpkg.LatestSuccessfulSnapshot(messages)
	if strings.TrimSpace(sessionID) == "" && !historyRepresented && !containsPlanContextMarker(messages) {
		// Blank session IDs identify one-shot runs rather than resumable sessions.
		// A fresh transcript must not inherit state from an earlier run that reused
		// the same engine and tool registry.
		c.clearTransientState(sessionID)
	}
	state, err := c.load(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if historyRepresented {
		if err := c.adoptHistory(ctx, sessionID, historySnapshot); err != nil {
			return nil, err
		}
	}

	var contextParts []string
	if !historyRepresented && state.present && state.snapshot.IsActive() {
		if text := state.snapshot.ContextMessage(); text != "" && !containsPlanContext(messages, text) {
			contextParts = append(contextParts, text)
		}
	}
	if guidance := c.promptGuidanceText(); guidance != "" && !containsPromptGuidance(messages) {
		contextParts = append(contextParts, guidance)
	}
	if len(contextParts) == 0 {
		return messages, nil
	}
	return insertPlanContextBeforeLatestUser(messages, planContextMessage(strings.Join(contextParts, "\n\n"))), nil
}

func (c *PlanController) prepareCompactionContext(ctx context.Context, sessionID string, result *llm.CompactionResult) error {
	if result == nil {
		return nil
	}
	state, err := c.load(ctx, sessionID)
	if err != nil {
		return err
	}
	historyRepresented := false
	if historySnapshot, ok := planpkg.LatestSuccessfulSnapshot(result.NewMessages); ok {
		if err := c.adoptHistory(ctx, sessionID, historySnapshot); err != nil {
			return err
		}
		historyRepresented = true
	}

	var contextParts []string
	activeMessages := result.ActiveMessages()
	if !historyRepresented && state.present && state.snapshot.IsActive() {
		if text := state.snapshot.ContextMessage(); text != "" && !containsPlanContext(activeMessages, text) {
			contextParts = append(contextParts, text)
		}
	}
	if guidance := c.promptGuidanceText(); guidance != "" && !containsPromptGuidance(activeMessages) {
		contextParts = append(contextParts, guidance)
	}
	if len(contextParts) > 0 {
		result.EphemeralMessages = append(result.EphemeralMessages, planContextMessage(strings.Join(contextParts, "\n\n")))
	}
	return nil
}

func containsPlanContextMarker(messages []llm.Message) bool {
	for _, message := range messages {
		if message.Role == llm.RoleDeveloper && strings.Contains(llm.MessageText(message), "<current_execution_plan>") {
			return true
		}
	}
	return false
}

func containsPlanContext(messages []llm.Message, text string) bool {
	for _, message := range messages {
		if message.Role != llm.RoleDeveloper {
			continue
		}
		if strings.Contains(strings.TrimSpace(llm.MessageText(message)), strings.TrimSpace(text)) {
			return true
		}
	}
	return false
}

func containsPromptGuidance(messages []llm.Message) bool {
	for _, message := range messages {
		if message.Role == llm.RoleDeveloper && strings.Contains(llm.MessageText(message), "<update_plan_guidance>") {
			return true
		}
	}
	return false
}

func insertPlanContextBeforeLatestUser(messages []llm.Message, contextMessage llm.Message) []llm.Message {
	insertAt := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == llm.RoleUser {
			insertAt = i
			break
		}
	}
	out := make([]llm.Message, 0, len(messages)+1)
	out = append(out, messages[:insertAt]...)
	out = append(out, contextMessage)
	out = append(out, messages[insertAt:]...)
	return out
}

func planContextMessage(text string) llm.Message {
	return llm.Message{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: text}}}
}

// UpdatePlanTool atomically replaces a session's current execution-plan snapshot.
type UpdatePlanTool struct {
	controller *PlanController
}

// NewUpdatePlanTool constructs update_plan with an injected controller.
func NewUpdatePlanTool(controller *PlanController) *UpdatePlanTool {
	if controller == nil {
		controller = NewPlanController(nil)
	}
	return &UpdatePlanTool{controller: controller}
}

func (t *UpdatePlanTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: UpdatePlanToolName,
		Description: "Publish the complete current execution plan. Each call replaces the previous ordered checklist; pass an empty plan array to clear it. " +
			`Use exactly {"plan":[{"step":"...","status":"pending|in_progress|completed"}],"explanation":"optional"}. ` +
			`At the top level, use the required "plan" array and optional "explanation" string. In each item, use exactly "step" and "status".`,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"explanation": map[string]any{
					"type":        "string",
					"maxLength":   planpkg.MaxExplanationRunes,
					"description": "Optional concise reason for this plan update.",
				},
				"plan": map[string]any{
					"type":        "array",
					"maxItems":    planpkg.MaxSteps,
					"description": "The complete ordered execution plan. An empty array clears current plan state.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"step": map[string]any{
								"type":      "string",
								"minLength": 1,
								"maxLength": planpkg.MaxStepRunes,
							},
							"status": map[string]any{
								"type": "string",
								"enum": []string{string(planpkg.StatusPending), string(planpkg.StatusInProgress), string(planpkg.StatusCompleted)},
							},
						},
						"required":             []string{"step", "status"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"plan"},
			"additionalProperties": false,
		},
	}
}

func (t *UpdatePlanTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	snapshot, err := planpkg.Parse(args)
	if err != nil {
		return llm.ToolOutput{}, NewToolErrorf(ErrInvalidParams, "invalid update_plan arguments: %v. %s", err, updatePlanShapeGuidance)
	}
	if err := t.controller.update(ctx, llm.SessionIDFromContext(ctx), snapshot); err != nil {
		return llm.ToolOutput{}, NewToolErrorf(ErrExecutionFailed, "%v", err)
	}
	if len(snapshot.Plan) == 0 {
		return llm.TextOutput("Plan cleared"), nil
	}
	return llm.TextOutput("Plan updated"), nil
}

func (t *UpdatePlanTool) Preview(args json.RawMessage) string {
	snapshot, err := planpkg.Parse(args)
	if err != nil {
		return "(update plan)"
	}
	if len(snapshot.Plan) == 0 {
		return "(clear plan)"
	}
	label := "steps"
	if len(snapshot.Plan) == 1 {
		label = "step"
	}
	return fmt.Sprintf("(update plan: %d %s)", len(snapshot.Plan), label)
}

// PrepareRequestContext implements llm.RequestContextTool.
func (t *UpdatePlanTool) PrepareRequestContext(ctx context.Context, sessionID string, messages []llm.Message) ([]llm.Message, error) {
	return t.controller.prepareRequestContext(ctx, sessionID, messages)
}

// PrepareCompactionContext implements llm.RequestContextTool.
func (t *UpdatePlanTool) PrepareCompactionContext(ctx context.Context, sessionID string, result *llm.CompactionResult) error {
	return t.controller.prepareCompactionContext(ctx, sessionID, result)
}
