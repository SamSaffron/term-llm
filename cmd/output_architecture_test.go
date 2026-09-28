package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Keep this list scoped to application workers used by an interactive UI.
// CLI-only commands, renderer-owned output, auth prompts outside the UI and
// terminal hand-offs are not diagnostics and must not be banned wholesale.
func TestInteractiveWorkersDoNotBypassDiagnosticSink(t *testing.T) {
	packages := []string{"../internal/llm", "../internal/agents", "../internal/skills", "../internal/mcp", "../internal/session", "../internal/tools", "../internal/termhost"}
	cmdFiles := map[string]bool{
		"chat.go": true, "chat_warnings.go": true, "runner.go": true, "session.go": true,
		"skills.go": true, "mcp.go": true, "sessions.go": true, "config_theme.go": true,
		"filetrack.go": true, "goal_runtime.go": true, "guardian_wiring.go": true,
		"process_reload.go": true, "spawn_runner.go": true, "tools.go": true,
		"interactive_diagnostics.go": true,
	}
	var violations []string
	for _, dir := range append(packages, ".") {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || (dir == "." && !cmdFiles[name]) {
				continue
			}
			path := filepath.Join(dir, name)
			fs := token.NewFileSet()
			file, err := parser.ParseFile(fs, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					// os.Stdout.Write / os.Stderr.Write are direct writes too.
					if stream, ok := sel.X.(*ast.SelectorExpr); ok {
						if pkg, ok := stream.X.(*ast.Ident); ok && pkg.Name == "os" && (stream.Sel.Name == "Stdout" || stream.Sel.Name == "Stderr") && strings.HasPrefix(sel.Sel.Name, "Write") {
							violations = append(violations, path+":"+fn.Name.Name+":os."+stream.Sel.Name+"."+sel.Sel.Name)
						}
						return true
					}
					base, ok := sel.X.(*ast.Ident)
					if !ok {
						return true
					}
					kind := ""
					switch base.Name {
					case "slog":
						if sel.Sel.Name == "Debug" || sel.Sel.Name == "Info" || sel.Sel.Name == "Warn" || sel.Sel.Name == "Error" || strings.HasPrefix(sel.Sel.Name, "Log") {
							kind = "slog." + sel.Sel.Name
						}
					case "log":
						if strings.HasPrefix(sel.Sel.Name, "Print") || sel.Sel.Name == "Fatal" || sel.Sel.Name == "Fatalln" {
							kind = "log." + sel.Sel.Name
						}
					case "fmt":
						if strings.HasPrefix(sel.Sel.Name, "Print") && dir != "." {
							kind = "fmt." + sel.Sel.Name
						}
						if strings.HasPrefix(sel.Sel.Name, "Fprint") && len(call.Args) > 0 {
							if dest, ok := call.Args[0].(*ast.SelectorExpr); ok {
								if pkg, ok := dest.X.(*ast.Ident); ok && pkg.Name == "os" && (dest.Sel.Name == "Stdout" || dest.Sel.Name == "Stderr") {
									kind = "fmt." + sel.Sel.Name + "(os." + dest.Sel.Name + ")"
								}
							}
						}
					case "os":
						if sel.Sel.Name == "Stdout" || sel.Sel.Name == "Stderr" {
							kind = "os." + sel.Sel.Name + "()"
						}
					}
					if kind == "" {
						return true
					}
					key := name + ":" + fn.Name.Name + ":" + kind
					if allowedInteractiveDirectOutput[key] {
						return true
					}
					violations = append(violations, path+":"+fn.Name.Name+":"+kind)
					return true
				})
			}
		}
	}
	sort.Strings(violations)
	if len(violations) != 0 {
		t.Fatalf("interactive workers bypass diagnostic routing (or need documented exemptions):\n%s", strings.Join(violations, "\n"))
	}
}

// Each exception must describe a deliberate non-diagnostic terminal interaction.
var allowedInteractiveDirectOutput = map[string]bool{
	// Explicit authentication paths run only before TUI ownership; refresh
	// status lines are guarded by runtimeoutput.Active in provider constructors.
	"chatgpt.go:NewChatGPTProviderWithOptions:fmt.Fprintln(os.Stderr)": true,
	"chatgpt.go:PromptForChatGPTAuth:fmt.Fprintln(os.Stderr)":          true,
	"chatgpt.go:runChatGPTDeviceCodeFlow:fmt.Fprint(os.Stderr)":        true,
	"chatgpt.go:runChatGPTDeviceCodeFlow:fmt.Fprintf(os.Stderr)":       true,
	"chatgpt.go:runChatGPTDeviceCodeFlow:fmt.Fprintln(os.Stderr)":      true,
	"chatgpt.go:runChatGPTBrowserFlow:fmt.Fprint(os.Stderr)":           true,
	"copilot.go:NewCopilotProvider:fmt.Println":                        true,
	"copilot.go:PromptForCopilotAuth:fmt.Println":                      true,
	"copilot.go:PromptForCopilotAuth:fmt.Print":                        true,
	"auth_prompt.go:waitForEnterOrInterrupt:fmt.Println":               true,
	// The forced-exit path runs only after Bubble Tea cannot stop and os.Exit.
	"chat.go:runChatOnce:fmt.Fprintln(os.Stderr)": true,
	// These are standalone CLI-only paths, not background chat workers.
	"skills.go:runSkillsAddCLI:fmt.Fprintln(os.Stderr)":             true,
	"sessions.go:buildSessionExportOptions:fmt.Fprintln(os.Stderr)": true,
}
