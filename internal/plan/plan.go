// Package plan owns the shared update_plan snapshot model, validation, history
// extraction, and bounded rendering helpers.
package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

const (
	// ToolName is the model-facing name of the execution plan snapshot tool.
	ToolName = "update_plan"

	MaxSteps            = 20
	MaxStepRunes        = 240
	MaxExplanationRunes = 500
)

// StepStatus is the lifecycle state of one execution-plan step.
type StepStatus string

const (
	StatusPending    StepStatus = "pending"
	StatusInProgress StepStatus = "in_progress"
	StatusCompleted  StepStatus = "completed"
)

// Step is one ordered item in an execution plan.
type Step struct {
	Step   string     `json:"step"`
	Status StepStatus `json:"status"`
}

// Snapshot is the complete model-controlled update_plan state. Every tool call
// replaces the previous snapshot atomically.
type Snapshot struct {
	Explanation string `json:"explanation,omitempty"`
	Plan        []Step `json:"plan"`
}

// SnapshotSummary is compact rendering/status data derived from a snapshot.
type SnapshotSummary struct {
	Completed   int
	Total       int
	CurrentStep string
}

type snapshotWire struct {
	Explanation json.RawMessage `json:"explanation,omitempty"`
	Plan        *[]Step         `json:"plan"`
}

// Parse decodes, normalizes, and validates a complete update_plan snapshot.
func Parse(raw json.RawMessage) (Snapshot, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var wire snapshotWire
	if err := decoder.Decode(&wire); err != nil {
		return Snapshot{}, fmt.Errorf("parse plan arguments: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Snapshot{}, err
	}
	if wire.Plan == nil {
		return Snapshot{}, fmt.Errorf("plan is required")
	}
	explanation := ""
	if len(wire.Explanation) > 0 {
		if bytes.Equal(bytes.TrimSpace(wire.Explanation), []byte("null")) {
			return Snapshot{}, fmt.Errorf("explanation must be a string")
		}
		if err := json.Unmarshal(wire.Explanation, &explanation); err != nil {
			return Snapshot{}, fmt.Errorf("explanation must be a string: %w", err)
		}
	}
	snapshot := Snapshot{
		Explanation: explanation,
		Plan:        append([]Step(nil), (*wire.Plan)...),
	}
	if snapshot.Plan == nil {
		snapshot.Plan = []Step{}
	}
	if err := snapshot.NormalizeAndValidate(); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("parse plan arguments: %w", err)
	}
	return fmt.Errorf("parse plan arguments: multiple JSON values")
}

// NormalizeAndValidate trims model text and enforces all update_plan invariants.
func (s *Snapshot) NormalizeAndValidate() error {
	if s == nil {
		return fmt.Errorf("plan snapshot is required")
	}
	s.Explanation = strings.TrimSpace(s.Explanation)
	if utf8.RuneCountInString(s.Explanation) > MaxExplanationRunes {
		return fmt.Errorf("explanation must be at most %d Unicode code points", MaxExplanationRunes)
	}
	if len(s.Plan) > MaxSteps {
		return fmt.Errorf("plan must contain at most %d steps", MaxSteps)
	}
	if s.Plan == nil {
		s.Plan = []Step{}
	}
	seen := make(map[string]struct{}, len(s.Plan))
	inProgress := 0
	for i := range s.Plan {
		step := &s.Plan[i]
		step.Step = strings.TrimSpace(step.Step)
		if step.Step == "" {
			return fmt.Errorf("step %d is required", i+1)
		}
		if utf8.RuneCountInString(step.Step) > MaxStepRunes {
			return fmt.Errorf("step %d must be at most %d Unicode code points", i+1, MaxStepRunes)
		}
		switch step.Status {
		case StatusPending, StatusCompleted:
		case StatusInProgress:
			inProgress++
		default:
			return fmt.Errorf("step %d has invalid status %q", i+1, step.Status)
		}
		normalized := normalizeStepText(step.Step)
		if _, exists := seen[normalized]; exists {
			return fmt.Errorf("plan contains duplicates after step-text normalization")
		}
		seen[normalized] = struct{}{}
	}
	if inProgress > 1 {
		return fmt.Errorf("plan may contain at most one in_progress step")
	}
	return nil
}

func normalizeStepText(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(text)), " "))
}

// CanonicalJSON returns a stable JSON representation of a valid snapshot.
func (s Snapshot) CanonicalJSON() ([]byte, error) {
	copy := Snapshot{Explanation: s.Explanation, Plan: append([]Step(nil), s.Plan...)}
	if err := copy.NormalizeAndValidate(); err != nil {
		return nil, err
	}
	return json.Marshal(copy)
}

// Equal reports whether two normalized snapshots have the same canonical data.
func (s Snapshot) Equal(other Snapshot) bool {
	left, leftErr := s.CanonicalJSON()
	right, rightErr := other.CanonicalJSON()
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

// IsActive reports whether a non-empty snapshot still has unfinished work.
func (s Snapshot) IsActive() bool {
	if len(s.Plan) == 0 {
		return false
	}
	for _, step := range s.Plan {
		if step.Status != StatusCompleted {
			return true
		}
	}
	return false
}

// Summary derives completed/total/current-step status without mutating the plan.
func (s Snapshot) Summary() SnapshotSummary {
	summary := SnapshotSummary{Total: len(s.Plan)}
	for _, step := range s.Plan {
		if step.Status == StatusCompleted {
			summary.Completed++
		}
		if summary.CurrentStep == "" && step.Status == StatusInProgress {
			summary.CurrentStep = step.Step
		}
	}
	return summary
}

// ContextMessage formats an active snapshot as bounded developer context. The
// validation limits bound the output to roughly 5.5K Unicode code points.
func (s Snapshot) ContextMessage() string {
	copy := Snapshot{Explanation: s.Explanation, Plan: append([]Step(nil), s.Plan...)}
	if err := copy.NormalizeAndValidate(); err != nil || !copy.IsActive() {
		return ""
	}
	var b strings.Builder
	b.WriteString("<current_execution_plan>\n")
	if copy.Explanation != "" {
		b.WriteString("Explanation: ")
		b.WriteString(copy.Explanation)
		b.WriteByte('\n')
	}
	for _, step := range copy.Plan {
		fmt.Fprintf(&b, "- [%s] %s\n", step.Status, step.Step)
	}
	b.WriteString("</current_execution_plan>\n\n")
	b.WriteString("This is restored execution state, not a new user request. Keep it current with update_plan when meaningful progress or the approach changes.")
	return b.String()
}

// ChecklistText formats a valid snapshot for transcript and terminal rendering.
func (s Snapshot) ChecklistText(running bool) string {
	copy := Snapshot{Explanation: s.Explanation, Plan: append([]Step(nil), s.Plan...)}
	if err := copy.NormalizeAndValidate(); err != nil {
		return ""
	}
	if len(copy.Plan) == 0 {
		if running {
			return "Clearing plan…"
		}
		return "Plan cleared"
	}
	title := "Plan updated"
	if running {
		title = "Updating plan…"
	}
	if copy.Explanation != "" {
		title += " — " + copy.Explanation
	}
	var b strings.Builder
	b.WriteString(title)
	for i, step := range copy.Plan {
		marker := "○"
		switch step.Status {
		case StatusCompleted:
			marker = "✓"
		case StatusInProgress:
			marker = "→"
		}
		if i == 0 {
			b.WriteString("\n\n")
		} else {
			b.WriteByte('\n')
		}
		b.WriteString(marker)
		b.WriteByte(' ')
		b.WriteString(step.Step)
	}
	return b.String()
}

// LatestSuccessfulSnapshot extracts the newest valid update_plan call with a
// matching successful tool result from active message history.
func LatestSuccessfulSnapshot(messages []core.Message) (Snapshot, bool) {
	successful := make(map[string]bool)
	for _, message := range messages {
		for _, part := range message.Parts {
			if part.Type != core.PartToolResult || part.ToolResult == nil {
				continue
			}
			result := part.ToolResult
			if result.ID != "" && !result.IsError && (result.Name == "" || result.Name == ToolName) {
				successful[result.ID] = true
			}
		}
	}
	for i := len(messages) - 1; i >= 0; i-- {
		parts := messages[i].Parts
		for j := len(parts) - 1; j >= 0; j-- {
			call := parts[j].ToolCall
			if parts[j].Type != core.PartToolCall || call == nil || call.Name != ToolName || !successful[call.ID] {
				continue
			}
			snapshot, err := Parse(call.Arguments)
			if err == nil {
				return snapshot, true
			}
		}
	}
	return Snapshot{}, false
}
