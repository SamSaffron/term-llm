package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

const WorkerReportToolName = "report"

// WorkerReportArgs is the structured mailbox envelope exposed to workers.
type WorkerReportArgs struct {
	Kind     session.WorkerReportKind `json:"kind"`
	Title    string                   `json:"title"`
	Body     string                   `json:"body"`
	Metadata json.RawMessage          `json:"metadata,omitempty"`
}

// WorkerReportTool is registered only for durable /thread child runs.
type WorkerReportTool struct {
	store         session.WorkerStore
	childID       string
	coordinatorID string
}

func NewWorkerReportTool(store session.WorkerStore, childID, coordinatorID string) *WorkerReportTool {
	return &WorkerReportTool{store: store, childID: strings.TrimSpace(childID), coordinatorID: strings.TrimSpace(coordinatorID)}
}

func (t *WorkerReportTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        WorkerReportToolName,
		Description: "Send a durable structured mailbox report to the coordinating chat. Use progress for useful milestones, blocker when unable to proceed, and result for the final outcome. Reports stay outside the coordinator's model context until the user explicitly imports one.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"kind":     map[string]any{"type": "string", "enum": []string{"progress", "result", "blocker"}},
				"title":    map[string]any{"type": "string", "maxLength": session.MaxWorkerReportTitleRunes},
				"body":     map[string]any{"type": "string", "maxLength": session.MaxWorkerReportBodyRunes},
				"metadata": map[string]any{"type": "object", "description": "Optional machine-readable provenance or summary fields."},
			},
			"required":             []string{"kind", "title", "body"},
			"additionalProperties": false,
		},
	}
}

func (t *WorkerReportTool) Preview(raw json.RawMessage) string {
	var args WorkerReportArgs
	_ = json.Unmarshal(raw, &args)
	if title := strings.TrimSpace(args.Title); title != "" {
		return fmt.Sprintf("%s: %s", args.Kind, title)
	}
	return "send worker report"
}

func (t *WorkerReportTool) Execute(ctx context.Context, raw json.RawMessage) (llm.ToolOutput, error) {
	if t == nil || t.store == nil || t.childID == "" || t.coordinatorID == "" {
		return llm.TextOutput("Worker mailbox is unavailable."), nil
	}
	var args WorkerReportArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return llm.TextOutput(fmt.Sprintf("Invalid report: %v", err)), nil
	}
	report, err := t.store.AddWorkerReport(ctx, session.WorkerReport{
		ChildSessionID:       t.childID,
		SourceSessionID:      t.childID,
		DestinationSessionID: t.coordinatorID,
		Kind:                 args.Kind,
		Title:                args.Title,
		Body:                 args.Body,
		Metadata:             args.Metadata,
		Origin:               "worker_tool",
	})
	if err != nil {
		return llm.TextOutput(fmt.Sprintf("Report was not saved: %v", err)), nil
	}
	switch args.Kind {
	case session.WorkerReportBlocker:
		_ = t.store.UpdateWorkerStatus(ctx, t.childID, session.WorkerBlocked)
	case session.WorkerReportProgress:
		_ = t.store.UpdateWorkerStatus(ctx, t.childID, session.WorkerRunning)
	}
	return llm.TextOutput(fmt.Sprintf("Report #%d saved to the coordinator mailbox as %s.", report.Sequence+1, report.Kind)), nil
}
