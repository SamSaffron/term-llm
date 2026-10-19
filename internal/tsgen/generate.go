// Package tsgen generates the explicitly registered serve JSON contracts for the web UI.
package tsgen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Registry is intentionally a reviewable allowlist, not a scan of every serve type.
// Roots must name declarations directly, not cmd compatibility aliases: an alias
// can emit a duplicate, self-referential wire name after its declaration moves.
var Registry = []string{
	"internal/app/serve.childRunProjection", "internal/app/serve.webSessionEntry", "internal/app/serve.webSelectedSessionEntry", "internal/app/serve.createWebSessionRequest", "internal/app/serve.serveEvent", "internal/app/serve.serveSkillRunEvent", "internal/app/serve/jobs.RunEvent", "internal/app/serve.hubNodeView", "internal/app/serve.webSessionMetrics",
	"internal/app/serve.serveMentionSearchRequest", "internal/app/serve.serveMentionSearchResponse",
	"internal/app/serve.sessionShareRequest", "internal/app/serve.sessionShareResponse", "internal/app/serve.sharingCapabilitiesResponse",
	"internal/app/serve.serveShellCreateRequest", "internal/app/serve.serveShellInputRequest", "internal/app/serve.sessionInterruptRequest", "internal/app/serve.sessionRuntimeGoalRequest",
	"internal/app/serve.sessionMessagesResponse",
}

const module = "github.com/samsaffron/term-llm/"

type declaration struct {
	node    ast.Expr
	imports map[string]string
}

type generator struct {
	root     string
	packages map[string]map[string]declaration
	pending  map[string]bool
}

func newGenerator(root string) *generator {
	return &generator{root: root, packages: map[string]map[string]declaration{}, pending: map[string]bool{}}
}

func (g *generator) load(dir string) error {
	if _, ok := g.packages[dir]; ok {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(g.root, dir))
	if err != nil {
		return err
	}
	types := map[string]declaration{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(g.root, dir, name), nil, 0)
		if err != nil {
			return err
		}
		imports := map[string]string{}
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			alias := filepath.Base(path)
			if imp.Name != nil {
				alias = imp.Name.Name
			}
			imports[alias] = strings.TrimPrefix(path, module)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typ := spec.(*ast.TypeSpec)
				types[typ.Name.Name] = declaration{typ.Type, imports}
			}
		}
	}
	g.packages[dir] = types
	return nil
}

func (g *generator) find(key string) (declaration, string, error) {
	dot := strings.LastIndex(key, ".")
	if dot < 0 {
		return declaration{}, "", fmt.Errorf("invalid registry entry %q", key)
	}
	dir, name := key[:dot], key[dot+1:]
	if err := g.load(dir); err != nil {
		return declaration{}, "", fmt.Errorf("load %s: %w", dir, err)
	}
	decl, ok := g.packages[dir][name]
	if !ok {
		return declaration{}, "", fmt.Errorf("unresolved JSON type %s", key)
	}
	return decl, dir, nil
}

// Only these exact packages use bare names; serve subpackages are not included.
var bareNamePackages = map[string]bool{
	"cmd":                true,
	"internal/app/serve": true,
}

// Wire names survive Go declaration moves/renames. Add overrides only when a
// declaration enters the registered closure; RunEvent is currently the sole jobs DTO.
var wireNames = map[string]string{
	"internal/app/serve/jobs.RunEvent": "JobsV2RunEvent",
}

func tsName(key string) string {
	if name, ok := wireNames[key]; ok {
		return name
	}
	dot := strings.LastIndex(key, ".")
	dir, name := key[:dot], key[dot+1:]
	if bareNamePackages[dir] {
		return strings.ToUpper(name[:1]) + name[1:]
	}
	pkg := filepath.Base(dir)
	return strings.ToUpper(pkg[:1]) + pkg[1:] + strings.ToUpper(name[:1]) + name[1:]
}

// Keep cmd-origin contracts in their historical name order, ahead of library
// types, which retain Go-key order. A move must not change either bucket/position.
func sortKey(key string) string {
	dir := key[:strings.LastIndex(key, ".")]
	if _, overridden := wireNames[key]; overridden || bareNamePackages[dir] {
		return "0\x00" + tsName(key)
	}
	return "1\x00" + key
}

func (g *generator) typeOf(expr ast.Expr, dir string, imports map[string]string) (string, error) {
	switch t := expr.(type) {
	case *ast.Ident:
		switch t.Name {
		case "string":
			return "string", nil
		case "bool":
			return "boolean", nil
		case "any":
			return "unknown", nil
		case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "float32", "float64", "byte":
			return "number", nil
		}
		key := dir + "." + t.Name
		if _, _, err := g.find(key); err != nil {
			return "", err
		}
		g.pending[key] = true
		return tsName(key), nil
	case *ast.StarExpr:
		value, err := g.typeOf(t.X, dir, imports)
		return value + " | null", err
	case *ast.ArrayType:
		if t.Len == nil {
			if element, ok := t.Elt.(*ast.Ident); ok && (element.Name == "byte" || element.Name == "uint8") {
				return "string | null", nil
			}
		}
		value, err := g.typeOf(t.Elt, dir, imports)
		if t.Len != nil {
			return "(" + value + ")[]", err
		}
		return "(" + value + ")[] | null", err
	case *ast.MapType:
		key, err := g.typeOf(t.Key, dir, imports)
		if err != nil {
			return "", err
		}
		value, err := g.typeOf(t.Value, dir, imports)
		if err != nil {
			return "", err
		}
		if key != "string" && key != "number" {
			return "", fmt.Errorf("unsupported JSON map key %s", key)
		}
		return "Record<" + key + ", " + value + "> | null", nil
	case *ast.InterfaceType:
		return "unknown", nil
	case *ast.SelectorExpr:
		pkg := t.X.(*ast.Ident).Name
		switch pkg + "." + t.Sel.Name {
		case "time.Time":
			return "string", nil
		case "json.RawMessage":
			return "unknown", nil
		}
		dep, ok := imports[pkg]
		if !ok || !strings.HasPrefix(dep, "internal/") {
			return "", fmt.Errorf("unsupported foreign type %s.%s", pkg, t.Sel.Name)
		}
		key := dep + "." + t.Sel.Name
		if _, _, err := g.find(key); err != nil {
			return "", err
		}
		g.pending[key] = true
		return tsName(key), nil
	case *ast.StructType:
		return g.structOf(t, dir, imports)
	default:
		return "", fmt.Errorf("unsupported JSON expression %T", expr)
	}
}

func (g *generator) structOf(t *ast.StructType, dir string, imports map[string]string) (string, error) {
	var parts, embedded []string
	for _, field := range t.Fields.List {
		if len(field.Names) == 0 {
			part, embed, err := g.promotedField(field, dir, imports)
			if err != nil {
				return "", err
			}
			parts, embedded = appendNonEmpty(parts, part), appendNonEmpty(embedded, embed)
			continue
		}
		fieldParts, err := g.namedField(field, dir, imports)
		if err != nil {
			return "", err
		}
		parts = append(parts, fieldParts...)
	}
	embedded = append(embedded, "{ "+strings.Join(parts, " ")+" }")
	return strings.Join(embedded, " & "), nil
}

func appendNonEmpty(list []string, s string) []string {
	if s == "" {
		return list
	}
	return append(list, s)
}

// jsonTag returns the field's json tag options; skip reports `json:"-"`.
func jsonTag(field *ast.Field) (options []string, skip bool, err error) {
	if field.Tag == nil {
		return nil, false, nil
	}
	tag, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		return nil, false, err
	}
	raw := reflect.StructTag(tag).Get("json")
	if raw == "-" {
		return nil, true, nil
	}
	return strings.Split(raw, ","), false, nil
}

// promotedField handles an embedded field. A json-named embedded field is a
// normal property; otherwise the embedded type's properties are promoted.
func (g *generator) promotedField(field *ast.Field, dir string, imports map[string]string) (part, embed string, err error) {
	value, err := g.typeOf(field.Type, dir, imports)
	if err != nil {
		return "", "", err
	}
	options, skip, err := jsonTag(field)
	if err != nil || skip {
		return "", "", err
	}
	if len(options) > 0 && options[0] != "" {
		return strconv.Quote(options[0]) + ": " + value + ";", "", nil
	}
	return "", value, nil
}

// namedField renders the exported names declared by one struct field.
func (g *generator) namedField(field *ast.Field, dir string, imports map[string]string) ([]string, error) {
	if !field.Names[0].IsExported() {
		return nil, nil
	}
	options, skip, err := jsonTag(field)
	if err != nil || skip {
		return nil, err
	}
	name := ""
	if len(options) > 0 {
		name = options[0]
	}
	value, err := g.typeOf(field.Type, dir, imports)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", fieldLabel(name, field), err)
	}
	optional, quoted := fieldOptions(options)
	if optional {
		value = strings.TrimSuffix(value, " | null")
	}
	if quoted {
		value = "string"
	}
	mark := ""
	if optional {
		mark = "?"
	}
	var parts []string
	for _, ident := range field.Names {
		key := name
		if key == "" {
			key = ident.Name
		}
		parts = append(parts, strconv.Quote(key)+mark+": "+value+";")
	}
	return parts, nil
}

func fieldLabel(name string, field *ast.Field) string {
	if name != "" {
		return name
	}
	return field.Names[0].Name
}

func fieldOptions(options []string) (optional, quoted bool) {
	for _, opt := range options {
		switch opt {
		case "omitempty", "omitzero":
			optional = true
		case "string":
			quoted = true
		}
	}
	return optional, quoted
}

func (g *generator) render(key string) (string, error) {
	decl, dir, err := g.find(key)
	if err != nil {
		return "", err
	}
	value, err := g.typeOf(decl.node, dir, decl.imports)
	if err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	return "export type " + tsName(key) + " = " + value + ";\n", nil
}

// Generate returns deterministic, dependency-complete TypeScript source.
func Generate(root string) ([]byte, error) {
	return generate(root, Registry)
}

func generate(root string, registry []string) ([]byte, error) {
	g := newGenerator(root)
	for _, key := range registry {
		g.pending[key] = true
	}
	result := map[string]string{}
	for {
		var next string
		for key := range g.pending {
			if _, done := result[key]; !done {
				next = key
				break
			}
		}
		if next == "" {
			break
		}
		text, err := g.render(next)
		if err != nil {
			return nil, err
		}
		result[next] = text
	}
	keys := make([]string, 0, len(result))
	for key := range result {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := sortKey(keys[i]), sortKey(keys[j])
		if left == right {
			return keys[i] < keys[j]
		}
		return left < right
	})
	var out bytes.Buffer
	out.WriteString("// Code generated by make api-types; DO NOT EDIT.\n\n")
	for i, key := range keys {
		if i > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(result[key])
	}
	return out.Bytes(), nil
}
