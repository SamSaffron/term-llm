package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

const (
	skillsmpBaseURL = "https://skillsmp.com/api/v1"
	defaultTimeout  = 30 * time.Second
)

// RemoteRegistryClient queries the SkillsMP API for skills.
type RemoteRegistryClient struct {
	baseURL    string
	httpClient *http.Client
	apiKey     string
}

// RemoteSkill represents a skill from the SkillsMP registry.
type RemoteSkill struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Author      string  `json:"author"`
	Repository  string  `json:"githubUrl"` // GitHub URL
	Category    string  `json:"category"`
	Downloads   int     `json:"downloads"`
	Stars       int     `json:"stars"`
	URL         string  `json:"skillUrl"` // SkillsMP page URL
	RawURL      string  `json:"rawUrl"`   // Direct SKILL.md URL
	License     string  `json:"license"`
	UpdatedAt   float64 `json:"updatedAt"` // Unix timestamp
}

// RemoteSearchResult contains the response from a skill search.
type RemoteSearchResult struct {
	Success bool `json:"success"`
	Data    struct {
		Skills []RemoteSkill `json:"skills"`
		Total  int           `json:"total"`
		Page   int           `json:"page"`
	} `json:"data"`
	// Flattened for convenience
	Skills []RemoteSkill `json:"-"`
}

// NewRemoteRegistryClient creates a new SkillsMP registry client.
func NewRemoteRegistryClient() *RemoteRegistryClient {
	apiKey := os.Getenv("SKILLSMP_API_KEY")

	return &RemoteRegistryClient{
		baseURL: skillsmpBaseURL,
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
		apiKey: apiKey,
	}
}

// NewRemoteRegistryClientWithKey creates a client with an explicit API key.
func NewRemoteRegistryClientWithKey(apiKey string) *RemoteRegistryClient {
	client := NewRemoteRegistryClient()
	if apiKey != "" {
		client.apiKey = apiKey
	}
	return client
}

// Search performs a keyword search for skills.
func (r *RemoteRegistryClient) Search(ctx context.Context, query string) (*RemoteSearchResult, error) {
	return r.doSearch(ctx, "/skills/search", query)
}

// AISearch performs an AI-powered semantic search for skills.
func (r *RemoteRegistryClient) AISearch(ctx context.Context, query string) (*RemoteSearchResult, error) {
	return r.doSearch(ctx, "/skills/ai-search", query)
}

func (r *RemoteRegistryClient) doSearch(ctx context.Context, endpoint, query string) (*RemoteSearchResult, error) {
	u, err := url.Parse(r.baseURL + endpoint)
	if err != nil {
		return nil, err
	}

	q := u.Query()
	if query != "" {
		q.Set("q", query)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "term-llm/1.0")

	// Add Bearer token if API key is set
	if r.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.apiKey)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("skills registry request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("API key required: set SKILLSMP_API_KEY environment variable or configure skills.api_key")
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("skills registry returned %d: %s", resp.StatusCode, string(body))
	}

	// Read the body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	// Check for API error in response
	var apiError struct {
		Success bool `json:"success"`
		Error   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &apiError); err == nil && !apiError.Success && apiError.Error.Code != "" {
		if apiError.Error.Code == "MISSING_API_KEY" || apiError.Error.Code == "INVALID_API_KEY" {
			return nil, fmt.Errorf("API key required: set SKILLSMP_API_KEY environment variable or configure skills.api_key")
		}
		return nil, fmt.Errorf("API error: %s - %s", apiError.Error.Code, apiError.Error.Message)
	}

	var result RemoteSearchResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse skills registry response: %w", err)
	}

	// Flatten the data structure for easier access
	result.Skills = result.Data.Skills

	return &result, nil
}

// GetSkill fetches details for a specific skill by name.
func (r *RemoteRegistryClient) GetSkill(ctx context.Context, name string) (*RemoteSkill, error) {
	result, err := r.Search(ctx, name)
	if err != nil {
		return nil, err
	}

	// Find exact match
	for _, s := range result.Skills {
		if s.Name == name {
			return &s, nil
		}
	}

	return nil, fmt.Errorf("skill not found: %s", name)
}

// DownloadSkill downloads a skill's SKILL.md content from its raw URL.
func (r *RemoteRegistryClient) DownloadSkill(ctx context.Context, skill *RemoteSkill) ([]byte, error) {
	if skill.RawURL == "" {
		// Try to construct raw URL from repository
		if skill.Repository == "" {
			return nil, fmt.Errorf("no download URL available for skill: %s", skill.Name)
		}
		// Assume GitHub and construct raw URL
		// e.g., https://github.com/user/repo -> https://raw.githubusercontent.com/user/repo/main/SKILL.md
		skill.RawURL = constructRawGitHubURL(skill.Repository)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", skill.RawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "term-llm/1.0")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download skill failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download skill returned %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

// constructRawGitHubURL converts a GitHub repo URL to a raw content URL.
func constructRawGitHubURL(repoURL string) string {
	// Handle various GitHub URL formats
	// https://github.com/user/repo -> https://raw.githubusercontent.com/user/repo/main/SKILL.md
	// https://github.com/user/repo/tree/main/path -> https://raw.githubusercontent.com/user/repo/main/path/SKILL.md

	u, err := url.Parse(repoURL)
	if err != nil {
		return repoURL + "/SKILL.md"
	}

	if u.Host == "github.com" {
		// Convert to raw.githubusercontent.com
		path := u.Path
		if len(path) > 0 && path[0] == '/' {
			path = path[1:]
		}

		// Check if it contains /tree/ or /blob/
		if idx := findSubstring(path, "/tree/"); idx != -1 {
			// Extract user/repo and the rest of the path
			parts := path[:idx]
			rest := path[idx+6:] // Skip "/tree/"
			return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/SKILL.md", parts, rest)
		}

		// Simple repo URL - assume main branch
		return fmt.Sprintf("https://raw.githubusercontent.com/%s/main/SKILL.md", path)
	}

	return repoURL + "/SKILL.md"
}

func findSubstring(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// HasAPIKey returns whether an API key is configured.
func (r *RemoteRegistryClient) HasAPIKey() bool {
	return r.apiKey != ""
}

// FetchRawURL fetches content from a raw URL (for updates).
func (r *RemoteRegistryClient) FetchRawURL(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "term-llm/1.0")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch returned %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}
