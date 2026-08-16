// Package subagentview reconstructs immutable completed subagent presentation
// from the durable parent spawn_agent result and child session transcript.
package subagentview

import (
	"encoding/json"
	"hash/fnv"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

const (
	PreviewToolLimit      = 5
	PreviewTextLineLimit  = 4
	MaxPreviewLineBytes   = 4 * 1024
	MaxExpandedActivities = 256
	MaxArtifacts          = 256
)

// Activity is one durable child tool invocation in transcript order.
type Activity struct {
	CallID  string
	Name    string
	Info    string
	Args    json.RawMessage
	Done    bool
	Success bool
}

// CompletedRun is the immutable render projection for one completed parent
// spawn_agent tool call. Parent result fields remain authoritative for semantic
// output; child fields supply internal activity and metrics.
type CompletedRun struct {
	ParentCallID   string
	ChildSessionID string
	AgentName      string
	Prompt         string
	Output         string
	Error          string
	ErrorType      string
	DurationMs     int64
	Partial        bool

	ChildAvailable bool
	Status         session.SessionStatus
	Provider       string
	Model          string
	StartedAt      time.Time
	CompletedAt    time.Time
	ToolCalls      int
	InputTokens    int
	OutputTokens   int

	Activities  []Activity
	TextPreview []string
	Diffs       []llm.DiffData
	Images      []string
	Fingerprint uint64
}

// Build reconstructs a completed run from already-loaded durable values. child
// and childMessages may be absent; the parent result still provides a useful
// fallback card in that case.
func Build(call *llm.ToolCall, result *llm.ToolResult, parsed tools.SpawnAgentResult, child *session.Session, childMessages []session.Message) CompletedRun {
	run := CompletedRun{
		AgentName:      strings.TrimSpace(parsed.AgentName),
		Output:         parsed.Output,
		Error:          parsed.Error,
		ErrorType:      parsed.Type,
		DurationMs:     parsed.Duration,
		ChildSessionID: strings.TrimSpace(parsed.SessionID),
		Partial:        parsed.Error != "" && parsed.Output != "",
	}
	if call != nil {
		run.ParentCallID = call.ID
		var args tools.SpawnAgentArgs
		if json.Unmarshal(call.Arguments, &args) == nil {
			run.Prompt = strings.TrimSpace(args.Prompt)
			if run.AgentName == "" {
				run.AgentName = strings.TrimSpace(args.AgentName)
			}
		}
	}
	if result != nil {
		run.Diffs = appendBounded(run.Diffs, result.Diffs, MaxArtifacts)
		run.Images = appendBounded(run.Images, result.Images, MaxArtifacts)
		if result.IsError && run.Error == "" {
			run.Error = "spawn_agent failed"
		}
	}
	if child != nil {
		run.ChildAvailable = true
		run.Status = child.Status
		run.Provider = child.Provider
		run.Model = child.Model
		run.StartedAt = child.CreatedAt
		if parsed.Duration > 0 && !child.CreatedAt.IsZero() {
			run.CompletedAt = child.CreatedAt.Add(time.Duration(parsed.Duration) * time.Millisecond)
		} else {
			run.CompletedAt = child.UpdatedAt
		}
		run.ToolCalls = child.ToolCalls
		run.InputTokens = child.InputTokens + child.CachedInputTokens + child.CacheWriteTokens
		run.OutputTokens = child.OutputTokens
	}

	activityByID := make(map[string]int)
	for _, message := range childMessages {
		for _, part := range message.Parts {
			switch part.Type {
			case llm.PartToolCall:
				if part.ToolCall == nil {
					continue
				}
				call := part.ToolCall
				activity := Activity{CallID: call.ID, Name: call.Name, Info: call.ToolInfo, Args: append(json.RawMessage(nil), call.Arguments...)}
				if len(run.Activities) < MaxExpandedActivities {
					activityByID[call.ID] = len(run.Activities)
					run.Activities = append(run.Activities, activity)
				}
			case llm.PartToolActivity:
				if part.ToolActivity == nil || len(run.Activities) >= MaxExpandedActivities {
					continue
				}
				activity := part.ToolActivity
				if activity.ID != "" {
					if _, exists := activityByID[activity.ID]; exists {
						continue
					}
					activityByID[activity.ID] = len(run.Activities)
				}
				run.Activities = append(run.Activities, Activity{
					CallID: activity.ID, Name: activity.Name, Info: activity.Info,
					Args: append(json.RawMessage(nil), activity.Arguments...), Done: true,
					Success: activity.Status != llm.ToolActivityFailed,
				})
			case llm.PartToolResult:
				if part.ToolResult == nil {
					continue
				}
				toolResult := part.ToolResult
				if idx, ok := activityByID[toolResult.ID]; ok {
					run.Activities[idx].Done = true
					run.Activities[idx].Success = !toolResult.IsError
				}
				run.Diffs = appendBounded(run.Diffs, toolResult.Diffs, MaxArtifacts)
				run.Images = appendBounded(run.Images, toolResult.Images, MaxArtifacts)
			}
		}
	}
	if child != nil && child.Status != session.StatusActive {
		for i := range run.Activities {
			if !run.Activities[i].Done {
				run.Activities[i].Done = true
				run.Activities[i].Success = false
			}
		}
	}
	if run.ToolCalls == 0 {
		run.ToolCalls = len(run.Activities)
	}
	run.TextPreview = outputPreviewLines(run.Output, PreviewTextLineLimit)
	run.Diffs = dedupeDiffs(run.Diffs)
	run.Images = dedupeStrings(run.Images)
	run.Fingerprint = fingerprint(run)
	return run
}

func outputPreviewLines(output string, limit int) []string {
	output = strings.ReplaceAll(output, "\r\n", "\n")
	lines := strings.Split(strings.TrimSpace(output), "\n")
	bounded := make([]string, 0, min(len(lines), limit))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > MaxPreviewLineBytes {
			start := len(line) - (MaxPreviewLineBytes - 3)
			for start < len(line) && !utf8.RuneStart(line[start]) {
				start++
			}
			line = "..." + line[start:]
		}
		bounded = append(bounded, line)
	}
	if limit > 0 && len(bounded) > limit {
		bounded = bounded[len(bounded)-limit:]
	}
	return bounded
}

func appendBounded[T any](dst, src []T, limit int) []T {
	if limit <= 0 || len(dst) >= limit || len(src) == 0 {
		return dst
	}
	remaining := limit - len(dst)
	if len(src) > remaining {
		src = src[:remaining]
	}
	return append(dst, src...)
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := values[:0]
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func dedupeDiffs(values []llm.DiffData) []llm.DiffData {
	seen := make(map[string]struct{}, len(values))
	out := values[:0]
	for _, value := range values {
		keyBytes, _ := json.Marshal(value)
		key := string(keyBytes)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func fingerprint(run CompletedRun) uint64 {
	h := fnv.New64a()
	write := func(value string) { _, _ = h.Write([]byte(value)); _, _ = h.Write([]byte{0}) }
	write(run.ParentCallID)
	write(run.ChildSessionID)
	write(run.AgentName)
	write(run.Prompt)
	write(run.Output)
	write(run.Error)
	write(run.ErrorType)
	write(run.Provider)
	write(run.Model)
	write(string(run.Status))
	write(strconv.FormatInt(run.DurationMs, 10))
	write(strconv.Itoa(run.ToolCalls))
	write(strconv.Itoa(run.InputTokens))
	write(strconv.Itoa(run.OutputTokens))
	write(run.StartedAt.UTC().Format(time.RFC3339Nano))
	write(run.CompletedAt.UTC().Format(time.RFC3339Nano))
	for _, activity := range run.Activities {
		write(activity.CallID)
		write(activity.Name)
		write(activity.Info)
		write(string(activity.Args))
		if activity.Done {
			write("done")
		}
		if activity.Success {
			write("success")
		}
	}
	for _, diff := range run.Diffs {
		data, _ := json.Marshal(diff)
		write(string(data))
	}
	for _, image := range run.Images {
		write(image)
	}
	return h.Sum64()
}
