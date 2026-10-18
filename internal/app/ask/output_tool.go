package ask

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

func EnsureOutputToolCaptured(ctx context.Context, provider llm.Provider, registry *llm.ToolRegistry, baseReq llm.Request, outputTool *tools.SetOutputTool, assistantText string) error {
	if outputTool == nil || outputTool.Captured() {
		return nil
	}
	if err := runOutputToolFinalization(ctx, provider, registry, baseReq, outputTool, assistantText); err != nil {
		return err
	}
	if !outputTool.Captured() {
		return fmt.Errorf("output tool %q was not called", outputTool.Name())
	}
	return nil
}

func runOutputToolFinalization(ctx context.Context, provider llm.Provider, registry *llm.ToolRegistry, baseReq llm.Request, outputTool *tools.SetOutputTool, assistantText string) error {
	if outputTool == nil || outputTool.Captured() {
		return nil
	}
	if registry == nil {
		registry = llm.NewToolRegistry()
		registry.Register(outputTool)
	}

	toolName := outputTool.Name()
	messages := append([]llm.Message{}, baseReq.Messages...)
	if strings.TrimSpace(assistantText) != "" {
		messages = append(messages, llm.AssistantText(assistantText))
	}

	const maxFinalizationAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxFinalizationAttempts; attempt++ {
		attemptMessages := append([]llm.Message{}, messages...)
		attemptMessages = append(attemptMessages, llm.UserText(outputToolFinalizationPrompt(toolName, attempt)))

		finalEngine := llm.NewEngine(provider, registry)
		finalReq := llm.Request{
			Model:             baseReq.Model,
			SessionID:         baseReq.SessionID,
			WorkingDir:        baseReq.WorkingDir,
			Messages:          attemptMessages,
			Tools:             []llm.ToolSpec{outputTool.Spec()},
			ToolChoice:        llm.ToolChoice{Mode: llm.ToolChoiceAuto},
			ParallelToolCalls: false,
			MaxTurns:          3,
			MaxOutputTokens:   baseReq.MaxOutputTokens,
			Debug:             baseReq.Debug,
			DebugRaw:          baseReq.DebugRaw,
		}
		if provider.Capabilities().SupportsToolChoice {
			finalReq.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceName, Name: toolName}
		}

		finalizerText, err := drainOutputToolFinalizationStream(ctx, finalEngine, finalReq, outputTool, toolName)
		if err == nil && outputTool.Captured() {
			return nil
		}
		if err != nil {
			lastErr = err
		}
		if outputTool.Captured() {
			return nil
		}
		if strings.TrimSpace(finalizerText) != "" {
			messages = append(messages, llm.AssistantText(finalizerText))
		}
	}

	if lastErr != nil && !isOutputToolFinalizationMiss(lastErr) {
		return fmt.Errorf("output tool finalization failed: %w", lastErr)
	}
	return fmt.Errorf("output tool %q was not called after %d finalization attempts", toolName, maxFinalizationAttempts)
}

func outputToolFinalizationPrompt(toolName string, attempt int) string {
	if attempt <= 1 {
		return fmt.Sprintf(
			"The task is DONE. Do not continue investigating. Do not inspect files. Do not call research, shell, read, edit, search, or any other work tool. Your previous answer is complete, but you failed to return it through the configured output tool %q. You MUST now make exactly one tool call: call %s with the final answer. Do not write prose before or after the tool call.",
			toolName,
			toolName,
		)
	}
	return fmt.Sprintf(
		"FINALIZATION RETRY %d: Your previous response still did not call the required output tool. This is a protocol error. The only valid response is exactly one tool call to %s. Do not output text. Do not explain. Do not call any other tool. Call %s now with the final answer.",
		attempt,
		toolName,
		toolName,
	)
}

func drainOutputToolFinalizationStream(ctx context.Context, finalEngine *llm.Engine, finalReq llm.Request, outputTool *tools.SetOutputTool, toolName string) (string, error) {
	stream, err := finalEngine.Stream(ctx, finalReq)
	if err != nil {
		if outputTool.Captured() && isOutputToolFinalizationMiss(err) {
			return "", nil
		}
		if !outputTool.Captured() && isOutputToolFinalizationMiss(err) {
			return "", fmt.Errorf("output tool %q was not called", toolName)
		}
		return "", err
	}
	defer stream.Close()

	var text strings.Builder
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			if outputTool.Captured() && isOutputToolFinalizationMiss(err) {
				break
			}
			if !outputTool.Captured() && isOutputToolFinalizationMiss(err) {
				return text.String(), fmt.Errorf("output tool %q was not called", toolName)
			}
			return text.String(), err
		}
		if event.Type == llm.EventTextDelta && event.Text != "" {
			text.WriteString(event.Text)
		}
		if event.Type == llm.EventError && event.Err != nil {
			if outputTool.Captured() && isOutputToolFinalizationMiss(event.Err) {
				break
			}
			if !outputTool.Captured() && isOutputToolFinalizationMiss(event.Err) {
				return text.String(), fmt.Errorf("output tool %q was not called", toolName)
			}
			return text.String(), event.Err
		}
	}

	if !outputTool.Captured() {
		return text.String(), fmt.Errorf("output tool %q was not called", toolName)
	}
	return text.String(), nil
}

func isOutputToolFinalizationMiss(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "agentic loop ended unexpectedly") || llm.IsMaxTurnsExceeded(err) || strings.Contains(msg, "no more turns configured") || strings.Contains(msg, "output tool") && strings.Contains(msg, "was not called")
}

func ensureOutputToolSpec(specs []llm.ToolSpec, outputSpec llm.ToolSpec) []llm.ToolSpec {
	if outputSpec.Name == "" {
		return append([]llm.ToolSpec(nil), specs...)
	}
	out := append([]llm.ToolSpec(nil), specs...)
	for _, spec := range out {
		if spec.Name == outputSpec.Name {
			return out
		}
	}
	return append(out, outputSpec)
}
