package chat

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"text/template"
	"time"
)

// referenceTitleFormat is the text/template implementation the hand-written
// evaluator replaced. It is kept in tests only, where linking text/template
// costs nothing, so the two can be compared directly.
func referenceTitleFormat(t *testing.T, format string, data terminalTitleFormatData, env TerminalTitleEnvironment) (string, bool) {
	t.Helper()
	simple := regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
	envDot := regexp.MustCompile(`\{\{\s*env\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
	format = envDot.ReplaceAllString(format, `{{env "$1"}}`)
	format = simple.ReplaceAllStringFunc(format, func(action string) string {
		if field, ok := terminalTitleFormatField(simple.FindStringSubmatch(action)[1]); ok {
			return "{{." + field + "}}"
		}
		return action
	})
	funcs := template.FuncMap{
		"env": env.Get,
		"default": func(fallback, value string) string {
			if strings.TrimSpace(value) == "" {
				return fallback
			}
			return value
		},
	}
	tmpl, err := template.New("terminal-title").Funcs(funcs).Option("missingkey=zero").Parse(format)
	if err != nil {
		return "", false
	}
	type referenceData struct {
		terminalTitleFormatData
		Env map[string]string
	}
	values := map[string]string{}
	for _, name := range []string{"DOCKER_CONTAINER_NAME", "MISSING", "EMPTY"} {
		if v := env.Get(name); v != "" {
			values[name] = v
		}
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, referenceData{data, values}); err != nil {
		return "", false
	}
	return b.String(), true
}

func TestTitleFormatMatchesTextTemplate(t *testing.T) {
	env := NewTerminalTitleEnvironment(map[string]string{"DOCKER_CONTAINER_NAME": "worker-1", "EMPTY": " "})
	formats := []string{
		"plain text, no actions",
		"[{{env.DOCKER_CONTAINER_NAME}}] {{attention}}{{agent}}/{{task}}/{{state}}/{{activity}}",
		`{{env "MISSING" | default "host"}} · {{title}}`,
		`{{env "EMPTY" | default "blank"}} · {{env "DOCKER_CONTAINER_NAME" | default "host"}}`,
		`{{default "host" (env "MISSING")}} {{default "x" (env "DOCKER_CONTAINER_NAME")}}`,
		"{{task}} · {{branch}} · {{model}}",
		"{{ title }} {{Title}} {{STABLE_TITLE}} {{stabletitle}} {{attention_marker}}{{attentionmarker}}",
		"{{phase}} {{elapsed}} {{.Title}} {{.Agent}} {{.Attention}} {{.Streaming}} {{.AttentionMarker}}",
		"{{.Env.DOCKER_CONTAINER_NAME}}|{{.Env.MISSING}}|",
		`{{"literal"}} {{` + "`raw`" + `}} {{"esc\"aped"}}`,
		"a  {{- .Agent -}}  b",
		"{{.Model | default \"none\"}} {{.Branch | default \"root\"}}",
		`{{env "MISSING" | default "" | default "chained"}}`,
		"brace } inside {{agent}} }} text",
	}
	states := []titleState{
		{},
		{Agent: "developer", Task: "Fix", Model: "fable"},
		{Agent: "developer", Task: "Custom title", Model: "fable", Branch: "branch", Phase: "Thinking", Streaming: true, Elapsed: 3 * time.Second},
		{Agent: "a", Task: "b", Attention: true},
	}
	for _, format := range formats {
		prog, err := parseTitleFormat(format)
		for _, st := range states {
			data := terminalTitleFormatDataForState(st, env)
			want, ok := referenceTitleFormat(t, format, data, env)
			if !ok {
				t.Fatalf("reference rejected %q", format)
			}
			if err != nil {
				t.Fatalf("parseTitleFormat(%q): %v", format, err)
			}
			if got := prog.execute(data, env); got != want {
				t.Errorf("format %q state %+v:\n got %q\nwant %q", format, st, got, want)
			}
		}
	}
}

func TestTitleFormatRejectsUnsupportedSyntax(t *testing.T) {
	for _, format := range []string{
		"{{unknown_placeholder}}",
		"{{.UnknownField}}",
		"{{agent",
		`{{env}}`,
		`{{env "A" "B"}}`,
		`{{default "x"}}`,
		`{{agent | title}}`,
		`{{agent model}}`,
		`{{default "x" (env "A"}}`,
		`{{"unterminated}}`,
		"{{}}",
		"{{if .Attention}}!{{end}}",
		`{{printf "%s" .Agent}}`,
	} {
		if _, err := parseTitleFormat(format); err == nil {
			t.Errorf("parseTitleFormat(%q) succeeded, want error", format)
		}
	}
}
