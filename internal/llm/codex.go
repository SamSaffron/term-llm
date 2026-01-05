package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const chatGPTResponsesURL = "https://chatgpt.com/backend-api/codex/responses"

// Model family -> prompt file mapping (from openai/codex repo)
var codexPromptFiles = map[string]string{
	"gpt-5.2-codex": "gpt-5.2-codex_prompt.md",
	"codex-max":     "gpt-5.1-codex-max_prompt.md",
	"codex":         "gpt_5_codex_prompt.md",
	"gpt-5.2":       "gpt_5_2_prompt.md",
	"gpt-5.1":       "gpt_5_1_prompt.md",
}

// instructionsCache caches Codex instructions in memory with TTL
var (
	instructionsCache    = make(map[string]cachedInstructions)
	instructionsCacheMu  sync.RWMutex
	instructionsCacheTTL = 15 * time.Minute
)

type cachedInstructions struct {
	content   string
	fetchedAt time.Time
}

// CodexProvider implements Provider using the ChatGPT backend API with Codex OAuth.
type CodexProvider struct {
	accessToken string
	accountID   string
	model       string
	effort      string // reasoning effort: "low", "medium", "high", "xhigh", or ""
	client      *http.Client
}

func NewCodexProvider(accessToken, model, accountID string) *CodexProvider {
	actualModel, effort := parseModelEffort(model)
	return &CodexProvider{
		accessToken: accessToken,
		accountID:   accountID,
		model:       actualModel,
		effort:      effort,
		client:      &http.Client{},
	}
}

func (p *CodexProvider) Name() string {
	if p.effort != "" {
		return fmt.Sprintf("Codex (%s, effort=%s)", p.model, p.effort)
	}
	return fmt.Sprintf("Codex (%s)", p.model)
}

func (p *CodexProvider) Capabilities() Capabilities {
	return Capabilities{NativeSearch: true, ToolCalls: true}
}

func (p *CodexProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		codexInstructions, err := p.getCodexInstructions()
		if err != nil {
			return fmt.Errorf("failed to get Codex instructions: %w", err)
		}

		system, user := flattenSystemUser(req.Messages)
		combinedPrompt := strings.TrimSpace(strings.Join([]string{system, user}, "\n\n"))
		if combinedPrompt == "" {
			return fmt.Errorf("no prompt content provided")
		}

		tools := []interface{}{}
		if req.Search {
			tools = append(tools, map[string]interface{}{"type": "web_search"})
		}
		for _, spec := range req.Tools {
			tools = append(tools, map[string]interface{}{
				"type":        "function",
				"name":        spec.Name,
				"description": spec.Description,
				"strict":      true,
				"parameters":  spec.Schema,
			})
		}

		reqBody := map[string]interface{}{
			"model":               chooseModel(req.Model, p.model),
			"instructions":        codexInstructions,
			"input":               p.buildInput(combinedPrompt),
			"tools":               tools,
			"tool_choice":         "auto",
			"parallel_tool_calls": req.ParallelToolCalls,
			"stream":              true,
			"store":               false,
			"include":             []string{},
		}

		if p.effort != "" {
			reqBody["reasoning"] = map[string]interface{}{
				"effort": p.effort,
			}
		}

		body, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("failed to marshal request: %w", err)
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", chatGPTResponsesURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}

		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+p.accessToken)
		httpReq.Header.Set("ChatGPT-Account-ID", p.accountID)
		httpReq.Header.Set("OpenAI-Beta", "responses=experimental")
		httpReq.Header.Set("originator", "codex_cli_rs")
		httpReq.Header.Set("Accept", "text/event-stream")

		resp, err := p.client.Do(httpReq)
		if err != nil {
			return fmt.Errorf("request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("API error (%d): %s", resp.StatusCode, string(respBody))
		}

		if len(req.Tools) > 0 {
			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("failed to read response: %w", err)
			}
			parsed, err := p.parseSSEResponse(respBody)
			if err != nil {
				return err
			}
			for _, item := range parsed.Output {
				if item.Type == "function_call" {
					events <- Event{Type: EventToolCall, Tool: &ToolCall{
						ID:        item.ID,
						Name:      item.Name,
						Arguments: json.RawMessage(item.Arguments),
					}}
				}
			}
			events <- Event{Type: EventDone}
			return nil
		}

		buf := make([]byte, 4096)
		var pending string
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				pending += string(buf[:n])
				for {
					idx := strings.Index(pending, "\n")
					if idx < 0 {
						break
					}
					line := pending[:idx]
					pending = pending[idx+1:]
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					jsonData := strings.TrimPrefix(line, "data: ")
					if jsonData == "" || jsonData == "[DONE]" {
						continue
					}
					var event struct {
						Type  string `json:"type"`
						Delta string `json:"delta"`
					}
					if json.Unmarshal([]byte(jsonData), &event) == nil {
						if event.Type == "response.output_text.delta" && event.Delta != "" {
							events <- Event{Type: EventTextDelta, Text: event.Delta}
						}
					}
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("stream read error: %w", err)
			}
		}

		events <- Event{Type: EventDone}
		return nil
	}), nil
}

func (p *CodexProvider) buildInput(userPrompt string) []map[string]interface{} {
	return []map[string]interface{}{
		{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": userPrompt},
			},
		},
	}
}

// Response structures for parsing SSE

type codexResponse struct {
	ID     string       `json:"id"`
	Status string       `json:"status"`
	Output []outputItem `json:"output"`
}

type outputItem struct {
	Type      string        `json:"type"`
	ID        string        `json:"id,omitempty"`
	Name      string        `json:"name,omitempty"`
	Arguments string        `json:"arguments,omitempty"`
	Content   []contentItem `json:"content,omitempty"`
}

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func (p *CodexProvider) parseSSEResponse(data []byte) (*codexResponse, error) {
	lines := strings.Split(string(data), "\n")
	var lastResponse *codexResponse

	for _, line := range lines {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		jsonData := strings.TrimPrefix(line, "data: ")
		if jsonData == "" || jsonData == "[DONE]" {
			continue
		}

		var response codexResponse
		if err := json.Unmarshal([]byte(jsonData), &response); err != nil {
			continue
		}
		if response.ID != "" {
			lastResponse = &response
		}
	}

	if lastResponse == nil {
		return nil, fmt.Errorf("no response found in SSE stream")
	}
	return lastResponse, nil
}

func (p *CodexProvider) getCodexInstructions() (string, error) {
	family := p.getModelFamily()

	instructionsCacheMu.RLock()
	cached, found := instructionsCache[family]
	instructionsCacheMu.RUnlock()

	if found && time.Since(cached.fetchedAt) < instructionsCacheTTL {
		return cached.content, nil
	}

	cacheDir := filepath.Join(os.TempDir(), "term-llm-codex")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create cache dir: %w", err)
	}

	promptFile, ok := codexPromptFiles[family]
	if !ok {
		promptFile = codexPromptFiles["gpt-5.1"]
	}

	cachedPath := filepath.Join(cacheDir, promptFile)
	metaPath := cachedPath + ".meta"

	if data, err := os.ReadFile(cachedPath); err == nil {
		if info, err := os.Stat(metaPath); err == nil {
			if time.Since(info.ModTime()) < instructionsCacheTTL {
				content := string(data)
				p.cacheInstructions(family, content)
				return content, nil
			}
		}
	}

	latestTag, err := p.getLatestReleaseTag()
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("https://raw.githubusercontent.com/openai/codex/%s/codex-rs/core/%s", latestTag, promptFile)
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to fetch instructions: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to fetch instructions: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read instructions: %w", err)
	}

	content := string(body)
	if err := os.WriteFile(cachedPath, body, 0644); err == nil {
		p.saveCacheMeta(metaPath, latestTag)
	}

	p.cacheInstructions(family, content)
	return content, nil
}

func (p *CodexProvider) getModelFamily() string {
	model := strings.ToLower(p.model)
	switch {
	case strings.Contains(model, "codex"):
		return "gpt-5.2-codex"
	case strings.Contains(model, "gpt-5.2"):
		return "gpt-5.2"
	case strings.Contains(model, "gpt-5.1"):
		return "gpt-5.1"
	default:
		return "gpt-5.1"
	}
}

func (p *CodexProvider) getLatestReleaseTag() (string, error) {
	resp, err := http.Get("https://api.github.com/repos/openai/codex/releases/latest")
	if err != nil {
		return "", fmt.Errorf("failed to fetch latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to fetch latest release: %s", resp.Status)
	}

	var data struct {
		TagName string `json:"tag_name"`
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if data.TagName != "" {
		return data.TagName, nil
	}

	return "", fmt.Errorf("could not determine latest release tag")
}

func (p *CodexProvider) cacheInstructions(family, content string) {
	instructionsCacheMu.Lock()
	instructionsCache[family] = cachedInstructions{content: content, fetchedAt: time.Now()}
	instructionsCacheMu.Unlock()
}

func (p *CodexProvider) saveCacheMeta(path, tag string) {
	meta := struct {
		Tag         string `json:"tag"`
		LastChecked int64  `json:"lastChecked"`
	}{
		Tag:         tag,
		LastChecked: time.Now().UnixNano(),
	}
	if data, err := json.Marshal(meta); err == nil {
		os.WriteFile(path, data, 0644)
	}
}
