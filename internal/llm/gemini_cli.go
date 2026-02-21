package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
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

	"github.com/samsaffron/term-llm/internal/credentials"
)

// geminiCLIHTTPClient is a package-level HTTP client with a timeout to prevent
// hung Gemini API calls from blocking the event stream goroutine forever.
var geminiCLIHTTPClient = &http.Client{Timeout: 10 * time.Minute}

const (
	codeAssistEndpoint             = "https://cloudcode-pa.googleapis.com"
	codeAssistAPIVersion           = "v1internal"
	googleTokenEndpoint            = "https://oauth2.googleapis.com/token"
	codeAssistProjectCacheFile     = "project.json"
	codeAssistTokenCacheFile       = "access-token.json"
	codeAssistClientCredsCacheFile = "client-creds.json"
	codeAssistProjectCacheTTL      = 24 * time.Hour
	codeAssistTokenExpiryBuffer    = 5 * time.Minute
)

type codeAssistProjectCache struct {
	ProjectID string `json:"project_id"`
	Platform  string `json:"platform"`
	FetchedAt int64  `json:"fetched_at"`
}

type codeAssistTokenCache struct {
	AccessToken string `json:"access_token"`
	ExpiryDate  int64  `json:"expiry_date"`
	CachedAt    int64  `json:"cached_at"`
}

type codeAssistClientCredsCache struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	SourcePath   string `json:"source_path,omitempty"`
	SourceMTime  int64  `json:"source_mtime,omitempty"`
	CachedAt     int64  `json:"cached_at,omitempty"`
}

// geminiOAuthClientCreds holds the OAuth client ID and secret
type geminiOAuthClientCreds struct {
	clientID     string
	clientSecret string
}

func loadClientCredsFromCache(debug bool) (*geminiOAuthClientCreds, bool) {
	start := time.Now()
	var cache codeAssistClientCredsCache
	if err := readCodeAssistCache(codeAssistClientCredsCacheFile, &cache); err != nil {
		logTiming(debug, "client creds cache read", start, "miss")
		return nil, false
	}
	if cache.ClientID == "" || cache.ClientSecret == "" {
		logTiming(debug, "client creds cache read", start, "empty")
		return nil, false
	}

	detail := "hit"
	if cache.SourcePath != "" && cache.SourceMTime != 0 {
		info, err := os.Stat(cache.SourcePath)
		if err != nil {
			detail = "hit-source-missing"
		} else if info.ModTime().UnixMilli() != cache.SourceMTime {
			logTiming(debug, "client creds cache read", start, "stale")
			return nil, false
		}
	}

	logTiming(debug, "client creds cache read", start, detail)
	return &geminiOAuthClientCreds{
		clientID:     cache.ClientID,
		clientSecret: cache.ClientSecret,
	}, true
}

func saveClientCredsToCache(debug bool, sourcePath string, creds *geminiOAuthClientCreds) {
	if creds == nil || creds.clientID == "" || creds.clientSecret == "" {
		return
	}

	start := time.Now()
	cache := codeAssistClientCredsCache{
		ClientID:     creds.clientID,
		ClientSecret: creds.clientSecret,
		SourcePath:   sourcePath,
		CachedAt:     time.Now().UnixMilli(),
	}

	if sourcePath != "" {
		if info, err := os.Stat(sourcePath); err == nil {
			cache.SourceMTime = info.ModTime().UnixMilli()
		}
	}

	if err := writeCodeAssistCache(codeAssistClientCredsCacheFile, &cache); err != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "Cache: client creds write failed: %v\n", err)
		}
		return
	}

	logTiming(debug, "client creds cache write", start, "")
}

// getGeminiOAuthClientCreds reads the OAuth client credentials, preferring cache.
func getGeminiOAuthClientCreds(debug bool) (*geminiOAuthClientCreds, bool, error) {
	if creds, ok := loadClientCredsFromCache(debug); ok {
		return creds, true, nil
	}

	creds, err := loadGeminiOAuthClientCredsFromCLI(debug)
	return creds, false, err
}

func loadGeminiOAuthClientCredsFromCLI(debug bool) (*geminiOAuthClientCreds, error) {
	loadStart := time.Now()
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

	logTiming(debug, "client creds load", loadStart, "gemini-cli")

	// Extract client ID and secret using simple string matching
	contentStr := string(content)

	clientID := extractConstant(contentStr, "OAUTH_CLIENT_ID")
	clientSecret := extractConstant(contentStr, "OAUTH_CLIENT_SECRET")

	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("failed to extract OAuth credentials from gemini-cli")
	}

	creds := &geminiOAuthClientCreds{
		clientID:     clientID,
		clientSecret: clientSecret,
	}

	saveClientCredsToCache(debug, oauth2Path, creds)

	return creds, nil
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

func logTiming(debug bool, label string, start time.Time, detail string) {
	if !debug {
		return
	}
	elapsed := time.Since(start).Round(time.Millisecond)
	if detail != "" {
		fmt.Fprintf(os.Stderr, "Timing: %s (%s) %s\n", label, detail, elapsed)
		return
	}
	fmt.Fprintf(os.Stderr, "Timing: %s %s\n", label, elapsed)
}

func codeAssistCacheDir() (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, "term-llm", "codeassist"), nil
}

func readCodeAssistCache(fileName string, dest any) error {
	cacheDir, err := codeAssistCacheDir()
	if err != nil {
		return err
	}
	cachePath := filepath.Join(cacheDir, fileName)
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dest)
}

func writeCodeAssistCache(fileName string, src any) error {
	cacheDir, err := codeAssistCacheDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return err
	}
	data, err := json.Marshal(src)
	if err != nil {
		return err
	}
	cachePath := filepath.Join(cacheDir, fileName)
	return os.WriteFile(cachePath, data, 0600)
}

func codeAssistPlatform() string {
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
	return platform
}

// GeminiCLIProvider implements Provider using Google Code Assist API with OAuth
type GeminiCLIProvider struct {
	creds       *credentials.GeminiOAuthCredentials
	model       string
	projectID   string                  // cached after loadCodeAssist
	clientCreds *geminiOAuthClientCreds // cached OAuth client credentials
	// Tracks whether client credentials came from disk cache (so we can retry on failure).
	clientCredsFromCache bool
	thinkingLevel        string // for Gemini 3: "MINIMAL", "LOW", "HIGH"
	thinkingBudget       *int32 // for Gemini 2.5: 0, 8192, etc.
}

func NewGeminiCLIProvider(creds *credentials.GeminiOAuthCredentials, model string) *GeminiCLIProvider {
	if model == "" {
		model = "gemini-3-flash-preview"
	}
	baseModel, thinkingCfg := parseGeminiModelThinking(model)
	return &GeminiCLIProvider{
		creds:          creds,
		model:          baseModel,
		thinkingLevel:  string(thinkingCfg.level),
		thinkingBudget: thinkingCfg.budget,
	}
}

func (p *GeminiCLIProvider) Name() string {
	if p.thinkingLevel != "" {
		return fmt.Sprintf("Gemini CLI (%s, thinking=%s)", p.model, strings.ToLower(p.thinkingLevel))
	}
	if p.thinkingBudget != nil {
		return fmt.Sprintf("Gemini CLI (%s, thinkingBudget=%d)", p.model, *p.thinkingBudget)
	}
	return fmt.Sprintf("Gemini CLI (%s)", p.model)
}

func (p *GeminiCLIProvider) Credential() string {
	return "gemini-cli"
}

func (p *GeminiCLIProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch:    true,
		NativeWebFetch:     false, // No native URL fetch (Gemini-based)
		ToolCalls:          true,
		SupportsToolChoice: false, // CLI interface doesn't support forcing specific tool use
	}
}

func (p *GeminiCLIProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		if err := p.ensureProjectID(ctx, req.Debug); err != nil {
			return err
		}

		token, err := p.getAccessToken(ctx, req.Debug)
		if err != nil {
			return err
		}

		system, contents := buildGeminiCLIContents(req.Messages)
		if len(contents) == 0 {
			return fmt.Errorf("no user content provided")
		}

		requestInner := map[string]interface{}{
			"contents": contents,
		}

		if system != "" {
			requestInner["systemInstruction"] = map[string]interface{}{
				"parts": []map[string]interface{}{
					{"text": system},
				},
			}
		}

		// Add thinking config based on model generation
		// Note: Skip thinking config when search or tools are enabled (not supported together)
		if !req.Search && len(req.Tools) == 0 {
			if p.thinkingLevel != "" {
				// Gemini 3 - use thinkingLevel
				requestInner["generationConfig"] = map[string]interface{}{
					"thinkingConfig": map[string]interface{}{
						"thinkingLevel": p.thinkingLevel,
					},
				}
			} else if p.thinkingBudget != nil {
				// Gemini 2.5 - use thinkingBudget
				requestInner["generationConfig"] = map[string]interface{}{
					"thinkingConfig": map[string]interface{}{
						"thinkingBudget": *p.thinkingBudget,
					},
				}
			}
		}

		if req.Search {
			requestInner["tools"] = []map[string]interface{}{
				{"googleSearch": map[string]interface{}{}},
			}
		}

		if len(req.Tools) > 0 {
			decls := make([]map[string]interface{}, 0, len(req.Tools))
			for _, spec := range req.Tools {
				// Normalize schema for Gemini's requirements (same as GeminiProvider)
				schema := normalizeSchemaForGemini(spec.Schema)
				decls = append(decls, map[string]interface{}{
					"name":        spec.Name,
					"description": spec.Description,
					"parameters":  schema,
				})
			}
			requestInner["tools"] = []map[string]interface{}{
				{"functionDeclarations": decls},
			}
			requestInner["toolConfig"] = map[string]interface{}{
				"functionCallingConfig": map[string]interface{}{"mode": "AUTO"},
			}
		}

		reqBody := map[string]interface{}{
			"model":          chooseModel(req.Model, p.model),
			"project":        p.projectID,
			"user_prompt_id": fmt.Sprintf("stream-%d", time.Now().UnixNano()),
			"request":        requestInner,
		}

		reqJSON, _ := json.Marshal(reqBody)

		if len(req.Tools) > 0 {
			reqURL := fmt.Sprintf("%s/%s:generateContent", codeAssistEndpoint, codeAssistAPIVersion)
			httpReq, err := http.NewRequestWithContext(ctx, "POST", reqURL, bytes.NewReader(reqJSON))
			if err != nil {
				return err
			}
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("Authorization", "Bearer "+token)

			resp, err := geminiCLIHTTPClient.Do(httpReq)
			if err != nil {
				return fmt.Errorf("generateContent request failed: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != 200 {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("generateContent failed with status %d: %s", resp.StatusCode, string(body))
			}

			var genResp struct {
				Response struct {
					Candidates []struct {
						Content struct {
							Parts []struct {
								Text             string `json:"text,omitempty"`
								Thought          bool   `json:"thought,omitempty"`
								ThoughtSignature string `json:"thoughtSignature,omitempty"`
								FunctionCall     *struct {
									ID   string                 `json:"id"`
									Name string                 `json:"name"`
									Args map[string]interface{} `json:"args"`
								} `json:"functionCall,omitempty"`
							} `json:"parts"`
						} `json:"content"`
					} `json:"candidates"`
				} `json:"response"`
			}

			if err := json.NewDecoder(resp.Body).Decode(&genResp); err != nil {
				return fmt.Errorf("failed to parse response: %w", err)
			}

			// Extract text and function calls with thought signatures from Parts
			// Gemini 3 returns thought signature that must be passed back with tool results
			var lastThoughtSig []byte
			for _, cand := range genResp.Response.Candidates {
				for _, part := range cand.Content.Parts {
					// Capture thought signature from thought parts
					if part.Thought && part.ThoughtSignature != "" {
						lastThoughtSig, _ = base64.StdEncoding.DecodeString(part.ThoughtSignature)
					}
					// Emit text parts (skip thought parts which are internal)
					if part.Text != "" && !part.Thought {
						events <- Event{Type: EventTextDelta, Text: part.Text}
					}
					if part.FunctionCall != nil {
						argsJSON, _ := json.Marshal(part.FunctionCall.Args)
						// Use thought signature from this part or preceding thought part
						var thoughtSig []byte
						if part.ThoughtSignature != "" {
							thoughtSig, _ = base64.StdEncoding.DecodeString(part.ThoughtSignature)
						} else {
							thoughtSig = lastThoughtSig
						}
						call := ToolCall{
							ID:         part.FunctionCall.ID,
							Name:       part.FunctionCall.Name,
							Arguments:  argsJSON,
							ThoughtSig: thoughtSig,
						}
						events <- Event{Type: EventToolCall, Tool: &call}
					}
				}
			}
			events <- Event{Type: EventDone}
			return nil
		}

		reqURL := fmt.Sprintf("%s/%s:streamGenerateContent?alt=sse", codeAssistEndpoint, codeAssistAPIVersion)
		httpReq, err := http.NewRequestWithContext(ctx, "POST", reqURL, bytes.NewReader(reqJSON))
		if err != nil {
			return err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+token)

		streamStart := time.Now()
		resp, err := geminiCLIHTTPClient.Do(httpReq)
		if err != nil {
			return fmt.Errorf("streamGenerateContent request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("streamGenerateContent failed with status %d: %s", resp.StatusCode, string(body))
		}

		type groundingSource struct {
			Title string
			URI   string
		}
		seenURIs := make(map[string]bool)
		var sources []groundingSource

		scanner := bufio.NewScanner(resp.Body)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024) // 1MB max line size
		firstChunkLogged := false
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}

			if !firstChunkLogged {
				firstChunkLogged = true
				logTiming(req.Debug, "streamGenerateContent first chunk", streamStart, "")
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
				if len(candidate.Content.Parts) > 0 {
					text := candidate.Content.Parts[0].Text
					if text != "" {
						events <- Event{Type: EventTextDelta, Text: text}
					}
				}

				if candidate.GroundingMetadata != nil {
					for _, gc := range candidate.GroundingMetadata.GroundingChunks {
						if gc.Web != nil && gc.Web.URI != "" && !seenURIs[gc.Web.URI] {
							seenURIs[gc.Web.URI] = true
							sources = append(sources, groundingSource{Title: gc.Web.Title, URI: gc.Web.URI})
						}
					}
				}
			}
		}

		if len(sources) > 0 {
			events <- Event{Type: EventTextDelta, Text: "\n\nSources:\n"}
			for i, src := range sources {
				label := src.URI
				if src.Title != "" {
					label = fmt.Sprintf("%s (%s)", src.Title, src.URI)
				}
				events <- Event{Type: EventTextDelta, Text: fmt.Sprintf("[%d] %s\n", i+1, label)}
			}
		}

		if firstChunkLogged {
			logTiming(req.Debug, "streamGenerateContent total", streamStart, "")
		} else {
			logTiming(req.Debug, "streamGenerateContent total", streamStart, "no chunks")
		}

		if err := scanner.Err(); err != nil {
			return err
		}

		events <- Event{Type: EventDone}
		return nil
	}), nil
}

// getAccessToken returns a valid access token, refreshing if needed
func (p *GeminiCLIProvider) getAccessToken(ctx context.Context, debug bool) (string, error) {
	now := time.Now().UnixMilli()
	bufferMs := int64(codeAssistTokenExpiryBuffer.Milliseconds())

	if p.creds.AccessToken != "" && (p.creds.ExpiryDate == 0 || now < p.creds.ExpiryDate-bufferMs) {
		logTiming(debug, "access token", time.Now(), "memory")
		return p.creds.AccessToken, nil
	}

	if p.loadAccessTokenFromCache(debug) {
		return p.creds.AccessToken, nil
	}

	// Ensure we have client credentials for refresh
	if p.clientCreds == nil {
		creds, fromCache, err := getGeminiOAuthClientCreds(debug)
		if err != nil {
			return "", fmt.Errorf("failed to get OAuth client credentials: %w", err)
		}
		p.clientCreds = creds
		p.clientCredsFromCache = fromCache
	}

	token, expiryDate, err := p.refreshAccessToken(ctx, debug, "")
	if err != nil && p.clientCredsFromCache {
		if debug {
			fmt.Fprintln(os.Stderr, "Cache: client creds refresh failed, reloading from gemini-cli")
		}
		creds, loadErr := loadGeminiOAuthClientCredsFromCLI(debug)
		if loadErr == nil {
			p.clientCreds = creds
			p.clientCredsFromCache = false
			token, expiryDate, err = p.refreshAccessToken(ctx, debug, "retry")
		}
	}
	if err != nil {
		return "", err
	}

	p.creds.AccessToken = token
	p.creds.ExpiryDate = expiryDate
	p.saveAccessTokenToCache(debug)

	return token, nil
}

func (p *GeminiCLIProvider) refreshAccessToken(ctx context.Context, debug bool, detail string) (string, int64, error) {
	refreshStart := time.Now()
	data := url.Values{}
	data.Set("client_id", p.clientCreds.clientID)
	data.Set("client_secret", p.clientCreds.clientSecret)
	data.Set("refresh_token", p.creds.RefreshToken)
	data.Set("grant_type", "refresh_token")

	tokenReq, err := http.NewRequestWithContext(ctx, "POST", googleTokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("failed to create token refresh request: %w", err)
	}
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := geminiCLIHTTPClient.Do(tokenReq)
	if err != nil {
		return "", 0, fmt.Errorf("failed to refresh token: %w", err)
	}
	defer resp.Body.Close()

	logTiming(debug, "token refresh", refreshStart, detail)

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", 0, fmt.Errorf("token refresh failed: %s", string(body))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", 0, fmt.Errorf("failed to parse token response: %w", err)
	}

	expiryDate := time.Now().UnixMilli() + int64(tokenResp.ExpiresIn)*1000
	return tokenResp.AccessToken, expiryDate, nil
}

func (p *GeminiCLIProvider) loadAccessTokenFromCache(debug bool) bool {
	start := time.Now()
	var cache codeAssistTokenCache
	if err := readCodeAssistCache(codeAssistTokenCacheFile, &cache); err != nil {
		logTiming(debug, "access token cache read", start, "miss")
		return false
	}
	if cache.AccessToken == "" || cache.ExpiryDate == 0 {
		logTiming(debug, "access token cache read", start, "empty")
		return false
	}

	bufferMs := int64(codeAssistTokenExpiryBuffer.Milliseconds())
	now := time.Now().UnixMilli()
	if now >= cache.ExpiryDate-bufferMs {
		logTiming(debug, "access token cache read", start, "expired")
		return false
	}

	p.creds.AccessToken = cache.AccessToken
	p.creds.ExpiryDate = cache.ExpiryDate
	logTiming(debug, "access token cache read", start, "hit")
	return true
}

func (p *GeminiCLIProvider) saveAccessTokenToCache(debug bool) {
	if p.creds.AccessToken == "" || p.creds.ExpiryDate == 0 {
		return
	}

	start := time.Now()
	cache := codeAssistTokenCache{
		AccessToken: p.creds.AccessToken,
		ExpiryDate:  p.creds.ExpiryDate,
		CachedAt:    time.Now().UnixMilli(),
	}
	if err := writeCodeAssistCache(codeAssistTokenCacheFile, &cache); err != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "Cache: access token write failed: %v\n", err)
		}
		return
	}
	logTiming(debug, "access token cache write", start, "")
}

func (p *GeminiCLIProvider) loadProjectIDFromCache(debug bool, platform string) bool {
	start := time.Now()
	var cache codeAssistProjectCache
	if err := readCodeAssistCache(codeAssistProjectCacheFile, &cache); err != nil {
		logTiming(debug, "project ID cache read", start, "miss")
		return false
	}
	if cache.ProjectID == "" {
		logTiming(debug, "project ID cache read", start, "empty")
		return false
	}
	if cache.Platform != "" && cache.Platform != platform {
		logTiming(debug, "project ID cache read", start, "platform-mismatch")
		return false
	}
	if cache.FetchedAt == 0 || time.Since(time.UnixMilli(cache.FetchedAt)) > codeAssistProjectCacheTTL {
		logTiming(debug, "project ID cache read", start, "expired")
		return false
	}

	p.projectID = cache.ProjectID
	logTiming(debug, "project ID cache read", start, "hit")
	return true
}

func (p *GeminiCLIProvider) saveProjectIDToCache(debug bool, platform string) {
	if p.projectID == "" {
		return
	}

	start := time.Now()
	cache := codeAssistProjectCache{
		ProjectID: p.projectID,
		Platform:  platform,
		FetchedAt: time.Now().UnixMilli(),
	}
	if err := writeCodeAssistCache(codeAssistProjectCacheFile, &cache); err != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "Cache: project ID write failed: %v\n", err)
		}
		return
	}
	logTiming(debug, "project ID cache write", start, "")
}

// ensureProjectID calls loadCodeAssist to get the project ID if not already cached
func (p *GeminiCLIProvider) ensureProjectID(ctx context.Context, debug bool) error {
	if p.projectID != "" {
		return nil
	}

	platform := codeAssistPlatform()
	if p.loadProjectIDFromCache(debug, platform) {
		return nil
	}

	token, err := p.getAccessToken(ctx, debug)
	if err != nil {
		return err
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

	loadStart := time.Now()
	resp, err := geminiCLIHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("loadCodeAssist request failed: %w", err)
	}
	defer resp.Body.Close()

	logTiming(debug, "loadCodeAssist request", loadStart, "")

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
	p.saveProjectIDToCache(debug, platform)
	return nil
}

// performSearch performs a Google Search query and returns the search context
func (p *GeminiCLIProvider) performSearch(ctx context.Context, query string, debug bool) (string, error) {
	if err := p.ensureProjectID(ctx, debug); err != nil {
		return "", err
	}

	token, err := p.getAccessToken(ctx, debug)
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

	resp, err := geminiCLIHTTPClient.Do(httpReq)
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

// buildGeminiCLIContents builds the contents array for the Code Assist API from messages.
// Returns system instruction text and contents array.
// Note: Consecutive RoleTool messages are merged into a single content block as required by Gemini API.
func buildGeminiCLIContents(messages []Message) (string, []map[string]interface{}) {
	messages = sanitizeToolHistory(messages)

	var systemParts []string
	contents := make([]map[string]interface{}, 0, len(messages))

	for i := 0; i < len(messages); i++ {
		msg := messages[i]
		switch msg.Role {
		case RoleSystem:
			if text := collectTextParts(msg.Parts); text != "" {
				systemParts = append(systemParts, text)
			}
		case RoleUser:
			content := buildGeminiCLIUserContent(msg.Parts)
			if content != nil {
				contents = append(contents, content)
			}
		case RoleAssistant:
			content := buildGeminiCLIModelContent(msg.Parts)
			if content != nil {
				contents = append(contents, content)
			}
		case RoleTool:
			// Collect all consecutive RoleTool messages into a single content block
			// This is required by Gemini API - all function responses for a turn must be together
			var allToolParts []Part
			for ; i < len(messages) && messages[i].Role == RoleTool; i++ {
				allToolParts = append(allToolParts, messages[i].Parts...)
			}
			i-- // Adjust for the outer loop increment
			content := buildGeminiCLIToolResultContent(allToolParts)
			if content != nil {
				contents = append(contents, content)
			}
		}
	}

	return strings.Join(systemParts, "\n\n"), contents
}

func buildGeminiCLIUserContent(parts []Part) map[string]interface{} {
	apiParts := make([]map[string]interface{}, 0)
	for _, part := range parts {
		switch part.Type {
		case PartText:
			if part.Text != "" {
				apiParts = append(apiParts, map[string]interface{}{"text": part.Text})
			}
		case PartImage:
			if part.ImageData != nil {
				apiParts = append(apiParts, map[string]interface{}{
					"inline_data": map[string]interface{}{
						"mime_type": part.ImageData.MediaType,
						"data":      part.ImageData.Base64,
					},
				})
			}
		}
	}
	if len(apiParts) == 0 {
		return nil
	}
	return map[string]interface{}{
		"role":  "user",
		"parts": apiParts,
	}
}

func buildGeminiCLIModelContent(parts []Part) map[string]interface{} {
	apiParts := make([]map[string]interface{}, 0)
	for _, part := range parts {
		switch part.Type {
		case PartText:
			if part.Text != "" {
				apiParts = append(apiParts, map[string]interface{}{"text": part.Text})
			}
		case PartToolCall:
			if part.ToolCall != nil {
				args := toolArgsToMap(part.ToolCall.Arguments)
				fcPart := map[string]interface{}{
					"functionCall": map[string]interface{}{
						"id":   part.ToolCall.ID,
						"name": part.ToolCall.Name,
						"args": args,
					},
				}
				// Include thoughtSignature (required for Gemini 3 thinking models)
				// Use synthetic signature if not present to skip validation
				if len(part.ToolCall.ThoughtSig) > 0 {
					fcPart["thoughtSignature"] = base64.StdEncoding.EncodeToString(part.ToolCall.ThoughtSig)
				} else {
					fcPart["thoughtSignature"] = "skip_thought_signature_validator"
				}
				apiParts = append(apiParts, fcPart)
			}
		}
	}
	if len(apiParts) == 0 {
		return nil
	}
	return map[string]interface{}{
		"role":  "model",
		"parts": apiParts,
	}
}

func buildGeminiCLIToolResultContent(parts []Part) map[string]interface{} {
	apiParts := make([]map[string]interface{}, 0)
	for _, part := range parts {
		switch part.Type {
		case PartText:
			if part.Text != "" {
				apiParts = append(apiParts, map[string]interface{}{"text": part.Text})
			}
		case PartToolResult:
			if part.ToolResult != nil {
				textContent := toolResultTextContent(part.ToolResult)

				// Add the function response with text content
				// Include ThoughtSignature if present (required for Gemini 3 thinking models)
				frPart := map[string]interface{}{
					"functionResponse": map[string]interface{}{
						"name":     part.ToolResult.Name,
						"response": map[string]interface{}{"output": textContent},
					},
				}
				// Include thoughtSignature (required for Gemini 3 thinking models)
				// Use synthetic signature if not present to skip validation
				if len(part.ToolResult.ThoughtSig) > 0 {
					frPart["thoughtSignature"] = base64.StdEncoding.EncodeToString(part.ToolResult.ThoughtSig)
				} else {
					frPart["thoughtSignature"] = "skip_thought_signature_validator"
				}
				apiParts = append(apiParts, frPart)

				for _, contentPart := range toolResultContentParts(part.ToolResult) {
					mimeType, base64Data, ok := toolResultImageData(contentPart)
					if !ok {
						continue
					}
					apiParts = append(apiParts, map[string]interface{}{
						"inlineData": map[string]interface{}{
							"mimeType": mimeType,
							"data":     base64Data,
						},
					})
				}
			}
		}
	}
	if len(apiParts) == 0 {
		return nil
	}
	return map[string]interface{}{
		"role":  "user",
		"parts": apiParts,
	}
}
