package llm

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// Classify sends a short prompt and returns the first lowercase word in the model response.
func Classify(ctx context.Context, provider Provider, prompt string, timeout time.Duration) (string, error) {
	if provider == nil {
		return "", fmt.Errorf("provider is nil")
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	classifyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stream, err := provider.Stream(classifyCtx, Request{
		Messages: []Message{UserText(prompt)},
		MaxTurns: 1,
	})
	if err != nil {
		return "", err
	}
	defer stream.Close()

	var buf strings.Builder
	for {
		ev, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return "", recvErr
		}
		if ev.Type == EventTextDelta {
			buf.WriteString(ev.Text)
		}
	}

	resp := strings.ToLower(strings.TrimSpace(buf.String()))
	if resp == "" {
		return "", fmt.Errorf("empty classification response")
	}
	fields := strings.Fields(resp)
	if len(fields) == 0 {
		return "", fmt.Errorf("empty classification response")
	}
	return fields[0], nil
}

type InterruptAction int

const (
	InterruptCancel InterruptAction = iota
	InterruptInterject
	InterruptQueue
)

// InterruptActivity summarizes active stream state for interrupt classification.
type InterruptActivity struct {
	CurrentTask string
	ToolsRun    []string
	ActiveTool  string
	ProseLen    int
}

func classifyInterruptHeuristic(msg string) (InterruptAction, bool) {
	normalized := strings.ToLower(strings.TrimSpace(msg))
	if normalized == "" {
		return InterruptQueue, true
	}

	overrides := []string{"/stop", "/cancel", "/takeover", "stop", "cancel", "abort", "never mind", "nevermind", "forget it"}
	for _, w := range overrides {
		if normalized == w || strings.HasPrefix(normalized, w+" ") {
			return InterruptCancel, true
		}
	}

	return InterruptQueue, false
}

// ClassifyInterrupt decides how to handle a new user message while a stream is active.
// It uses instant heuristics first and then an optional fast LLM call.
func ClassifyInterrupt(ctx context.Context, fastProvider Provider, msg string, activity InterruptActivity) InterruptAction {
	if action, ok := classifyInterruptHeuristic(msg); ok {
		return action
	}
	if fastProvider == nil {
		return InterruptQueue
	}

	toolsRun := "none"
	if len(activity.ToolsRun) > 0 {
		toolsRun = strings.Join(activity.ToolsRun, ", ")
	}
	agentActivity := "thinking"
	if strings.TrimSpace(activity.ActiveTool) != "" {
		agentActivity = "running tool " + strings.TrimSpace(activity.ActiveTool)
	} else if strings.TrimSpace(activity.CurrentTask) != "" {
		agentActivity = strings.TrimSpace(activity.CurrentTask)
	}

	prompt := fmt.Sprintf(`You are classifying a user message sent while an AI agent is busy working.

Agent is currently: %s
Agent task: %s
Tools already executed: %s
Agent has produced: %d characters so far.

User's new message: %q

Classify as one word:
- cancel: user wants to stop current work
- interject: user wants to add info/correction, agent should continue and incorporate
- queue: user is starting a new topic, finish current work first

Reply with exactly one word.`,
		agentActivity,
		strings.TrimSpace(activity.CurrentTask),
		toolsRun,
		activity.ProseLen,
		strings.TrimSpace(msg),
	)

	decision, err := Classify(ctx, fastProvider, prompt, 3*time.Second)
	if err != nil {
		return InterruptQueue
	}

	switch strings.TrimSpace(decision) {
	case "cancel", "abort", "stop":
		return InterruptCancel
	case "interject", "inject":
		return InterruptInterject
	case "queue", "wait", "later":
		return InterruptQueue
	default:
		return InterruptQueue
	}
}
