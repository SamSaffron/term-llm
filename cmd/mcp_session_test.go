package cmd

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/mcpsession"
	"github.com/spf13/cobra"
)

func coerce(t *testing.T, schema map[string]any, args ...string) (map[string]any, string) {
	t.Helper()
	calls, err := parseMCPRunCalls(append([]string{"tool"}, args...))
	if err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	applyInputSchemaToArgs(&warn, &calls[0], schema)
	return calls[0].args, warn.String()
}

func props(p map[string]any, extra ...any) map[string]any {
	s := map[string]any{"properties": p}
	if len(extra) == 2 {
		s[extra[0].(string)] = extra[1]
	}
	return s
}

func TestApplyInputSchemaToArgs(t *testing.T) {
	str := map[string]any{"type": "string"}

	args, _ := coerce(t, props(map[string]any{"text": str}), "text=1815")
	if args["text"] != "1815" {
		t.Fatalf("string property should keep raw text, got %#v", args["text"])
	}
	args, _ = coerce(t, props(map[string]any{"n": map[string]any{"type": "integer"}, "b": map[string]any{"type": "boolean"}}), "n=3", "b=true")
	if args["n"] != int64(3) || args["b"] != true {
		t.Fatalf("non-string properties keep detected types: %#v", args)
	}
	// string|number accepts the parsed number as-is.
	args, _ = coerce(t, props(map[string]any{"v": map[string]any{"type": []any{"number", "string"}}}), "v=2")
	if args["v"] != int64(2) {
		t.Fatalf("union accepting number should keep it, got %#v", args["v"])
	}
	// string|null: null parsed value is accepted only if null is declared.
	args, _ = coerce(t, props(map[string]any{"v": map[string]any{"type": []any{"string", "null"}}}), "v=null")
	if args["v"] != nil {
		t.Fatalf("declared null should stay null, got %#v", args["v"])
	}
	// anyOf with only string alternatives.
	args, _ = coerce(t, props(map[string]any{"v": map[string]any{"anyOf": []any{str}}}), "v=true")
	if args["v"] != "true" {
		t.Fatalf("anyOf string should keep raw text, got %#v", args["v"])
	}
	// enum of strings.
	args, _ = coerce(t, props(map[string]any{"level": map[string]any{"enum": []any{"1", "2"}}}), "level=1")
	if args["level"] != "1" {
		t.Fatalf("string enum should keep raw text, got %#v", args["level"])
	}
	// enum of numbers keeps the number.
	args, _ = coerce(t, props(map[string]any{"level": map[string]any{"enum": []any{1.0, 2.0}}}), "level=1")
	if args["level"] != int64(1) {
		t.Fatalf("numeric enum should keep the number, got %#v", args["level"])
	}
}

func TestApplyInputSchemaUnknownKeyHintOnlyWhenForbidden(t *testing.T) {
	p := map[string]any{"target": map[string]any{"type": "string"}, "text": map[string]any{"type": "string"}}
	_, warn := coerce(t, props(p, "additionalProperties", false), "ref=e5", "text=x")
	if !strings.Contains(warn, "ref") || !strings.Contains(warn, "valid arguments: target, text") {
		t.Fatalf("expected unknown-argument hint, got %q", warn)
	}
	if _, warn := coerce(t, props(p), "ref=e5"); warn != "" {
		t.Fatalf("no hint when additional properties are allowed, got %q", warn)
	}
}

func TestParseMCPRunCallsRawTracking(t *testing.T) {
	str := props(map[string]any{"text": map[string]any{"type": "string"}, "a": map[string]any{"type": "string"}})
	// A JSON object replaces earlier key=value pairs, including their raw text.
	args, _ := coerce(t, str, "text=1", `{"a":"z"}`)
	if _, ok := args["text"]; ok || args["a"] != "z" {
		t.Fatalf("JSON object should replace earlier args, got %#v", args)
	}
	// @file content must not be overwritten by an earlier raw value.
	f, err := os.CreateTemp(t.TempDir(), "arg")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("from-file")
	f.Close()
	args, _ = coerce(t, str, "text=old", "text=@"+f.Name())
	if args["text"] != "from-file" {
		t.Fatalf("@file value should win, got %#v", args["text"])
	}
}

func TestMCPRunSessionTargetPrecedence(t *testing.T) {
	defer func(s string, sp bool) { mcpRunSession, mcpRunSpawn = s, sp }(mcpRunSession, mcpRunSpawn)
	t.Setenv(mcpsession.EnvVar, "/env.sock")

	mcpRunSession, mcpRunSpawn = "", false
	if got, explicit := mcpRunSessionTarget(); got != "/env.sock" || explicit {
		t.Fatalf("env target = %q explicit=%v", got, explicit)
	}
	mcpRunSession = "/flag.sock"
	if got, explicit := mcpRunSessionTarget(); got != "/flag.sock" || !explicit {
		t.Fatalf("flag target = %q explicit=%v", got, explicit)
	}
	mcpRunSpawn = true
	if got, _ := mcpRunSessionTarget(); got != "" {
		t.Fatalf("--spawn must bypass sessions, got %q", got)
	}
}

func TestMCPRunInSessionReportsUnavailableForDeadSocket(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := mcpRunInSession(cmd, "/nonexistent/mcp-gone@00000000.sock", "playwright", []mcpToolCall{{name: "browser_snapshot", args: map[string]any{}}})
	if !errors.Is(err, errMCPSessionUnavailable) {
		t.Fatalf("dead socket should be errMCPSessionUnavailable (enables spawn fallback for inherited env), got %v", err)
	}
}
