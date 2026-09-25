package mcp

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"
)

var validServerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidateServerName checks a name for use as an MCP tool namespace.
func ValidateServerName(name string) error {
	if !validServerName.MatchString(name) || strings.Contains(name, "__") {
		return fmt.Errorf("invalid MCP server name %q", name)
	}
	return nil
}

func cleanServerName(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name = strings.Trim(b.String(), "-._")
	name = strings.ReplaceAll(name, "__", "-")
	if len(name) > 64 {
		name = strings.TrimRight(name[:64], "-._")
	}
	if name == "" {
		return "server"
	}
	return name
}

// DeriveNameFromURL suggests a local server name from the URL hostname and path.
func DeriveNameFromURL(u *url.URL) string {
	name := u.Hostname()
	for _, prefix := range []string{"www.", "api.", "mcp."} {
		name = strings.TrimPrefix(name, prefix)
	}
	for _, suffix := range []string{".com", ".io", ".ai", ".dev"} {
		name = strings.TrimSuffix(name, suffix)
	}
	name = strings.ReplaceAll(name, ".", "-")
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if parts[0] != "" && parts[0] != "mcp" {
		name += "-" + parts[0]
	}
	return cleanServerName(name)
}

// SplitCommandLine splits a command using shell-style quoting without executing it.
func SplitCommandLine(s string) ([]string, error) {
	var args []string
	var b strings.Builder
	quote, escaped, active := rune(0), false, false
	for _, r := range s {
		switch {
		case escaped:
			b.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
			active = true
		case quote != 0 && r == quote:
			quote = 0
		case quote == 0 && (r == '\'' || r == '"'):
			quote = r
			active = true
		case quote == 0 && unicode.IsSpace(r):
			if active {
				args = append(args, b.String())
				b.Reset()
				active = false
			}
		default:
			b.WriteRune(r)
			active = true
		}
	}
	if quote != 0 || escaped {
		return nil, fmt.Errorf("unterminated command quote or escape")
	}
	if active {
		args = append(args, b.String())
	}
	if len(args) == 0 || args[0] == "" {
		return nil, fmt.Errorf("empty command")
	}
	return args, nil
}

// DeriveNameFromCommand picks the package argument when possible, or executable basename.
func DeriveNameFromCommand(argv []string) string {
	if len(argv) == 0 {
		return "server"
	}
	start := 0
	switch path.Base(argv[0]) {
	case "npx", "uvx", "bunx":
		start = 1
	case "pnpm":
		if len(argv) > 1 && argv[1] == "dlx" {
			start = 2
		}
	}
	candidate := path.Base(argv[0])
	for _, arg := range argv[start:] {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		candidate = arg
		break
	}
	if strings.HasPrefix(candidate, "@") && strings.Contains(candidate, "/") {
		scope, pkg, _ := strings.Cut(strings.TrimPrefix(candidate, "@"), "/")
		if i := strings.Index(pkg, "@"); i >= 0 {
			pkg = pkg[:i]
		}
		candidate = pkg
		if candidate == "mcp" || candidate == "server" || candidate == "mcp-server" {
			candidate = scope
		}
	} else {
		candidate = path.Base(candidate)
	}
	if i := strings.Index(candidate, "@"); i >= 0 {
		candidate = candidate[:i]
	}
	for _, prefix := range []string{"mcp-server-", "server-", "mcp-"} {
		candidate = strings.TrimPrefix(candidate, prefix)
	}
	for _, suffix := range []string{"-mcp-server", "-mcp", "-server"} {
		candidate = strings.TrimSuffix(candidate, suffix)
	}
	return cleanServerName(candidate)
}

// DeriveNameFromRegistry preserves clean registry names and derives package names otherwise.
func DeriveNameFromRegistry(server *RegistryServer, query string) string {
	if server.Name != "" && !strings.ContainsAny(server.Name, "@/") && ValidateServerName(server.Name) == nil {
		return server.Name
	}
	if strings.HasPrefix(query, "@") {
		return DeriveNameFromCommand([]string{"npx", query})
	}
	for _, pkg := range server.Packages {
		if pkg.Identifier != "" {
			return DeriveNameFromCommand([]string{"npx", pkg.Identifier})
		}
	}
	return cleanServerName(query)
}
