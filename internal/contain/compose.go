package contain

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type ComposeInfo struct {
	Path          string
	Services      map[string]ServiceInfo
	Hints         Hints
	Invalid       bool
	InvalidReason string
}

type Hints struct {
	Purpose        string
	Workspace      string
	DefaultService string
	Shell          string
	PreferredCLI   string
	Agent          string
	ExecRecipes    map[string]ExecRecipe
}

type ExecRecipe struct {
	Name        string
	Description string
	Command     []string
}

const containUserLabel = "org.term-llm.contain.user"

type ServiceInfo struct {
	Name         string
	Labels       map[string]string
	BuildContext string
}

func (i ComposeInfo) DefaultService() string {
	if i.Hints.DefaultService != "" {
		return i.Hints.DefaultService
	}
	return "app"
}

func (i ComposeInfo) Shell() string {
	if i.Hints.Shell != "" {
		return i.Hints.Shell
	}
	return "/bin/sh"
}

func (i ComposeInfo) DefaultUser(service string) string {
	serviceInfo, ok := i.Services[service]
	if !ok {
		return ""
	}
	return strings.TrimSpace(serviceInfo.Labels[containUserLabel])
}

// ReadComposeInfo lightly parses the Compose file for term-llm hints, service
// names, and labels. It intentionally does not fully validate Compose.
func ReadComposeInfo(path string) (ComposeInfo, error) {
	info := ComposeInfo{Path: path, Services: map[string]ServiceInfo{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return info, err
	}
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		info.Invalid = true
		info.InvalidReason = err.Error()
		return info, nil
	}
	root, ok := asStringMap(raw)
	if !ok {
		info.Invalid = true
		info.InvalidReason = "compose file must be a YAML mapping"
		return info, nil
	}

	if hintsMap, ok := asStringMap(root["x-term-llm"]); ok {
		info.Hints = Hints{
			Purpose:        stringValue(hintsMap["purpose"]),
			Workspace:      stringValue(hintsMap["workspace"]),
			DefaultService: stringValue(hintsMap["default_service"]),
			Shell:          stringValue(hintsMap["shell"]),
			PreferredCLI:   stringValue(hintsMap["preferred_cli"]),
			Agent:          stringValue(hintsMap["agent"]),
			ExecRecipes:    parseExecRecipes(hintsMap["exec_recipes"]),
		}
	}

	servicesMap, ok := asStringMap(root["services"])
	if !ok || len(servicesMap) == 0 {
		info.Invalid = true
		info.InvalidReason = "compose file must define at least one service"
		return info, nil
	}
	for name, value := range servicesMap {
		service := ServiceInfo{Name: name, Labels: map[string]string{}}
		if svcMap, ok := asStringMap(value); ok {
			service.Labels = parseLabels(svcMap["labels"])
			service.BuildContext = parseBuildContext(svcMap["build"])
		}
		info.Services[name] = service
	}
	return info, nil
}

func parseExecRecipes(raw any) map[string]ExecRecipe {
	recipes := map[string]ExecRecipe{}
	recipesMap, ok := asStringMap(raw)
	if !ok {
		return recipes
	}
	for name, value := range recipesMap {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		recipe := ExecRecipe{Name: name}
		if recipeMap, ok := asStringMap(value); ok {
			recipe.Description = stringValue(recipeMap["description"])
			recipe.Command = stringSliceValue(recipeMap["command"])
		} else {
			recipe.Command = stringSliceValue(value)
		}
		if len(recipe.Command) == 0 {
			continue
		}
		recipes[name] = recipe
	}
	return recipes
}

func stringSliceValue(v any) []string {
	if v == nil {
		return nil
	}
	switch values := v.(type) {
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			text := strings.TrimSpace(stringValue(value))
			if text != "" {
				out = append(out, text)
			}
		}
		return out
	case []string:
		out := make([]string, 0, len(values))
		for _, value := range values {
			text := strings.TrimSpace(value)
			if text != "" {
				out = append(out, text)
			}
		}
		return out
	case string:
		return strings.Fields(values)
	default:
		text := strings.TrimSpace(stringValue(values))
		if text == "" {
			return nil
		}
		return []string{text}
	}
}

func parseBuildContext(raw any) string {
	if raw == nil {
		return ""
	}
	if buildMap, ok := asStringMap(raw); ok {
		return stringValue(buildMap["context"])
	}
	return stringValue(raw)
}

func parseLabels(raw any) map[string]string {
	labels := map[string]string{}
	if raw == nil {
		return labels
	}
	if m, ok := asStringMap(raw); ok {
		for k, v := range m {
			labels[k] = stringValue(v)
		}
		return labels
	}
	switch values := raw.(type) {
	case []any:
		for _, v := range values {
			parseLabelString(labels, stringValue(v))
		}
	case []string:
		for _, v := range values {
			parseLabelString(labels, v)
		}
	}
	return labels
}

func parseLabelString(labels map[string]string, label string) {
	if label == "" {
		return
	}
	parts := strings.SplitN(label, "=", 2)
	if len(parts) == 1 {
		labels[parts[0]] = ""
		return
	}
	labels[parts[0]] = parts[1]
}

func asStringMap(raw any) (map[string]any, bool) {
	switch m := raw.(type) {
	case map[string]any:
		return m, true
	case map[interface{}]interface{}:
		out := make(map[string]any, len(m))
		for k, v := range m {
			ks, ok := k.(string)
			if !ok {
				return nil, false
			}
			out[ks] = v
		}
		return out, true
	}
	return nil, false
}

func stringValue(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	default:
		return fmt.Sprint(t)
	}
}
