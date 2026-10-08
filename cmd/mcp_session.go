package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

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

// mcpRunSessionTarget returns the session to route `mcp run` through, if any.
func mcpRunSessionTarget() string {
	if mcpRunSpawn {
		return ""
	}
	if strings.TrimSpace(mcpRunSession) != "" {
		return mcpRunSession
	}
	return strings.TrimSpace(os.Getenv(mcpsession.EnvVar))
}

func mcpRunInSession(cmd *cobra.Command, target, serverName string, calls []mcpToolCall) error {
	client, err := mcpsession.Dial(target)
	if err != nil {
		return fmt.Errorf("%w (use --spawn to start a fresh server instead)", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpRunTimeout)
	defer cancel()

	schemas := make(map[string]map[string]any)
	if info, err := client.Session(ctx); err == nil {
		for _, srv := range info.Servers {
			if srv.Name != serverName {
				continue
			}
			for _, tool := range srv.Tools {
				schemas[tool.Name] = tool.InputSchema
			}
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
		ctx, cancel := context.WithTimeout(context.Background(), mcpRunTimeout)
		info, err := client.Session(ctx)
		cancel()
		if err != nil {
			// Stale socket from a crashed process; skip it.
			continue
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
// schema: a property declared as a string keeps its raw text instead of being
// auto-detected as a number/bool/null. Unknown keys produce a stderr hint
// listing the valid ones, because the server error alone rarely names them.
func applyInputSchemaToArgs(w io.Writer, call *mcpToolCall, schema map[string]any) {
	if call == nil || schema == nil {
		return
	}
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return
	}
	var unknown []string
	for key := range call.args {
		if _, ok := props[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	for key, raw := range call.raw {
		prop, _ := props[key].(map[string]any)
		if prop != nil && schemaAllowsOnlyString(prop) {
			call.args[key] = raw
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

func schemaAllowsOnlyString(prop map[string]any) bool {
	switch t := prop["type"].(type) {
	case string:
		return t == "string"
	case []any:
		hasString := false
		for _, v := range t {
			switch v {
			case "string":
				hasString = true
			case "number", "integer", "boolean":
				return false
			}
		}
		return hasString
	}
	return false
}
