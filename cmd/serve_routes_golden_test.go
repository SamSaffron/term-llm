package cmd

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"
)

// TestServeRoutesGolden snapshots the actual ServeMux registrations, including
// their immediate auth wrapper. The passkey and Hub outer auth middleware is
// recorded separately because it wraps the entire mux, not individual routes.
// This is deliberately a source-level invariant: net/http has no API to
// enumerate a ServeMux's registered patterns or its middleware chain.
func TestServeRoutesGolden(t *testing.T) {
	sources := map[string]string{
		"web":        "serve.go",
		"hub":        "serve_hub_auth.go",
		"passkey":    "serve_hub_passkey.go",
		"widgets":    "serve_widgets.go",
		"extensions": "serve_extensions.go",
	}
	var routes []string
	for group, source := range sources {
		f, err := parser.ParseFile(token.NewFileSet(), source, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		// Authentication around the entire mux is just as important as the
		// per-route s.auth calls. Fail if either wrapper is removed.
		if group == "hub" && !hasRouteWrapper(f, "auth", "mux") {
			t.Fatal("Hub mux lost its auth wrapper")
		}
		if group == "web" && !hasRouteWrapper(f, "passkeyAuth", "handler") {
			t.Fatal("Web mux lost its outer passkey auth wrapper")
		}
		ast.Inspect(f, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
				return true
			}
			var pattern string
			switch p := call.Args[0].(type) {
			case *ast.BasicLit:
				pattern, err = strconv.Unquote(p.Value)
				if err != nil {
					t.Fatal(err)
				}
			case *ast.Ident:
				if p.Name != "hubNodeOrderPath" {
					return true
				}
				pattern = apphub.NodeOrderPath
			default:
				return true // outer base-path mounting, not an inner route
			}
			if !strings.HasPrefix(pattern, "/") && !strings.Contains(pattern, " /") {
				return true
			}
			method := "*"
			if parts := strings.SplitN(pattern, " ", 2); len(parts) == 2 {
				method, pattern = parts[0], parts[1]
			}
			auth := "public"
			if group == "hub" {
				switch pattern {
				case "/healthz", "/dist/hub.js", "/dist/hub.css":
					auth = "public"
				case "/api/connect", "/api/register-node", "/api/register-node/":
					auth = "node-or-registration-auth" // handler verifies its own credential
				case apphub.NodeOrderPath, "/api/delegations", "/api/delegations/":
					auth = "hub-or-node-auth" // conditional node-auth bypass
				default:
					auth = "hub-mux-auth"
				}
			} else if group == "passkey" {
				auth = "outer-passkey-auth" // Web and Hub both use these routes
			} else if containsServeAuth(call.Args[1]) {
				auth = "serve-auth"
			}
			routes = append(routes, group+" "+method+" "+pattern+" "+auth)
			return true
		})
	}
	sort.Strings(routes)
	got := []byte(strings.Join(routes, "\n") + "\n")
	golden := filepath.Join("testdata", "routes.golden")
	if os.Getenv("UPDATE_SERVE_ROUTES_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("serve HTTP routes or auth wrappers changed; regenerate only after reviewing routes.golden\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func hasRouteWrapper(file *ast.File, name, argument string) bool {
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return true
		}
		if arg, ok := call.Args[0].(*ast.Ident); ok && arg.Name == argument {
			found = true
		}
		return true
	})
	return found
}

func containsServeAuth(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "auth" {
				found = true
			}
		}
		return true
	})
	return found
}
