package mcpstart

import (
	"fmt"
	"io"
	"strings"

	"github.com/samsaffron/term-llm/internal/mcp"
)

// CleanSelection validates explicit CLI selections but repairs implicit defaults
// and restored selections. The manager has already loaded its configuration.
func CleanSelection(manager *mcp.Manager, selection string, explicit bool, warnings io.Writer) (string, error) {
	names := ParseServerList(selection)
	if explicit {
		if err := manager.ValidateSelection(names); err != nil {
			return "", err
		}
		return strings.Join(NormalizeSelection(names), ","), nil
	}
	available := make(map[string]bool)
	for _, name := range manager.AvailableServers() {
		available[name] = true
	}
	var kept []string
	for _, name := range names {
		if available[name] {
			kept = append(kept, name)
		} else if warnings != nil {
			fmt.Fprintf(warnings, "warning: dropping MCP server %q: unavailable or not permitted for this agent\n", name)
		}
	}
	return strings.Join(NormalizeSelection(kept), ","), nil
}
