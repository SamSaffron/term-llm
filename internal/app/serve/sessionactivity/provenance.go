package sessionactivity

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/term-llm/term-llm/internal/formatutil"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/tools"
)

type jobReference struct {
	JobID string
	RunID string
	Agent string
	Title string
}

func validActivityID(id string) bool {
	if len(id) == 0 || len(id) > 160 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// Only machine-stored successful results paired to their calls, and trusted
// notification markers, establish membership. Prompts, tool arguments, shell
// commands and provider-visible notification text never establish membership.
//
// A queue_agent result or a structured job notification establishes the job. A
// wait_for_jobs result that names its run establishes that exact run only, so
// sibling and future executions of the job stay out of scope. A legacy wait
// result without a run id establishes nothing: it can neither add a job nor
// widen an exact reference, because the queue_agent result or notification that
// started that job already carries its own scope.
func jobReferences(messages []session.Message) []jobReference {
	calls := make(map[string]string)
	labels := make(map[string]string)
	for _, message := range messages {
		for _, part := range message.Parts {
			if message.Role == core.RoleAssistant && part.ToolCall != nil {
				name := part.ToolCall.Name
				if name == tools.QueueAgentToolName || name == tools.WaitForJobsToolName {
					calls[part.ToolCall.ID] = name
					labels[part.ToolCall.ID] = queuedTaskLabel(part.ToolCall.Arguments)
				}
			}
		}
	}
	refs := make(map[string]jobReference)
	add := func(ref jobReference) {
		if !validActivityID(ref.JobID) {
			return
		}
		if ref.RunID != "" && !validActivityID(ref.RunID) {
			return
		}
		key := "job:" + ref.JobID
		if ref.RunID != "" {
			key = "run:" + ref.RunID
		}
		if ref.Title == "" {
			ref.Title = refs[key].Title
		}
		refs[key] = ref
	}
	for _, message := range messages {
		for _, part := range message.Parts {
			if part.Type == core.PartJobNotification && part.JobNotification != nil {
				info := part.JobNotification
				add(jobReference{JobID: info.JobID, RunID: info.RunID, Agent: info.Agent})
				continue
			}
			result := part.ToolResult
			if result == nil || result.IsError || message.Role != core.RoleTool {
				continue
			}
			name := calls[result.ID]
			if result.Name != "" && result.Name != name {
				continue
			}
			switch name {
			case tools.QueueAgentToolName:
				var value tools.QueueAgentResult
				if json.Unmarshal([]byte(result.Content), &value) == nil {
					add(jobReference{JobID: value.JobID, RunID: value.RunID, Agent: value.AgentName, Title: labels[result.ID]})
				}
			case tools.WaitForJobsToolName:
				var values []tools.QueuedJobResult
				if json.Unmarshal([]byte(result.Content), &values) == nil {
					for _, value := range values {
						if value.RunID == "" {
							continue // Legacy run-less wait: never membership evidence.
						}
						add(jobReference{JobID: value.JobID, RunID: value.RunID})
					}
				}
			}
		}
	}
	out := make([]jobReference, 0, len(refs))
	for _, ref := range refs {
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.Compare(out[i].JobID+":"+out[i].RunID, out[j].JobID+":"+out[j].RunID) < 0
	})
	return out
}

// An explicit short label is presentation metadata, never membership evidence.
// Do not fall back to prompt/instructions when a call has no suitable label.
func queuedTaskLabel(arguments json.RawMessage) string {
	var args struct {
		Label string `json:"label"`
	}
	if json.Unmarshal(arguments, &args) != nil {
		return ""
	}
	return formatutil.NormalizeDisplayLabel(args.Label)
}
