package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/mcpsession"
	"github.com/spf13/cobra"
)

var mcpSessionsJSON bool

var mcpSessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "List live term-llm sessions exposing MCP servers",
	Long: `List term-llm sessions whose MCP servers can be called with
"term-llm mcp run --session <id>". Each session with running MCP servers
listens on a private unix socket (mode 0600) in $XDG_RUNTIME_DIR/term-llm.`,
	Args: cobra.NoArgs,
	RunE: mcpSessions,
}

func init() {
	mcpSessionsCmd.Flags().BoolVar(&mcpSessionsJSON, "json", false, "Output JSON including tool schemas")
}

// errMCPSessionUnavailable marks failures to reach the session socket at all,
// as opposed to tool or server errors returned through a live session.
var errMCPSessionUnavailable = errors.New("MCP session unavailable")

// mcpRunSessionTarget returns the session to route `mcp run` through, if any,
// and whether it was requested explicitly with --session.
func mcpRunSessionTarget() (target string, explicit bool) {
	if mcpRunSpawn {
		return "", false
	}
	if t := strings.TrimSpace(mcpRunSession); t != "" {
		return t, true
	}
	return strings.TrimSpace(os.Getenv(mcpsession.EnvVar)), false
}

func mcpRunInSession(cmd *cobra.Command, target, serverName string, calls []mcpToolCall) error {
	client, err := mcpsession.Dial(target)
	if err != nil {
		return fmt.Errorf("%w: %v (use --spawn to start a fresh server instead)", errMCPSessionUnavailable, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpRunTimeout)
	defer cancel()

	info, err := client.Session(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v (use --spawn to start a fresh server instead)", errMCPSessionUnavailable, err)
	}
	schemas := make(map[string]map[string]any)
	for _, srv := range info.Servers {
		if srv.Name != serverName {
			continue
		}
		for _, tool := range srv.Tools {
			schemas[tool.Name] = tool.InputSchema
		}
	}

	multiple := len(calls) > 1
	for _, call := range calls {
		applyInputSchemaToArgs(cmd.ErrOrStderr(), &call, schemas[call.name])
		argsJSON, err := json.Marshal(call.args)
		if err != nil {
			return fmt.Errorf("marshal args for %s: %w", call.name, err)
		}
		if multiple {
			fmt.Fprintf(cmd.OutOrStdout(), "--- %s ---\n", call.name)
		}
		res, err := client.Call(ctx, serverName, call.name, argsJSON)
		if err != nil {
			return fmt.Errorf("call %s via session: %w", call.name, err)
		}
		result := res.ToolOutput()
		if err := renderMCPToolResult(cmd.OutOrStdout(), result); err != nil {
			return fmt.Errorf("format result for %s: %w", call.name, err)
		}
		if err := mcpToolResultError(call.name, result); err != nil {
			return err
		}
	}
	return nil
}

func mcpSessions(cmd *cobra.Command, _ []string) error {
	mcpsession.SweepStale()
	paths, err := mcpsession.ListSockets()
	if err != nil {
		return err
	}
	current := strings.TrimSpace(os.Getenv(mcpsession.EnvVar))
	var all []map[string]any
	out := cmd.OutOrStdout()
	if len(paths) == 0 && !mcpSessionsJSON {
		fmt.Fprintln(out, "No live MCP sessions.")
		return nil
	}
	for _, path := range paths {
		label := mcpsession.LabelFromPath(path)
		client, err := mcpsession.Dial(path)
		if err != nil {
			continue
		}
		// Short per-socket timeout so one wedged session cannot stall listing.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		info, err := client.Session(ctx)
		cancel()
		if err != nil {
			// Stale socket from a crashed process; skip it.
			continue
		}
		if info.SessionID != "" {
			label = info.SessionID
		}
		if mcpSessionsJSON {
			all = append(all, map[string]any{"id": label, "socket": path, "session": info})
			continue
		}
		marker := ""
		if current == path {
			marker = "  (current)"
		}
		fmt.Fprintf(out, "%s  pid %d%s\n  socket: %s\n", label, info.PID, marker, path)
		for _, srv := range info.Servers {
			fmt.Fprintf(out, "  %s [%s] %d tools", srv.Name, srv.Status, len(srv.Tools))
			if srv.Error != "" {
				fmt.Fprintf(out, " error: %s", srv.Error)
			}
			fmt.Fprintln(out)
		}
	}
	if mcpSessionsJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if all == nil {
			all = []map[string]any{}
		}
		return enc.Encode(all)
	}
	return nil
}

// applyInputSchemaToArgs re-types key=value arguments using the tool's JSON
// schema. Values are auto-detected (1815 -> number) first; when the schema
// rejects the detected type but accepts a string, the raw text is used
// instead (text=1815 stays "1815"). Enums are honoured the same way. Unknown
// keys produce a stderr hint listing valid ones when the schema forbids
// additional properties, because server errors rarely name them.
func applyInputSchemaToArgs(w io.Writer, call *mcpToolCall, schema map[string]any) {
	if call == nil || schema == nil {
		return
	}
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return
	}
	for key, raw := range call.raw {
		prop, _ := props[key].(map[string]any)
		if prop == nil {
			continue
		}
		parsed := call.args[key]
		if _, isString := parsed.(string); isString {
			continue
		}
		if schemaPrefersRawString(prop, parsed, raw) {
			call.args[key] = raw
		}
	}
	if additional, ok := schema["additionalProperties"].(bool); !ok || additional {
		return
	}
	var unknown []string
	for key := range call.args {
		if _, ok := props[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		valid := make([]string, 0, len(props))
		for key := range props {
			valid = append(valid, key)
		}
		sort.Strings(valid)
		sort.Strings(unknown)
		fmt.Fprintf(w, "warning: %s does not declare argument(s) %s; valid arguments: %s\n",
			call.name, strings.Join(unknown, ", "), strings.Join(valid, ", "))
	}
}

// schemaPrefersRawString reports whether a non-string auto-detected value
// should be replaced by its raw text for this property schema.
func schemaPrefersRawString(prop map[string]any, parsed any, raw string) bool {
	if enum, ok := prop["enum"].([]any); ok && len(enum) > 0 {
		parsedOK, rawOK := false, false
		for _, v := range enum {
			if jsonEqual(v, parsed) {
				parsedOK = true
			}
			if s, ok := v.(string); ok && s == raw {
				rawOK = true
			}
		}
		if parsedOK || rawOK {
			return !parsedOK && rawOK
		}
	}
	types := schemaTypes(prop)
	if len(types) == 0 || !types["string"] {
		return false
	}
	return !types[jsonTypeOf(parsed)] && !(jsonTypeOf(parsed) == "integer" && types["number"])
}

func schemaTypes(prop map[string]any) map[string]bool {
	out := map[string]bool{}
	switch t := prop["type"].(type) {
	case string:
		out[t] = true
	case []any:
		for _, v := range t {
			if s, ok := v.(string); ok {
				out[s] = true
			}
		}
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if alts, ok := prop[key].([]any); ok {
			for _, alt := range alts {
				if m, ok := alt.(map[string]any); ok {
					for k := range schemaTypes(m) {
						out[k] = true
					}
				}
			}
		}
	}
	return out
}

func jsonTypeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case int, int64:
		return "integer"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return ""
}

func jsonEqual(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}
