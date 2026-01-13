package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	ReadURLToolName = "read_url"
	maxReadURLChars = 50000
)

// ReadURLTool fetches web pages using Jina AI Reader.
type ReadURLTool struct {
	client *http.Client
}

func NewReadURLTool() *ReadURLTool {
	return &ReadURLTool{
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (t *ReadURLTool) Spec() ToolSpec {
	return ReadURLToolSpec()
}

func (t *ReadURLTool) Preview(args json.RawMessage) string {
	var payload struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &payload); err != nil || payload.URL == "" {
		return ""
	}
	return payload.URL
}

// ReadURLToolSpec returns the tool spec for reading web pages.
func ReadURLToolSpec() ToolSpec {
	return ToolSpec{
		Name:        ReadURLToolName,
		Description: "Fetch and read a web page. Returns the page content as clean markdown. Use this to read full content from URLs found in search results.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url": map[string]interface{}{
					"type":        "string",
					"description": "The URL to fetch and read",
				},
			},
			"required":             []string{"url"},
			"additionalProperties": false,
		},
	}
}

func (t *ReadURLTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var payload struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return "", fmt.Errorf("parse read_url args: %w", err)
	}

	if payload.URL == "" {
		return "", fmt.Errorf("url is required")
	}

	// Ensure URL has a scheme
	url := payload.URL
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "https://" + url
	}

	// Fetch via Jina AI Reader
	jinaURL := "https://r.jina.ai/" + url

	req, err := http.NewRequestWithContext(ctx, "GET", jinaURL, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Sprintf("Error fetching URL: %v", err), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Return HTTP errors as content so LLM can handle gracefully
		statusText := http.StatusText(resp.StatusCode)
		if statusText == "" {
			statusText = "Unknown"
		}
		return fmt.Sprintf("Error: HTTP %d %s - Unable to fetch this URL.", resp.StatusCode, statusText), nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("Error reading response: %v", err), nil
	}

	content := string(body)

	// Truncate if too long
	if len(content) > maxReadURLChars {
		content = content[:maxReadURLChars] + "\n\n[Content truncated at 50,000 characters]"
	}

	return content, nil
}
