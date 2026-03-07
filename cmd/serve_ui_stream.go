package cmd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

type serveUIResponseEvent struct {
	name    string
	payload any
	done    bool
}

func isServeUIRequest(r *http.Request) bool {
	value := strings.TrimSpace(strings.ToLower(r.Header.Get("X-Term-LLM-UI")))
	return value == "1" || value == "true" || value == "yes"
}

func publishServeUIResponseEvent(ch chan<- serveUIResponseEvent, liveDone <-chan struct{}, event serveUIResponseEvent) {
	select {
	case <-liveDone:
		return
	case ch <- event:
	}
}

func serveUILiveDetached(liveDone <-chan struct{}) bool {
	select {
	case <-liveDone:
		return true
	default:
		return false
	}
}

func (s *serveServer) streamUIResponses(w http.ResponseWriter, r *http.Request, runtime *serveRuntime, stateful bool, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, sessionID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming not supported")
		return
	}

	setSSEHeaders(w)
	liveEvents := make(chan serveUIResponseEvent, 32)
	liveDone := make(chan struct{})
	var liveDoneOnce sync.Once

	go s.runUIResponsesDetached(runtime, stateful, replaceHistory, inputMessages, llmReq, sessionID, liveEvents, liveDone)

	pingMu, stopPing := sseKeepalive(w, flusher, 20*time.Second)
	defer stopPing()
	defer liveDoneOnce.Do(func() { close(liveDone) })

	for {
		select {
		case <-r.Context().Done():
			return
		case evt, ok := <-liveEvents:
			if !ok {
				return
			}
			pingMu.Lock()
			if evt.done {
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			} else {
				_ = writeSSEEvent(w, evt.name, evt.payload)
			}
			flusher.Flush()
			pingMu.Unlock()
		}
	}
}

func (s *serveServer) runUIResponsesDetached(runtime *serveRuntime, stateful bool, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, sessionID string, liveEvents chan<- serveUIResponseEvent, liveDone <-chan struct{}) {
	defer close(liveEvents)

	runtime.clearLastUIRunError()

	runCtx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	respID := "resp_" + randomSuffix()
	model := llmReq.Model
	if model == "" {
		model = runtime.defaultModel
	}
	created := time.Now().Unix()

	publish := func(name string, payload any) {
		publishServeUIResponseEvent(liveEvents, liveDone, serveUIResponseEvent{name: name, payload: payload})
	}

	publish("response.created", map[string]any{
		"response": map[string]any{
			"id":      respID,
			"object":  "response",
			"created": created,
			"model":   model,
			"status":  "in_progress",
		},
	})

	outputIndex := 0
	toolsSeen := false
	result, err := runtime.RunWithEvents(runCtx, stateful, replaceHistory, inputMessages, llmReq, func(ev llm.Event) error {
		switch ev.Type {
		case llm.EventTextDelta:
			if toolsSeen {
				publish("response.output_text.new_segment", map[string]any{"output_index": outputIndex})
				toolsSeen = false
			}
			publish("response.output_text.delta", map[string]any{
				"output_index": outputIndex,
				"delta":        ev.Text,
			})
		case llm.EventToolCall:
			if ev.Tool == nil {
				return nil
			}
			toolsSeen = true
			item := map[string]any{
				"id":        "fc_" + ev.Tool.ID,
				"type":      "function_call",
				"call_id":   ev.Tool.ID,
				"name":      ev.Tool.Name,
				"arguments": string(ev.Tool.Arguments),
			}
			publish("response.output_item.added", map[string]any{"output_index": outputIndex, "item": item})
			publish("response.function_call_arguments.delta", map[string]any{"output_index": outputIndex, "delta": string(ev.Tool.Arguments)})
			publish("response.output_item.done", map[string]any{"output_index": outputIndex, "item": item})
			outputIndex++
		case llm.EventToolExecStart:
			if ev.ToolName == tools.AskUserToolName {
				if prompt, promptErr := runtime.prepareAskUserFromToolArgs(ev.ToolCallID, ev.ToolArgs); promptErr == nil {
					publish("response.ask_user.prompt", prompt)
				}
			}
			publish("response.tool_exec.start", map[string]any{
				"call_id":        ev.ToolCallID,
				"tool_name":      ev.ToolName,
				"tool_info":      ev.ToolInfo,
				"tool_arguments": string(ev.ToolArgs),
			})
		case llm.EventToolExecEnd:
			if ev.ToolName == tools.AskUserToolName {
				runtime.clearPendingAskUser(ev.ToolCallID)
			}
			payload := map[string]any{
				"call_id":   ev.ToolCallID,
				"tool_name": ev.ToolName,
				"success":   ev.ToolSuccess,
			}
			if len(ev.ToolImages) > 0 {
				imageURLs := make([]string, 0, len(ev.ToolImages))
				for _, imgPath := range ev.ToolImages {
					imageURLs = append(imageURLs, "/images/"+filepath.Base(imgPath))
				}
				payload["images"] = imageURLs
			}
			publish("response.tool_exec.end", payload)
		case llm.EventHeartbeat:
			publish("response.heartbeat", map[string]any{
				"call_id":   ev.ToolCallID,
				"tool_name": ev.ToolName,
			})
		case llm.EventInterjection:
			publish("response.interjection", map[string]any{"text": ev.Text})
		}
		return nil
	})

	if err != nil {
		if serveUILiveDetached(liveDone) {
			runtime.setLastUIRunError(err.Error())
		} else {
			runtime.clearLastUIRunError()
		}
		errType := "invalid_request_error"
		if errors.Is(err, errServeSessionBusy) {
			errType = "conflict_error"
		}
		publish("response.failed", map[string]any{
			"error": map[string]any{"message": err.Error(), "type": errType},
		})
		publishServeUIResponseEvent(liveEvents, liveDone, serveUIResponseEvent{done: true})
		return
	}

	runtime.clearLastUIRunError()
	s.registerResponseID(runtime, respID, sessionID)
	publish("response.completed", map[string]any{
		"response": map[string]any{
			"id":      respID,
			"object":  "response",
			"created": created,
			"model":   model,
			"status":  "completed",
			"usage": map[string]any{
				"input_tokens":  result.Usage.InputTokens,
				"output_tokens": result.Usage.OutputTokens,
				"total_tokens":  result.Usage.InputTokens + result.Usage.OutputTokens,
				"input_tokens_details": map[string]any{
					"cached_tokens": result.Usage.CachedInputTokens,
				},
			},
			"session_usage": map[string]any{
				"input_tokens":  result.SessionUsage.InputTokens,
				"output_tokens": result.SessionUsage.OutputTokens,
				"total_tokens":  result.SessionUsage.InputTokens + result.SessionUsage.OutputTokens,
				"input_tokens_details": map[string]any{
					"cached_tokens": result.SessionUsage.CachedInputTokens,
				},
			},
		},
	})
	publishServeUIResponseEvent(liveEvents, liveDone, serveUIResponseEvent{done: true})
}
