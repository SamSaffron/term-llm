package serve

import (
	"fmt"
	"strings"
)

var knownPlatforms = map[string]bool{"web": true, "api": true, "jobs": true, "telegram": true, "proxy": true}

// ResolvePlatforms returns the list of platforms to serve. Positional args take
// precedence; if none are given, configPlatforms (from config.yaml
// serve.platforms) is used as fallback.
func ResolvePlatforms(args []string, configPlatforms []string) ([]string, error) {
	var raw []string
	if len(args) > 0 {
		raw = args
	} else if len(configPlatforms) > 0 {
		raw = configPlatforms
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("no platforms specified\n\nUsage: term-llm serve <platform> [platform...]\n\nExamples:\n  term-llm serve web\n  term-llm serve telegram web\n\nOr set serve.platforms in config.yaml")
	}

	seen := make(map[string]bool)
	var out []string
	for _, a := range raw {
		p := strings.TrimSpace(strings.ToLower(a))
		if p == "" {
			continue
		}
		if !knownPlatforms[p] {
			return nil, fmt.Errorf("unknown platform %q (valid: web, api, jobs, telegram, proxy)", p)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no platforms specified\n\nUsage: term-llm serve <platform> [platform...]\n\nExamples:\n  term-llm serve web\n  term-llm serve telegram web\n\nOr set serve.platforms in config.yaml")
	}
	return out, nil
}

// PlatformContains reports whether platforms includes name.
func PlatformContains(platforms []string, name string) bool {
	for _, p := range platforms {
		if p == name {
			return true
		}
	}
	return false
}
