package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultExaMCPURL       = "https://mcp.exa.ai/mcp"
	exaMCPSearchTool       = "web_search_exa"
	exaMCPFetchTool        = "web_fetch_exa"
	exaMCPDefaultMaxChars  = 50000
	exaMCPTruncationSuffix = "\n\n[Content truncated at 50,000 characters]"
)

// ExaMCPClient implements search and fetch using Exa's remote MCP server.
type ExaMCPClient struct {
	url    string
	apiKey string
}

func NewExaMCPClient(url, apiKey string) *ExaMCPClient {
	if strings.TrimSpace(url) == "" {
		url = defaultExaMCPURL
	}
	return &ExaMCPClient{url: url, apiKey: apiKey}
}

func (e *ExaMCPClient) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("empty query")
	}
	if maxResults <= 0 {
		maxResults = 10
	}
	if maxResults > 20 {
		maxResults = 20
	}

	args, err := json.Marshal(map[string]any{
		"query":      query,
		"numResults": maxResults,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal exa mcp search args: %w", err)
	}

	out, err := e.callTool(ctx, exaMCPSearchTool, args)
	if err != nil {
		return nil, err
	}
	return parseExaMCPSearchResults(out, maxResults), nil
}

// FetchURL fetches a URL using Exa MCP's web_fetch_exa tool.
func (e *ExaMCPClient) FetchURL(ctx context.Context, url string) (string, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return "", fmt.Errorf("url is required")
	}

	args, err := json.Marshal(map[string]any{
		"urls":          []string{url},
		"maxCharacters": exaMCPDefaultMaxChars,
	})
	if err != nil {
		return "", fmt.Errorf("marshal exa mcp fetch args: %w", err)
	}

	out, err := e.callTool(ctx, exaMCPFetchTool, args)
	if err != nil {
		return "", err
	}
	if len([]rune(out)) > exaMCPDefaultMaxChars {
		runes := []rune(out)
		out = string(runes[:exaMCPDefaultMaxChars]) + exaMCPTruncationSuffix
	}
	return out, nil
}

func (e *ExaMCPClient) callTool(ctx context.Context, tool string, args json.RawMessage) (string, error) {
	arguments := map[string]any{}
	if err := json.Unmarshal(args, &arguments); err != nil {
		return "", fmt.Errorf("invalid exa mcp tool arguments: %w", err)
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	if e.apiKey != "" {
		httpClient.Transport = &exaMCPHeaderTransport{
			base:    http.DefaultTransport,
			headers: map[string]string{"x-api-key": e.apiKey},
		}
	}

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "term-llm", Version: "1.0.0"}, nil)
	transport := &mcpsdk.StreamableClientTransport{
		Endpoint:   e.url,
		HTTPClient: httpClient,
		MaxRetries: 5,
	}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return "", fmt.Errorf("connect to exa mcp: %w", err)
	}
	defer session.Close()

	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: tool, Arguments: arguments})
	if err != nil {
		return "", fmt.Errorf("call exa mcp %s: %w", tool, err)
	}
	if result.IsError {
		return "", fmt.Errorf("exa mcp %s returned error: %s", tool, formatExaMCPContent(result.Content))
	}
	return formatExaMCPContent(result.Content), nil
}

type exaMCPHeaderTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *exaMCPHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	return t.base.RoundTrip(req)
}

func formatExaMCPContent(content []mcpsdk.Content) string {
	var sb strings.Builder
	for _, c := range content {
		switch v := c.(type) {
		case *mcpsdk.TextContent:
			sb.WriteString(v.Text)
		default:
			if data, err := json.Marshal(c); err == nil {
				sb.WriteString(string(data))
			}
		}
	}
	return sb.String()
}

func parseExaMCPSearchResults(output string, maxResults int) []Result {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil
	}

	blocks := strings.Split(output, "\n---")
	results := make([]Result, 0, len(blocks))
	for _, block := range blocks {
		result := parseExaMCPResultBlock(block)
		if result.Title == "" || result.URL == "" {
			continue
		}
		results = append(results, result)
		if len(results) >= maxResults {
			break
		}
	}
	return results
}

func parseExaMCPResultBlock(block string) Result {
	var r Result
	var snippet strings.Builder
	inHighlights := false

	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "Title:"):
			r.Title = strings.TrimSpace(strings.TrimPrefix(trimmed, "Title:"))
			inHighlights = false
		case strings.HasPrefix(trimmed, "URL:"):
			r.URL = strings.TrimSpace(strings.TrimPrefix(trimmed, "URL:"))
			inHighlights = false
		case strings.HasPrefix(trimmed, "Highlights:"):
			inHighlights = true
		case inHighlights && trimmed != "" && !strings.HasPrefix(trimmed, "Published:") && !strings.HasPrefix(trimmed, "Author:"):
			if snippet.Len() > 0 {
				snippet.WriteString(" ")
			}
			snippet.WriteString(trimmed)
		}
	}

	r.Snippet = strings.TrimSpace(snippet.String())
	if len([]rune(r.Snippet)) > 500 {
		runes := []rune(r.Snippet)
		r.Snippet = string(runes[:500]) + "..."
	}
	return r
}
