package contain

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

type TemplateDescriptor struct {
	Version     int              `yaml:"version"`
	Name        string           `yaml:"name"`
	Description string           `yaml:"description"`
	Prompts     []TemplatePrompt `yaml:"prompts"`
}

type TemplatePrompt struct {
	ID       string             `yaml:"id"`
	Label    string             `yaml:"label"`
	Type     string             `yaml:"type"`
	Default  any                `yaml:"default"`
	Options  []string           `yaml:"options"`
	Required bool               `yaml:"required"`
	When     *TemplateCondition `yaml:"when"`
}

type TemplateCondition struct {
	Equals map[string]string `yaml:"equals"`
}

func parseTemplateDescriptor(data []byte) (*TemplateDescriptor, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var desc TemplateDescriptor
	if err := yaml.Unmarshal(data, &desc); err != nil {
		return nil, err
	}
	return &desc, nil
}

func ResolveTemplateValues(desc *TemplateDescriptor, provided map[string]string, noInput bool, stdin io.Reader, stdout io.Writer) (map[string]string, error) {
	values := map[string]string{}
	if provided != nil {
		for k, v := range provided {
			values[k] = v
		}
	}
	if desc == nil {
		return values, nil
	}
	if stdin == nil {
		stdin = os.Stdin
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	interactive := !noInput && isInteractiveReader(stdin)
	reader := bufio.NewReader(stdin)

	for _, prompt := range desc.Prompts {
		if prompt.ID == "" {
			return nil, fmt.Errorf("template prompt missing id")
		}
		if !promptApplies(prompt, values) {
			if _, ok := values[prompt.ID]; !ok {
				values[prompt.ID] = ""
			}
			continue
		}
		if _, ok := values[prompt.ID]; ok {
			continue
		}
		def, err := resolvePromptDefault(prompt.Default)
		if err != nil {
			return nil, fmt.Errorf("prompt %s default: %w", prompt.ID, err)
		}
		if interactive {
			value, err := askTemplatePrompt(reader, stdin, stdout, prompt, def)
			if err != nil {
				return nil, err
			}
			values[prompt.ID] = value
		} else {
			values[prompt.ID] = def
		}
		if prompt.Required && values[prompt.ID] == "" {
			return nil, fmt.Errorf("template prompt %q is required; pass --set %s=value or run interactively", prompt.ID, prompt.ID)
		}
	}
	return values, nil
}

func promptApplies(prompt TemplatePrompt, values map[string]string) bool {
	if prompt.When == nil || len(prompt.When.Equals) == 0 {
		return true
	}
	for key, want := range prompt.When.Equals {
		if values[key] != want {
			return false
		}
	}
	return true
}

func resolvePromptDefault(raw any) (string, error) {
	if raw == nil {
		return "", nil
	}
	switch v := raw.(type) {
	case string:
		return v, nil
	case int:
		return strconv.Itoa(v), nil
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	case map[string]any:
		return resolveGeneratedDefault(v)
	case map[any]any:
		m := map[string]any{}
		for k, val := range v {
			ks, ok := k.(string)
			if !ok {
				continue
			}
			m[ks] = val
		}
		return resolveGeneratedDefault(m)
	default:
		return fmt.Sprint(v), nil
	}
}

func resolveGeneratedDefault(m map[string]any) (string, error) {
	if m["generate"] == "agent_distro" {
		return defaultAgentDistro(), nil
	}
	if m["generate"] == "hex" {
		bytesN := 24
		if b, ok := m["bytes"]; ok {
			switch v := b.(type) {
			case int:
				bytesN = v
			case int64:
				bytesN = int(v)
			case float64:
				bytesN = int(v)
			case string:
				parsed, err := strconv.Atoi(v)
				if err != nil {
					return "", err
				}
				bytesN = parsed
			}
		}
		buf := make([]byte, bytesN)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		return hex.EncodeToString(buf), nil
	}
	return "", fmt.Errorf("unsupported generated default %v", m["generate"])
}

func askTemplatePrompt(reader *bufio.Reader, stdin io.Reader, stdout io.Writer, prompt TemplatePrompt, def string) (string, error) {
	label := prompt.Label
	if label == "" {
		label = prompt.ID
	}
	promptType := prompt.Type
	if promptType == "" {
		promptType = "string"
	}

	switch promptType {
	case "select":
		fmt.Fprintf(stdout, "%s", label)
		if len(prompt.Options) > 0 {
			fmt.Fprintf(stdout, " [%s]", strings.Join(prompt.Options, "/"))
		}
		if def != "" {
			fmt.Fprintf(stdout, " (%s)", def)
		}
		fmt.Fprint(stdout, ": ")
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		value := strings.TrimSpace(line)
		if value == "" {
			value = def
		}
		if len(prompt.Options) > 0 && value != "" {
			for _, opt := range prompt.Options {
				if value == opt {
					return value, nil
				}
			}
			return "", fmt.Errorf("invalid value %q for %s; expected one of %s", value, prompt.ID, strings.Join(prompt.Options, ", "))
		}
		return value, nil
	case "secret":
		if def != "" {
			fmt.Fprintf(stdout, "%s [generated/hidden]: ", label)
		} else {
			fmt.Fprintf(stdout, "%s: ", label)
		}
		if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			bytes, err := term.ReadPassword(int(f.Fd()))
			fmt.Fprintln(stdout)
			if err != nil {
				return "", err
			}
			value := strings.TrimSpace(string(bytes))
			if value == "" {
				return def, nil
			}
			return value, nil
		}
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		value := strings.TrimSpace(line)
		if value == "" {
			value = def
		}
		return value, nil
	default:
		if def != "" {
			fmt.Fprintf(stdout, "%s [%s]: ", label, def)
		} else {
			fmt.Fprintf(stdout, "%s: ", label)
		}
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		value := strings.TrimSpace(line)
		if value == "" {
			value = def
		}
		return value, nil
	}
}

func isInteractiveReader(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
