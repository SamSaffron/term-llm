// Package buildinfo exposes application identity to packages below the CLI.
package buildinfo

import "strings"

// Version is initialized by the CLI at startup from its linker-provided version.
// Libraries and development builds default to dev.
var Version = "dev"

// UserAgent identifies the application without claiming a provider's version.
func UserAgent() string {
	version := strings.TrimPrefix(strings.TrimSpace(Version), "v")
	if version == "" || strings.ContainsAny(version, " \t\r\n") {
		version = "dev"
	}
	return "term-llm/" + version
}
