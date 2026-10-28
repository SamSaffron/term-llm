package procutil

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
)

// ResolveShellEnvironment snapshots the inherited environment and applies the
// exact validated overrides that will be shown for approval. No expansion of
// values (including $PATH) is performed.
func ResolveShellEnvironment(overrides map[string]string) ([]string, error) {
	keys := shellEnvironmentKeys(overrides)
	shadowed := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return nil, fmt.Errorf("invalid environment key %q: must be nonempty and contain neither '=' nor NUL", key)
		}
		if strings.ContainsRune(overrides[key], '\x00') {
			return nil, fmt.Errorf("environment value for %q contains NUL", key)
		}
		shadowed[shellEnvironmentKey(key)] = struct{}{}
	}
	inherited := os.Environ()
	env := make([]string, 0, len(inherited)+len(keys))
	for _, entry := range inherited {
		if key, _, ok := strings.Cut(entry, "="); ok {
			if _, exists := shadowed[shellEnvironmentKey(key)]; exists {
				continue
			}
		}
		env = append(env, entry)
	}
	for _, key := range keys {
		env = append(env, key+"="+overrides[key])
	}
	return env, nil
}

func shellEnvironmentKey(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}

func shellEnvironmentKeys(overrides map[string]string) []string {
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ShellEnvironmentCommand is approval evidence, not the string executed. It
// models env being installed BEFORE the shell starts (including startup hooks),
// rather than misleadingly showing assignments inside an already-started shell.
// Stable, lossless quoting binds every supplied key/value and the shell itself
// to both human/Guardian review and exact approval caches. Unspecified variables
// continue to inherit the host environment; they are not model overrides.
func ShellEnvironmentCommand(command, shell string, overrides map[string]string) string {
	if len(overrides) == 0 {
		return command
	}
	quote := func(value string) string {
		return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
	}
	var b strings.Builder
	b.WriteString("env --")
	for _, key := range shellEnvironmentKeys(overrides) {
		b.WriteByte(' ')
		b.WriteString(quote(key + "=" + overrides[key]))
	}
	b.WriteByte(' ')
	b.WriteString(quote(shell))
	b.WriteString(" -c ")
	b.WriteString(quote(command))
	return b.String()
}
