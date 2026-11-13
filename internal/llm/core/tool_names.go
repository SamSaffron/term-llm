package core

import "strings"

// Provider-neutral names of built-in tools the engine treats specially.
const (
	SuggestCommandsToolName = "suggest_commands"
	WebSearchToolName       = "web_search"
	ReadURLToolName         = "read_url"
)

// WebMCPToolPrefix is reserved for tools explicitly offered by a connected
// page/client response. It must not be claimed by a configured MCP server
// namespace, agent custom tool, or skill script tool.
const WebMCPToolPrefix = "webmcp__"

// IsWebMCPToolName reports whether name belongs to the reserved client namespace.
func IsWebMCPToolName(name string) bool { return strings.HasPrefix(name, WebMCPToolPrefix) }
