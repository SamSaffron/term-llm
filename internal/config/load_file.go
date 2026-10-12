package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	mapstructure "github.com/go-viper/mapstructure/v2"
	"gopkg.in/yaml.v3"
)

// configFileNames lists the file names Load accepts in the config directory,
// in lookup order.
var configFileNames = []string{"config.yaml", "config.yml"}

// FindConfigFile returns the config file Load reads, or "" when none exists.
func FindConfigFile() (string, error) {
	dir, err := GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("get config dir: %w", err)
	}
	for _, name := range configFileNames {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", nil
}

// fileSettings is the parsed config file before defaults are applied.
type fileSettings struct {
	// path is the file that was read; empty when no config file exists.
	path string
	// values holds the file contents with every map key lowercased, because
	// config keys are case-insensitive.
	values map[string]any
	// providerEnv holds providers.<name>.env exactly as written: environment
	// variable names keep their case and values keep their YAML text. Keyed
	// by lowercase provider name.
	providerEnv map[string]map[string]string
}

func readFileSettings(path string) (*fileSettings, error) {
	settings := &fileSettings{path: path}
	if path == "" {
		return settings, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if lowered, ok := lowercaseKeys(values).(map[string]any); ok {
		settings.values = lowered
	}

	var envs struct {
		Providers map[string]struct {
			Env map[string]string `yaml:"env"`
		} `yaml:"providers"`
	}
	if yaml.Unmarshal(data, &envs) != nil {
		// A malformed providers section is reported by decodeSettings.
		return settings, nil
	}
	for name, provider := range envs.Providers {
		if provider.Env == nil {
			continue
		}
		if settings.providerEnv == nil {
			settings.providerEnv = make(map[string]map[string]string)
		}
		settings.providerEnv[strings.ToLower(name)] = provider.Env
	}
	return settings, nil
}

// lowercaseKeys returns a copy of v with every map key lowercased, including
// maps nested inside lists.
func lowercaseKeys(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, child := range val {
			out[strings.ToLower(k)] = lowercaseKeys(child)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(val))
		for k, child := range val {
			out[strings.ToLower(fmt.Sprint(k))] = lowercaseKeys(child)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, child := range val {
			out[i] = lowercaseKeys(child)
		}
		return out
	default:
		return v
	}
}

// lookup returns the file value at a dotted path. A null value counts as
// absent.
func (s *fileSettings) lookup(path string) (any, bool) {
	var current any = s.values
	for _, part := range strings.Split(strings.ToLower(path), ".") {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return current, current != nil
}

// has reports whether the config file sets a non-null value at path.
func (s *fileSettings) has(path string) bool {
	_, ok := s.lookup(path)
	return ok
}

// mergedSettings layers the file values over the schema defaults.
func (s *fileSettings) mergedSettings() map[string]any {
	merged := make(map[string]any)
	for key, value := range GetDefaults() {
		setNestedValue(merged, strings.Split(strings.ToLower(key), "."), value)
	}
	mergeSettings(merged, s.values)
	for name, env := range s.providerEnv {
		provider := setNestedMap(merged, "providers", name)
		provider["env"] = env
	}
	return merged
}

func setNestedValue(dst map[string]any, path []string, value any) {
	parent := setNestedMap(dst, path[:len(path)-1]...)
	parent[path[len(path)-1]] = value
}

// setNestedMap returns the map at path, creating maps as needed and replacing
// non-map values along the way.
func setNestedMap(dst map[string]any, path ...string) map[string]any {
	current := dst
	for _, part := range path {
		child, ok := current[part].(map[string]any)
		if !ok {
			child = make(map[string]any)
			current[part] = child
		}
		current = child
	}
	return current
}

// mergeSettings overlays src onto dst. A null in src keeps dst's value, and a
// map with no non-null values adds nothing, so "name:" and "name: {}" do not
// create entries on their own. Lists and scalars replace dst's value.
func mergeSettings(dst, src map[string]any) {
	for key, value := range src {
		switch val := value.(type) {
		case nil:
			continue
		case map[string]any:
			child, ok := dst[key].(map[string]any)
			if !ok {
				child = make(map[string]any)
			}
			mergeSettings(child, val)
			if len(child) > 0 {
				dst[key] = child
			}
		default:
			dst[key] = value
		}
	}
}

// decodeSettings decodes merged settings into cfg. Inputs are weakly typed so
// that, for example, a quoted "5" fills an int field.
func decodeSettings(settings map[string]any, cfg *Config) error {
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		WeaklyTypedInput: true,
		DecodeHook:       mapstructure.ComposeDecodeHookFunc(liveControlPlaneDecodeHook(), providerModelsDecodeHook()),
		Result:           cfg,
	})
	if err != nil {
		return err
	}
	return decoder.Decode(settings)
}
