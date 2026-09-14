package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/mcp"
)

// MCPCheck validates mcp.json without starting any server.
type MCPCheck struct {
	// Path overrides the mcp.json location. Empty uses the default.
	Path string
}

// ID implements Check.
func (c *MCPCheck) ID() string { return "mcp" }

// Title implements Check.
func (c *MCPCheck) Title() string { return "MCP servers" }

// Run implements Check.
func (c *MCPCheck) Run(ctx context.Context) []Finding {
	path := c.Path
	if path == "" {
		resolved, err := mcp.DefaultConfigPath()
		if err != nil {
			return []Finding{{Title: "cannot locate mcp.json", Severity: SeverityError, Detail: err.Error()}}
		}
		path = resolved
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Finding{{Title: "no mcp.json", Severity: SeverityInfo, Detail: path + " does not exist"}}
		}
		return []Finding{{Title: "cannot read mcp.json", Severity: SeverityError, Detail: err.Error()}}
	}

	cfg, err := mcp.LoadConfigFromPath(path)
	if err != nil {
		return []Finding{{
			Title:    "mcp.json is not valid JSON",
			Severity: SeverityError,
			Detail:   err.Error(),
			Remedy:   "every MCP server is unavailable until this parses",
		}}
	}

	var findings []Finding
	for _, name := range duplicateServerNames(data) {
		findings = append(findings, Finding{
			Title:    fmt.Sprintf("mcp.json defines server %q more than once", name),
			Severity: SeverityWarn,
			Detail:   "JSON objects cannot hold duplicate keys; only the last definition is used",
			Remedy:   "delete the shadowed definition",
		})
	}

	names := cfg.ServerNames()
	sort.Strings(names)
	for _, name := range names {
		server := cfg.Servers[name]
		if err := server.Validate(); err != nil {
			findings = append(findings, Finding{
				Title:    fmt.Sprintf("MCP server %q is misconfigured", name),
				Severity: SeverityError,
				Detail:   err.Error(),
			})
			continue
		}
		if server.TransportType() != "stdio" {
			if !strings.HasPrefix(server.URL, "http://") && !strings.HasPrefix(server.URL, "https://") {
				findings = append(findings, Finding{
					Title:    fmt.Sprintf("MCP server %q has a non-HTTP url", name),
					Severity: SeverityWarn,
					Detail:   server.URL,
				})
			}
			continue
		}
		if _, err := exec.LookPath(server.Command); err != nil {
			findings = append(findings, Finding{
				Title:    fmt.Sprintf("MCP server %q cannot start: %q is not on PATH", name, server.Command),
				Severity: SeverityError,
				Detail:   err.Error(),
				Remedy:   "install the command, use an absolute path, or remove the server from mcp.json",
			})
		}
	}
	return findings
}

// duplicateServerNames reports server names defined more than once. The loader
// decodes into a map, so duplicates are silently collapsed to the last entry.
func duplicateServerNames(data []byte) []string {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil
		}
		key, _ := keyToken.(string)
		if key != "servers" {
			var skip json.RawMessage
			if err := decoder.Decode(&skip); err != nil {
				return nil
			}
			continue
		}
		return duplicateObjectKeys(decoder)
	}
	return nil
}

func duplicateObjectKeys(decoder *json.Decoder) []string {
	token, err := decoder.Token()
	if err != nil {
		return nil
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil
	}
	seen := map[string]bool{}
	duplicates := map[string]bool{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil
		}
		key, _ := keyToken.(string)
		if seen[key] {
			duplicates[key] = true
		}
		seen[key] = true
		var skip json.RawMessage
		if err := decoder.Decode(&skip); err != nil {
			return nil
		}
	}
	out := make([]string, 0, len(duplicates))
	for key := range duplicates {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
