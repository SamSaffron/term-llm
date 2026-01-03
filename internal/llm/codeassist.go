package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/prompt"
)

const (
	codeAssistEndpoint   = "https://cloudcode-pa.googleapis.com"
	codeAssistAPIVersion = "v1internal"
	googleTokenEndpoint  = "https://oauth2.googleapis.com/token"
)

// GeminiOAuthCredentials holds the OAuth credentials loaded from ~/.gemini/oauth_creds.json
type GeminiOAuthCredentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiryDate   int64  `json:"expiry_date"`
}

// geminiOAuthClientCreds holds the OAuth client ID and secret
type geminiOAuthClientCreds struct {
	clientID     string
	clientSecret string
}

// getGeminiOAuthClientCreds reads the OAuth client credentials from gemini-cli installation
func getGeminiOAuthClientCreds() (*geminiOAuthClientCreds, error) {
	// Try to find gemini-cli installation and read credentials from oauth2.js
	geminiPath, err := exec.LookPath("gemini")
	if err != nil {
		return nil, fmt.Errorf("gemini-cli not found in PATH: %w", err)
	}

	// Resolve symlink to find actual installation
	realPath, err := filepath.EvalSymlinks(geminiPath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve gemini path: %w", err)
	}

	// Navigate from dist/index.js to find oauth2.js
	// Path: .../node_modules/@google/gemini-cli/dist/index.js
	// Target: .../node_modules/@google/gemini-cli-core/dist/src/code_assist/oauth2.js
	baseDir := filepath.Dir(filepath.Dir(realPath)) // go up from dist/index.js to package root
	oauth2Path := filepath.Join(baseDir, "node_modules", "@google", "gemini-cli-core", "dist", "src", "code_assist", "oauth2.js")

	content, err := os.ReadFile(oauth2Path)
	if err != nil {
		return nil, fmt.Errorf("failed to read oauth2.js from gemini-cli: %w", err)
	}

	// Extract client ID and secret using simple string matching
	contentStr := string(content)

	clientID := extractConstant(contentStr, "OAUTH_CLIENT_ID")
	clientSecret := extractConstant(contentStr, "OAUTH_CLIENT_SECRET")

	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("failed to extract OAuth credentials from gemini-cli")
	}

	return &geminiOAuthClientCreds{
		clientID:     clientID,
		clientSecret: clientSecret,
	}, nil
}

// extractConstant extracts a string constant value from JS source
func extractConstant(content, name string) string {
	// Look for: const NAME = 'value';
	prefix := "const " + name + " = '"
	start := strings.Index(content, prefix)
	if start == -1 {
		return ""
	}
	start += len(prefix)
	end := strings.Index(content[start:], "'")
	if end == -1 {
		return ""
	}
	return content[start : start+end]
}

// CodeAssistProvider implements Provider using Google Code Assist API with OAuth
type CodeAssistProvider struct {
	creds       *GeminiOAuthCredentials
	model       string
	projectID   string                  // cached after loadCodeAssist
	clientCreds *geminiOAuthClientCreds // cached OAuth client credentials
}

func NewCodeAssistProvider(creds *GeminiOAuthCredentials, model string) *CodeAssistProvider {
	return &CodeAssistProvider{
		creds: creds,
		model: model,
	}
}

func (p *CodeAssistProvider) Name() string {
	return fmt.Sprintf("Gemini Code Assist (%s)", p.model)
}

// getAccessToken returns a valid access token, refreshing if needed
func (p *CodeAssistProvider) getAccessToken() (string, error) {
	now := time.Now().UnixMilli()
	bufferMs := int64(5 * 60 * 1000) // 5 minute buffer

	if p.creds.ExpiryDate == 0 || now < p.creds.ExpiryDate-bufferMs {
		return p.creds.AccessToken, nil
	}

	// Ensure we have client credentials for refresh
	if p.clientCreds == nil {
		creds, err := getGeminiOAuthClientCreds()
		if err != nil {
			return "", fmt.Errorf("failed to get OAuth client credentials: %w", err)
		}
		p.clientCreds = creds
	}

	// Refresh the token
	data := url.Values{}
	data.Set("client_id", p.clientCreds.clientID)
	data.Set("client_secret", p.clientCreds.clientSecret)
	data.Set("refresh_token", p.creds.RefreshToken)
	data.Set("grant_type", "refresh_token")

	resp, err := http.Post(googleTokenEndpoint, "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
	if err != nil {
		return "", fmt.Errorf("failed to refresh token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token refresh failed: %s", string(body))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("failed to parse token response: %w", err)
	}

	p.creds.AccessToken = tokenResp.AccessToken
	p.creds.ExpiryDate = time.Now().UnixMilli() + int64(tokenResp.ExpiresIn)*1000

	return tokenResp.AccessToken, nil
}

// ensureProjectID calls loadCodeAssist to get the project ID if not already cached
func (p *CodeAssistProvider) ensureProjectID(ctx context.Context) error {
	if p.projectID != "" {
		return nil
	}

	token, err := p.getAccessToken()
	if err != nil {
		return err
	}

	platform := "PLATFORM_UNSPECIFIED"
	switch runtime.GOOS {
	case "darwin":
		if runtime.GOARCH == "arm64" {
			platform = "DARWIN_ARM64"
		} else {
			platform = "DARWIN_AMD64"
		}
	case "linux":
		if runtime.GOARCH == "arm64" {
			platform = "LINUX_ARM64"
		} else {
			platform = "LINUX_AMD64"
		}
	case "windows":
		platform = "WINDOWS_AMD64"
	}

	reqBody := map[string]interface{}{
		"metadata": map[string]string{
			"ideType":    "IDE_UNSPECIFIED",
			"platform":   platform,
			"pluginType": "GEMINI",
		},
	}

	reqJSON, _ := json.Marshal(reqBody)
	url := fmt.Sprintf("%s/%s:loadCodeAssist", codeAssistEndpoint, codeAssistAPIVersion)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqJSON))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("loadCodeAssist request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("loadCodeAssist failed with status %d: %s", resp.StatusCode, string(body))
	}

	var loadResp struct {
		CloudaicompanionProject string `json:"cloudaicompanionProject"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&loadResp); err != nil {
		return fmt.Errorf("failed to parse loadCodeAssist response: %w", err)
	}

	p.projectID = loadResp.CloudaicompanionProject
	return nil
}

func (p *CodeAssistProvider) SuggestCommands(ctx context.Context, req SuggestRequest) ([]CommandSuggestion, error) {
	if req.EnableSearch {
		return p.suggestWithSearch(ctx, req)
	}
	return p.suggestWithoutSearch(ctx, req)
}

// performSearch performs a Google Search query and returns the search context
func (p *CodeAssistProvider) performSearch(ctx context.Context, query string, debug bool) (string, error) {
	if err := p.ensureProjectID(ctx); err != nil {
		return "", err
	}

	token, err := p.getAccessToken()
	if err != nil {
		return "", err
	}

	searchPrompt := fmt.Sprintf("Search for current information about: %s\n\nProvide a concise summary of the most relevant and up-to-date information found.", query)

	requestInner := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"role": "user",
				"parts": []map[string]interface{}{
					{"text": searchPrompt},
				},
			},
		},
		"tools": []map[string]interface{}{
			{"googleSearch": map[string]interface{}{}},
		},
	}

	reqBody := map[string]interface{}{
		"model":          p.model,
		"project":        p.projectID,
		"user_prompt_id": fmt.Sprintf("search-%d", time.Now().UnixNano()),
		"request":        requestInner,
	}

	if debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: Code Assist Search Request ===")
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "Query: %s\n", query)
		fmt.Fprintln(os.Stderr, "==========================================")
	}

	reqJSON, _ := json.Marshal(reqBody)
	url := fmt.Sprintf("%s/%s:generateContent", codeAssistEndpoint, codeAssistAPIVersion)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqJSON))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("search failed with status %d: %s", resp.StatusCode, string(body))
	}

	var genResp struct {
		Response struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		} `json:"response"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&genResp); err != nil {
		return "", fmt.Errorf("failed to parse search response: %w", err)
	}

	if len(genResp.Response.Candidates) == 0 || len(genResp.Response.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("no search results from model")
	}

	searchResult := genResp.Response.Candidates[0].Content.Parts[0].Text

	if debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: Code Assist Search Response ===")
		fmt.Fprintf(os.Stderr, "Result: %s\n", searchResult)
		fmt.Fprintln(os.Stderr, "===========================================")
	}

	return searchResult, nil
}

func (p *CodeAssistProvider) suggestWithSearch(ctx context.Context, req SuggestRequest) ([]CommandSuggestion, error) {
	// Phase 1: Perform search to get current information
	searchContext, err := p.performSearch(ctx, req.UserInput, req.Debug)
	if err != nil {
		// If search fails, fall back to suggestions without search
		if req.Debug {
			fmt.Fprintf(os.Stderr, "Search failed, falling back: %v\n", err)
		}
		return p.suggestWithoutSearch(ctx, req)
	}

	// Phase 2: Generate suggestions with search context
	if err := p.ensureProjectID(ctx); err != nil {
		return nil, err
	}

	token, err := p.getAccessToken()
	if err != nil {
		return nil, err
	}

	numSuggestions := req.NumSuggestions
	if numSuggestions <= 0 {
		numSuggestions = 3
	}

	systemPrompt := prompt.SuggestSystemPrompt(req.Shell, req.Instructions, numSuggestions, true)

	// Include search results in the user prompt
	userPrompt := prompt.SuggestUserPrompt(req.UserInput, req.Files, req.Stdin)
	if searchContext != "" {
		userPrompt = fmt.Sprintf("%s\n\n<search_results>\n%s\n</search_results>", userPrompt, searchContext)
	}

	// Build inner request with JSON schema (no search tool - incompatible)
	requestInner := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"role": "user",
				"parts": []map[string]interface{}{
					{"text": userPrompt},
				},
			},
		},
		"systemInstruction": map[string]interface{}{
			"parts": []map[string]interface{}{
				{"text": systemPrompt},
			},
		},
		"generationConfig": map[string]interface{}{
			"responseMimeType": "application/json",
			"responseSchema":   prompt.SuggestSchema(numSuggestions),
		},
	}

	reqBody := map[string]interface{}{
		"model":          p.model,
		"project":        p.projectID,
		"user_prompt_id": fmt.Sprintf("suggest-%d", time.Now().UnixNano()),
		"request":        requestInner,
	}

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: Code Assist Request (with search) ===")
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "Project: %s\n", p.projectID)
		fmt.Fprintf(os.Stderr, "System:\n%s\n", systemPrompt)
		fmt.Fprintf(os.Stderr, "User:\n%s\n", userPrompt)
		fmt.Fprintln(os.Stderr, "=================================================")
	}

	reqJSON, _ := json.Marshal(reqBody)
	url := fmt.Sprintf("%s/%s:generateContent", codeAssistEndpoint, codeAssistAPIVersion)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqJSON))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("generateContent request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("generateContent failed with status %d: %s", resp.StatusCode, string(body))
	}

	var genResp struct {
		Response struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		} `json:"response"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&genResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if len(genResp.Response.Candidates) == 0 || len(genResp.Response.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("no response from model")
	}

	text := genResp.Response.Candidates[0].Content.Parts[0].Text

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: Code Assist Response (with search) ===")
		fmt.Fprintf(os.Stderr, "Raw JSON: %s\n", text)
		fmt.Fprintln(os.Stderr, "==================================================")
	}

	var suggestions suggestionsResponse
	if err := json.Unmarshal([]byte(text), &suggestions); err != nil {
		return nil, fmt.Errorf("failed to parse suggestions JSON: %w", err)
	}

	return suggestions.Suggestions, nil
}

func (p *CodeAssistProvider) suggestWithoutSearch(ctx context.Context, req SuggestRequest) ([]CommandSuggestion, error) {
	if err := p.ensureProjectID(ctx); err != nil {
		return nil, err
	}

	token, err := p.getAccessToken()
	if err != nil {
		return nil, err
	}

	numSuggestions := req.NumSuggestions
	if numSuggestions <= 0 {
		numSuggestions = 3
	}

	systemPrompt := prompt.SuggestSystemPrompt(req.Shell, req.Instructions, numSuggestions, false)
	userPrompt := prompt.SuggestUserPrompt(req.UserInput, req.Files, req.Stdin)

	// Build inner request
	requestInner := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"role": "user",
				"parts": []map[string]interface{}{
					{"text": userPrompt},
				},
			},
		},
		"systemInstruction": map[string]interface{}{
			"parts": []map[string]interface{}{
				{"text": systemPrompt},
			},
		},
		"generationConfig": map[string]interface{}{
			"responseMimeType": "application/json",
			"responseSchema":   prompt.SuggestSchema(numSuggestions),
		},
	}

	reqBody := map[string]interface{}{
		"model":          p.model,
		"project":        p.projectID,
		"user_prompt_id": fmt.Sprintf("suggest-%d", time.Now().UnixNano()),
		"request":        requestInner,
	}

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: Code Assist Request ===")
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "Project: %s\n", p.projectID)
		fmt.Fprintf(os.Stderr, "System:\n%s\n", systemPrompt)
		fmt.Fprintf(os.Stderr, "User:\n%s\n", userPrompt)
		fmt.Fprintln(os.Stderr, "==================================")
	}

	reqJSON, _ := json.Marshal(reqBody)
	url := fmt.Sprintf("%s/%s:generateContent", codeAssistEndpoint, codeAssistAPIVersion)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqJSON))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("generateContent request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("generateContent failed with status %d: %s", resp.StatusCode, string(body))
	}

	var genResp struct {
		Response struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		} `json:"response"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&genResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if len(genResp.Response.Candidates) == 0 || len(genResp.Response.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("no response from model")
	}

	text := genResp.Response.Candidates[0].Content.Parts[0].Text

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: Code Assist Response ===")
		fmt.Fprintf(os.Stderr, "Raw JSON: %s\n", text)
		fmt.Fprintln(os.Stderr, "===================================")
	}

	var suggestions suggestionsResponse
	if err := json.Unmarshal([]byte(text), &suggestions); err != nil {
		return nil, fmt.Errorf("failed to parse suggestions JSON: %w", err)
	}

	return suggestions.Suggestions, nil
}

func (p *CodeAssistProvider) StreamResponse(ctx context.Context, req AskRequest, output chan<- string) error {
	defer close(output)

	if err := p.ensureProjectID(ctx); err != nil {
		return err
	}

	token, err := p.getAccessToken()
	if err != nil {
		return err
	}

	userMessage := prompt.AskUserPrompt(req.Question, req.Files, req.Stdin)

	requestInner := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"role": "user",
				"parts": []map[string]interface{}{
					{"text": userMessage},
				},
			},
		},
	}

	// Add system instruction if provided
	if req.Instructions != "" {
		requestInner["systemInstruction"] = map[string]interface{}{
			"parts": []map[string]interface{}{
				{"text": req.Instructions},
			},
		}
	}

	// Add Google Search tool if search is enabled
	if req.EnableSearch {
		requestInner["tools"] = []map[string]interface{}{
			{"googleSearch": map[string]interface{}{}},
		}
	}

	reqBody := map[string]interface{}{
		"model":          p.model,
		"project":        p.projectID,
		"user_prompt_id": fmt.Sprintf("ask-%d", time.Now().UnixNano()),
		"request":        requestInner,
	}

	if req.Debug {
		fmt.Fprintln(os.Stderr, "=== DEBUG: Code Assist Stream Request ===")
		fmt.Fprintf(os.Stderr, "Provider: %s\n", p.Name())
		fmt.Fprintf(os.Stderr, "Project: %s\n", p.projectID)
		fmt.Fprintf(os.Stderr, "Question: %s\n", req.Question)
		fmt.Fprintf(os.Stderr, "Search: %v\n", req.EnableSearch)
		fmt.Fprintln(os.Stderr, "==========================================")
	}

	reqJSON, _ := json.Marshal(reqBody)
	url := fmt.Sprintf("%s/%s:streamGenerateContent?alt=sse", codeAssistEndpoint, codeAssistAPIVersion)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqJSON))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("streamGenerateContent request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("streamGenerateContent failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Track sources from grounding metadata
	type groundingSource struct {
		Title string
		URI   string
	}
	seenURIs := make(map[string]bool)
	var sources []groundingSource

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := line[6:]
		var chunk struct {
			Response struct {
				Candidates []struct {
					Content struct {
						Parts []struct {
							Text string `json:"text"`
						} `json:"parts"`
					} `json:"content"`
					GroundingMetadata *struct {
						GroundingChunks []struct {
							Web *struct {
								URI   string `json:"uri"`
								Title string `json:"title"`
							} `json:"web"`
						} `json:"groundingChunks"`
					} `json:"groundingMetadata"`
				} `json:"candidates"`
			} `json:"response"`
		}

		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if len(chunk.Response.Candidates) > 0 {
			candidate := chunk.Response.Candidates[0]

			// Extract text
			if len(candidate.Content.Parts) > 0 {
				text := candidate.Content.Parts[0].Text
				if text != "" {
					output <- text
				}
			}

			// Collect grounding sources (deduplicated)
			if candidate.GroundingMetadata != nil {
				for _, gc := range candidate.GroundingMetadata.GroundingChunks {
					if gc.Web != nil && gc.Web.URI != "" && !seenURIs[gc.Web.URI] {
						seenURIs[gc.Web.URI] = true
						sources = append(sources, groundingSource{
							Title: gc.Web.Title,
							URI:   gc.Web.URI,
						})
					}
				}
			}
		}
	}

	// Output sources at the end if we collected any
	if len(sources) > 0 {
		output <- "\n\nSources:\n"
		for i, src := range sources {
			if src.Title != "" {
				output <- fmt.Sprintf("[%d] %s (%s)\n", i+1, src.Title, src.URI)
			} else {
				output <- fmt.Sprintf("[%d] %s\n", i+1, src.URI)
			}
		}
	}

	return scanner.Err()
}
