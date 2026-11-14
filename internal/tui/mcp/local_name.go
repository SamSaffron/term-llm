package mcp

import (
	"strings"

	"github.com/term-llm/term-llm/internal/mcp"
)

// registryLocalName is shared by installation, installed-state checks and
// connection tests: policy always names the local mcp.json key, not a label.
func registryLocalName(server *mcp.RegistryServer) string {
	name := server.Name
	if name != "" && !strings.ContainsAny(name, "@/") {
		return name
	}
	name = server.DisplayName()
	if strings.HasPrefix(name, "@") {
		parts := strings.Split(name, "/")
		if len(parts) > 1 {
			if parts[1] == "mcp" {
				return strings.TrimPrefix(parts[0], "@")
			}
			return strings.TrimPrefix(strings.TrimSuffix(parts[1], "-mcp"), "mcp-")
		}
	}
	return name
}
