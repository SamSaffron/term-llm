package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	registryBaseURL = "https://registry.modelcontextprotocol.io"
	defaultLimit    = 50
)

// RegistryClient queries the official MCP registry.
type RegistryClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewRegistryClient creates a new registry client.
func NewRegistryClient() *RegistryClient {
	return &RegistryClient{
		baseURL: registryBaseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// RegistryServer represents a server from the registry.
type RegistryServer struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Version     string          `json:"version"`
	Repository  *RepositoryInfo `json:"repository,omitempty"`
	Packages    []PackageInfo   `json:"packages,omitempty"`
	Meta        *RegistryMeta   `json:"_meta,omitempty"`
}

// RepositoryInfo contains source repository information.
type RepositoryInfo struct {
	URL       string `json:"url"`
	Source    string `json:"source"`
	Subfolder string `json:"subfolder,omitempty"`
}

// PackageInfo describes how to install/run a server.
type PackageInfo struct {
	RegistryType    string         `json:"registryType"` // npm, pypi, oci
	RegistryBaseURL string         `json:"registryBaseUrl,omitempty"`
	Identifier      string         `json:"identifier"`
	Version         string         `json:"version"`
	Transport       *TransportInfo `json:"transport,omitempty"`
	Arguments       []ArgumentInfo `json:"packageArguments,omitempty"`
	RuntimeArgs     []ArgumentInfo `json:"runtimeArguments,omitempty"`
}

// TransportInfo describes the transport type.
type TransportInfo struct {
	Type string `json:"type"` // stdio, sse, streamable-http
	URL  string `json:"url,omitempty"`
}

// ArgumentInfo describes a command-line argument.
type ArgumentInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`   // positional, named
	Format      string `json:"format"` // string, number
	IsRequired  bool   `json:"isRequired"`
	Default     string `json:"default,omitempty"`
}

// RegistryMeta contains registry-specific metadata.
type RegistryMeta struct {
	Official *OfficialMeta `json:"io.modelcontextprotocol.registry/official,omitempty"`
}

// OfficialMeta contains official registry metadata.
type OfficialMeta struct {
	Status      string `json:"status"` // active, deleted
	PublishedAt string `json:"publishedAt"`
	UpdatedAt   string `json:"updatedAt"`
	IsLatest    bool   `json:"isLatest"`
}

// SearchResult contains the response from a registry search.
type SearchResult struct {
	Servers  []RegistryServerWrapper `json:"servers"`
	Metadata SearchMetadata          `json:"metadata"`
}

// RegistryServerWrapper wraps server with metadata.
type RegistryServerWrapper struct {
	Server RegistryServer `json:"server"`
	Meta   *RegistryMeta  `json:"_meta,omitempty"`
}

// SearchMetadata contains pagination info.
type SearchMetadata struct {
	Count      int    `json:"count"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// SearchOptions configures a registry search.
type SearchOptions struct {
	Query  string
	Limit  int
	Cursor string
}

// Search queries the registry for MCP servers.
func (r *RegistryClient) Search(ctx context.Context, opts SearchOptions) (*SearchResult, error) {
	if opts.Limit <= 0 {
		opts.Limit = defaultLimit
	}
	if opts.Limit > 100 {
		opts.Limit = 100
	}

	u, err := url.Parse(r.baseURL + "/v0/servers")
	if err != nil {
		return nil, err
	}

	q := u.Query()
	q.Set("limit", strconv.Itoa(opts.Limit))
	if opts.Query != "" {
		q.Set("search", opts.Query)
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "term-llm/1.0")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registry returned %d: %s", resp.StatusCode, string(body))
	}

	var result SearchResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("parse registry response: %w", err)
	}

	// Filter out non-active servers
	filtered := make([]RegistryServerWrapper, 0, len(result.Servers))
	for _, s := range result.Servers {
		if s.Meta != nil && s.Meta.Official != nil && s.Meta.Official.Status != "active" {
			continue
		}
		filtered = append(filtered, s)
	}
	result.Servers = filtered

	return &result, nil
}

// GetServer fetches a specific server by name.
func (r *RegistryClient) GetServer(ctx context.Context, name string) (*RegistryServer, error) {
	result, err := r.Search(ctx, SearchOptions{Query: name, Limit: 100})
	if err != nil {
		return nil, err
	}

	// Find exact match
	for _, s := range result.Servers {
		if s.Server.Name == name {
			return &s.Server, nil
		}
	}

	return nil, fmt.Errorf("server not found: %s", name)
}

// ToServerConfig converts a registry server to a local configuration.
// Returns the config and a flag indicating if user input is needed.
func (s *RegistryServer) ToServerConfig() (ServerConfig, bool) {
	cfg := ServerConfig{
		Env: make(map[string]string),
	}

	// Find the first npm package with stdio transport (most common)
	for _, pkg := range s.Packages {
		if pkg.RegistryType == "npm" {
			transport := "stdio"
			if pkg.Transport != nil {
				transport = pkg.Transport.Type
			}
			if transport != "stdio" {
				continue
			}

			cfg.Command = "npx"
			cfg.Args = []string{"-y", pkg.Identifier}

			// Add any required arguments
			needsInput := false
			for _, arg := range pkg.Arguments {
				if arg.IsRequired && arg.Default == "" {
					needsInput = true
					// Add placeholder for required args
					cfg.Args = append(cfg.Args, fmt.Sprintf("<%s>", arg.Name))
				} else if arg.Default != "" {
					cfg.Args = append(cfg.Args, arg.Default)
				}
			}

			return cfg, needsInput
		}
	}

	// Try pypi packages
	for _, pkg := range s.Packages {
		if pkg.RegistryType == "pypi" {
			transport := "stdio"
			if pkg.Transport != nil {
				transport = pkg.Transport.Type
			}
			if transport != "stdio" {
				continue
			}

			cfg.Command = "uvx"
			cfg.Args = []string{pkg.Identifier}

			needsInput := false
			for _, arg := range pkg.Arguments {
				if arg.IsRequired && arg.Default == "" {
					needsInput = true
					cfg.Args = append(cfg.Args, fmt.Sprintf("<%s>", arg.Name))
				} else if arg.Default != "" {
					cfg.Args = append(cfg.Args, arg.Default)
				}
			}

			return cfg, needsInput
		}
	}

	return cfg, true
}

// DisplayName returns a user-friendly display name for the server.
func (s *RegistryServer) DisplayName() string {
	// Use the package identifier if it looks cleaner
	for _, pkg := range s.Packages {
		if pkg.RegistryType == "npm" && pkg.Identifier != "" {
			return pkg.Identifier
		}
	}
	return s.Name
}
