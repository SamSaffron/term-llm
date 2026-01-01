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

	"github.com/samsaffron/term-llm/internal/prompt"
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
	instructionsCache   = make(map[string]cachedInstructions)
	instructionsCacheMu sync.RWMutex
	instructionsCacheTTL = 15 * time.Minute
)

type cachedInstructions struct {
	content   string
	fetchedAt time.Time
}

// CodexProvider implements Provider using the ChatGPT backend API with Codex OAuth
type CodexProvider struct {
	accessToken string
	accountID   string
	model       string
	client      *http.Client
}

func NewCodexProvider(accessToken, model, accountID string) *CodexProvider {
	return &CodexProvider{
		accessToken: accessToken,
		accountID:   accountID,
		model:       model,
		client:      &http.Client{},
	}
}

func (p *CodexProvider) Name() string {
	return fmt.Sprintf("Codex (%s)", p.model)
}

func (p *CodexProvider) SuggestCommands(ctx context.Context, req SuggestRequest) ([]CommandSuggestion, error) {
	numSuggestions := req.NumSuggestions
	if numSuggestions <= 0 {
		numSuggestions = 3
	}

	// Fetch Codex instructions from GitHub (required by ChatGPT backend)
	// NOTE: Instructions must be EXACTLY the Codex instructions - no modifications allowed
	codexInstructions, err := p.getCodexInstructions()
	if err != nil {
		return nil, fmt.Errorf("failed to get Codex instructions: %w", err)
	}

	// Build tools list
	tools := []interface{}{}

	// Add web search tool if enabled (must come before function tools)
	if req.EnableSearch {
		tools = append(tools, map[string]interface{}{"type": "web_search"})
	}

	// Add suggest_commands function tool
	// ChatGPT backend uses flat format (name at top level, not nested in function)
	tools = append(tools, map[string]interface{}{
		"type":        "function",
		"name":        "suggest_commands",
		"description": "Suggest shell commands based on user input",
		"strict":      true,
		"parameters":  prompt.SuggestSchema(numSuggestions),
	})

	// Combine system context with user prompt (instructions must stay unchanged)
	systemPrompt := prompt.SuggestSystemPrompt(req.Shell, req.Instructions, numSuggestions, req.EnableSearch)
	userPrompt := prompt.SuggestUserPrompt(req.UserInput)
	combinedPrompt := systemPrompt + "\n\n" + userPrompt

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: Codex Request ===")
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "Backend: ChatGPT (chatgpt.com/backend-api/codex)\n")
		fmt.Fprintf(os.Stderr, "Codex instructions: %d chars\n", len(codexInstructions))
		if req.EnableSearch {
			fmt.Fprintln(os.Stderr, "Tools: web_search, suggest_commands")
		} else {
			fmt.Fprintln(os.Stderr, "Tools: suggest_commands")
		}
		fmt.Fprintf(os.Stderr, "Combined prompt:\n%s\n", combinedPrompt)
		fmt.Fprintln(os.Stderr, "============================")
	}

	// Build request body
	reqBody := map[string]interface{}{
		"model":               p.model,
		"instructions":        codexInstructions,
		"input":               p.buildInput(combinedPrompt),
		"tools":               tools,
		"tool_choice":         "auto",
		"parallel_tool_calls": true,
		"stream":              true,
		"store":               false,
		"include":             []string{},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", chatGPTResponsesURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set required headers
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.accessToken)
	httpReq.Header.Set("ChatGPT-Account-ID", p.accountID)
	httpReq.Header.Set("OpenAI-Beta", "responses=experimental")
	httpReq.Header.Set("originator", "codex_cli_rs")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (%d): %s", resp.StatusCode, string(respBody))
	}

	// Parse SSE response
	result, err := p.parseSSEResponse(respBody)
	if err != nil {
		return nil, err
	}

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: Codex Response ===")
		fmt.Fprintf(os.Stderr, "Status: %s\n", result.Status)
		fmt.Fprintf(os.Stderr, "Output items: %d\n", len(result.Output))
		for i, item := range result.Output {
			fmt.Fprintf(os.Stderr, "Output %d: type=%s", i, item.Type)
			if item.Name != "" {
				fmt.Fprintf(os.Stderr, " name=%s", item.Name)
			}
			if item.ID != "" {
				fmt.Fprintf(os.Stderr, " id=%s", item.ID)
			}
			fmt.Fprintln(os.Stderr)
		}
		fmt.Fprintln(os.Stderr, "=============================")
	}

	// Find the function call output
	for _, item := range result.Output {
		if item.Type == "function_call" && item.Name == "suggest_commands" {
			var suggestions suggestionsResponse
			if err := json.Unmarshal([]byte(item.Arguments), &suggestions); err != nil {
				return nil, fmt.Errorf("failed to parse response: %w", err)
			}
			return suggestions.Suggestions, nil
		}
	}

	// Check for text response (model didn't call the function)
	for _, item := range result.Output {
		if item.Type == "message" {
			for _, c := range item.Content {
				if c.Type == "output_text" && c.Text != "" {
					return nil, fmt.Errorf("model returned text instead of function call: %s", c.Text)
				}
			}
		}
	}

	return nil, fmt.Errorf("no suggest_commands function call in response")
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

		var event struct {
			Type     string         `json:"type"`
			Response *codexResponse `json:"response"`
		}
		if err := json.Unmarshal([]byte(jsonData), &event); err != nil {
			continue
		}

		if event.Response != nil {
			lastResponse = event.Response
		}
	}

	if lastResponse == nil {
		return nil, fmt.Errorf("no response found in SSE stream")
	}
	return lastResponse, nil
}

// Codex instructions fetching with caching

func (p *CodexProvider) getCodexInstructions() (string, error) {
	family := p.getModelFamily()

	// Check memory cache first
	instructionsCacheMu.RLock()
	if cached, ok := instructionsCache[family]; ok && time.Since(cached.fetchedAt) < instructionsCacheTTL {
		instructionsCacheMu.RUnlock()
		return cached.content, nil
	}
	instructionsCacheMu.RUnlock()

	// Check disk cache
	cacheDir, _ := os.UserCacheDir()
	cacheFile := filepath.Join(cacheDir, "term-llm", "codex-instructions-"+family+".md")
	cacheMetaFile := filepath.Join(cacheDir, "term-llm", "codex-instructions-"+family+"-meta.json")

	var cachedTag string
	if metaBytes, err := os.ReadFile(cacheMetaFile); err == nil {
		var meta struct {
			Tag         string `json:"tag"`
			LastChecked int64  `json:"lastChecked"`
		}
		if json.Unmarshal(metaBytes, &meta) == nil {
			// If cache is recent, use it
			if time.Since(time.Unix(0, meta.LastChecked)) < instructionsCacheTTL {
				if content, err := os.ReadFile(cacheFile); err == nil {
					p.cacheInstructions(family, string(content))
					return string(content), nil
				}
			}
			cachedTag = meta.Tag
		}
	}

	// Fetch from GitHub
	tag, err := p.getLatestReleaseTag()
	if err != nil {
		// Try using cached version if available
		if content, err := os.ReadFile(cacheFile); err == nil {
			return string(content), nil
		}
		return "", fmt.Errorf("failed to get release tag: %w", err)
	}

	promptFile, ok := codexPromptFiles[family]
	if !ok {
		promptFile = codexPromptFiles["gpt-5.1"]
	}

	// If tag hasn't changed and we have cached content, use it
	if tag == cachedTag {
		if content, err := os.ReadFile(cacheFile); err == nil {
			p.saveCacheMeta(cacheMetaFile, tag)
			p.cacheInstructions(family, string(content))
			return string(content), nil
		}
	}

	url := fmt.Sprintf("https://raw.githubusercontent.com/openai/codex/%s/codex-rs/core/%s", tag, promptFile)
	resp, err := http.Get(url)
	if err != nil {
		if content, err := os.ReadFile(cacheFile); err == nil {
			return string(content), nil
		}
		return "", fmt.Errorf("failed to fetch instructions: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		if content, err := os.ReadFile(cacheFile); err == nil {
			return string(content), nil
		}
		return "", fmt.Errorf("GitHub returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	content := string(body)

	// Save to disk cache
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0755); err == nil {
		os.WriteFile(cacheFile, body, 0644)
		p.saveCacheMeta(cacheMetaFile, tag)
	}

	p.cacheInstructions(family, content)
	return content, nil
}

func (p *CodexProvider) getModelFamily() string {
	model := strings.ToLower(p.model)
	if strings.Contains(model, "gpt-5.2-codex") || strings.Contains(model, "gpt 5.2 codex") {
		return "gpt-5.2-codex"
	}
	if strings.Contains(model, "codex-max") {
		return "codex-max"
	}
	if strings.Contains(model, "codex") || strings.HasPrefix(model, "codex-") {
		return "codex"
	}
	if strings.Contains(model, "gpt-5.2") {
		return "gpt-5.2"
	}
	return "gpt-5.1"
}

func (p *CodexProvider) getLatestReleaseTag() (string, error) {
	// Try GitHub API first
	resp, err := http.Get("https://api.github.com/repos/openai/codex/releases/latest")
	if err == nil && resp.StatusCode == 200 {
		defer resp.Body.Close()
		var release struct {
			TagName string `json:"tag_name"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&release); err == nil && release.TagName != "" {
			return release.TagName, nil
		}
	}

	// Fallback: follow redirect from releases/latest
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err = client.Get("https://github.com/openai/codex/releases/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	location := resp.Header.Get("Location")
	if location != "" {
		parts := strings.Split(location, "/tag/")
		if len(parts) == 2 {
			return parts[1], nil
		}
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

func (p *CodexProvider) StreamResponse(ctx context.Context, req AskRequest, output chan<- string) error {
	defer close(output)

	// Fetch Codex instructions from GitHub (required by ChatGPT backend)
	codexInstructions, err := p.getCodexInstructions()
	if err != nil {
		return fmt.Errorf("failed to get Codex instructions: %w", err)
	}

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: Codex Stream Request ===")
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "Question: %s\n", req.Question)
		fmt.Fprintf(os.Stderr, "Search: %v\n", req.EnableSearch)
		fmt.Fprintln(os.Stderr, "===================================")
	}

	// Build request body
	reqBody := map[string]interface{}{
		"model":        p.model,
		"instructions": codexInstructions,
		"input":        p.buildInput(req.Question),
		"stream":       true,
		"store":        false,
	}

	// Add web search tool if enabled
	if req.EnableSearch {
		reqBody["tools"] = []interface{}{
			map[string]interface{}{"type": "web_search"},
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

	// Set required headers
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

	// Read SSE stream and send text deltas
	buf := make([]byte, 4096)
	var pending string
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			pending += string(buf[:n])
			// Process complete lines
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
						output <- event.Delta
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

	return nil
}
