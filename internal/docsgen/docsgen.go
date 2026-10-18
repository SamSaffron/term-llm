// Package docsgen renders source-backed sections of the documentation.
package docsgen

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/providerspec"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Page identifies a generated section in a reference page.
type Page struct {
	Path, Section, Content string
}

// Pages returns the generated content, independent of the current contents of
// the Markdown files. root is the registered Cobra command tree.
func Pages(root *cobra.Command) []Page {
	return []Page{
		{"configuration.md", "configuration", configTable()},
		{"built-in-tools.md", "tools", toolTable()},
		{"providers-and-models.md", "providers", providerTable()},
		{"provider-setup-details.md", "provider-setup", setupTable()},
		{"cli.md", "cli", commandTree(root)},
	}
}

func cell(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "—"
	}
	return s
}

// summary returns the first sentence of a model-facing description, which is
// often a multi-paragraph prompt unsuited to a reference table.
func summary(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i+1]
	}
	const max = 160
	if len(s) > max {
		cut := strings.LastIndex(s[:max], " ")
		if cut <= 0 {
			cut = max
		}
		s = s[:cut] + "…"
	}
	return s
}

func code(s string) string {
	if s == "" {
		return "—"
	}
	return "`" + strings.ReplaceAll(s, "`", "\\`") + "`"
}

func valueType(v any) string {
	if v == nil {
		return "—"
	}
	t := reflect.TypeOf(v)
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		return "list of " + valueType(reflect.Zero(t.Elem()).Interface())
	case reflect.Map:
		return "map"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	default:
		return "string"
	}
}

func configTable() string {
	var b strings.Builder
	b.WriteString("| Key | Type | Default | Description |\n|---|---|---|---|\n")
	for _, s := range config.ConfigKeySpecs() {
		v := s.Placeholder
		defaultText := "—"
		if s.HasDefault {
			v = s.Default
			defaultText = code(fmt.Sprint(s.Default))
			if text, ok := s.Default.(string); ok && text == "" {
				defaultText = "`\"\"`"
			}
		}
		if s.Sensitive {
			defaultText = "—"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", code(s.Path), cell(valueType(v)), cell(defaultText), cell(s.Description))
	}
	return b.String()
}

func toolTable() string {
	var b strings.Builder
	b.WriteString("| Tool | Description | Permission policy | Surfaces | Default enabled | Aliases |\n|---|---|---|---|---|---|\n")
	for _, d := range tools.BuiltinToolDescriptors() {
		modes := make([]string, len(d.Modes))
		for i, mode := range d.Modes {
			modes[i] = code(string(mode))
		}
		aliases := make([]string, len(d.Aliases))
		for i, alias := range d.Aliases {
			aliases[i] = code(alias)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %t | %s |\n", code(d.Name), cell(summary(d.Description)), code(string(d.Policy)), strings.Join(modes, ", "), d.DefaultEnabled, cell(strings.Join(aliases, ", ")))
	}
	return b.String()
}

func credentials(s providerspec.Spec) string {
	parts := []string{string(s.Credential)}
	if s.APIKeyEnv != "" {
		parts = append(parts, code(s.APIKeyEnv))
	}
	if s.HostEnv != "" {
		parts = append(parts, code(s.HostEnv))
	}
	if s.Setup != "" {
		parts = append(parts, s.Setup)
	}
	return strings.Join(parts, "; ")
}

func capabilities(s providerspec.Spec) string {
	var parts []string
	for _, item := range []struct {
		enabled bool
		label   string
	}{
		{s.Capabilities.Tools, "tools"}, {s.Capabilities.Images, "images"},
		{s.Capabilities.ReasoningEffort, "reasoning effort"},
		{s.Capabilities.CLI, "companion CLI"}, {s.Capabilities.FixedToolBridge, "fixed tool bridge"},
	} {
		if item.enabled {
			parts = append(parts, item.label)
		}
	}
	return cell(strings.Join(parts, ", "))
}

func providerTable() string {
	var b strings.Builder
	b.WriteString("| Provider | Type | Display name | Credentials / environment | Capabilities | Model listing |\n|---|---|---|---|---|---|\n")
	for _, s := range providerspec.All() {
		listing := string(s.ModelListing)
		if s.ListModels {
			listing += " (upstream listing)"
		} else if listing == "curated" {
			listing += " (local catalog)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", code(s.Name), code(string(s.Type)), cell(s.DisplayName), cell(credentials(s)), capabilities(s), cell(listing))
	}
	return b.String()
}

func setupTable() string {
	var b strings.Builder
	b.WriteString("| Provider | Setup | Default model | Fast model |\n|---|---|---|---|\n")
	for _, s := range providerspec.All() {
		defaults := map[string]string{}
		for _, d := range s.Defaults {
			if d.Path == "model" || d.Path == "fast_model" {
				defaults[d.Path] = fmt.Sprint(d.Value)
			}
		}
		setup := s.Setup
		if setup == "" && s.APIKeyEnv != "" {
			setup = "set " + code(s.APIKeyEnv)
		}
		if setup == "" {
			setup = string(s.Credential)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", code(s.Name), cell(setup), code(defaults["model"]), code(defaults["fast_model"]))
	}
	return b.String()
}

func commandTree(root *cobra.Command) string {
	var b strings.Builder
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Hidden || !c.IsAvailableCommand() && c != root {
			return
		}
		fmt.Fprintf(&b, "### `%s`\n\n%s\n\n", c.CommandPath(), cell(c.Short))
		flags := c.LocalNonPersistentFlags()
		if c == root {
			b.WriteString("Global flags (inherited by subcommands):\n\n")
		}
		var rows []string
		addFlag := func(f *pflag.Flag) {
			if f.Hidden || f.Name == "help" {
				return
			}
			name := "--" + f.Name
			if f.Shorthand != "" {
				name += ", -" + f.Shorthand
			}
			rows = append(rows, fmt.Sprintf("| %s | %s | %s |", code(name), cell(f.Value.Type()), cell(f.Usage)))
		}
		flags.VisitAll(addFlag)
		c.PersistentFlags().VisitAll(addFlag)
		sort.Strings(rows)
		if len(rows) > 0 {
			b.WriteString("| Flag | Type | Description |\n|---|---|---|\n")
			b.WriteString(strings.Join(rows, "\n") + "\n\n")
		}
		for _, child := range c.Commands() {
			walk(child)
		}
	}
	walk(root)
	return b.String()
}

// Render replaces only the marked section, preserving surrounding prose.
func Render(original []byte, page Page) ([]byte, error) {
	begin := "<!-- BEGIN GENERATED: " + page.Section + " -->"
	end := "<!-- END GENERATED: " + page.Section + " -->"
	s := string(original)
	if strings.Count(s, begin) != 1 || strings.Count(s, end) != 1 {
		return nil, fmt.Errorf("%s: expected exactly one pair of %s markers", page.Path, page.Section)
	}
	start := strings.Index(s, begin) + len(begin)
	stop := strings.Index(s, end)
	if stop < start {
		return nil, fmt.Errorf("%s: reversed %s markers", page.Path, page.Section)
	}
	return []byte(s[:start] + "\n" + strings.TrimSpace(page.Content) + "\n" + s[stop:]), nil
}

// Sync checks or updates generated sections under the reference directory.
// Missing pages/markers fail closed rather than silently omitting a section.
func Sync(dir string, pages []Page, check bool) error {
	for _, page := range pages {
		path := filepath.Join(dir, page.Path)
		original, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		rendered, err := Render(original, page)
		if err != nil {
			return err
		}
		if string(rendered) == string(original) {
			continue
		}
		if check {
			return fmt.Errorf("%s: generated reference docs are out of date; run make docs", path)
		}
		if err := os.WriteFile(path, rendered, 0644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}
