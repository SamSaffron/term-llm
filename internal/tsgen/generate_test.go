package tsgen

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestGenerateEndsWithSingleNewline(t *testing.T) {
	got, err := Generate("../..")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(got), ";\n") || strings.HasSuffix(string(got), "\n\n") {
		t.Fatal("generated API contracts must end with exactly one newline")
	}
}

func TestJSONFieldRules(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "wire")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	source := `package wire
import ("encoding/json"; "time")
type Base struct { Kept string ` + "`json:\"kept\"`" + `; Hidden string ` + "`json:\"-\"`" + ` }
type Data struct {
 Base
 Pointer *int ` + "`json:\"pointer\"`" + `
 Optional *int ` + "`json:\"optional,omitempty\"`" + `
 When time.Time ` + "`json:\"when\"`" + `
 Quoted int ` + "`json:\"quoted,string\"`" + `
 Blob json.RawMessage ` + "`json:\"blob\"`" + `
 Bytes []byte ` + "`json:\"bytes\"`" + `
 Items []string ` + "`json:\"items\"`" + `
 Map map[string]int ` + "`json:\"map\"`" + `
 Left, Right int
}`
	if err := os.WriteFile(filepath.Join(dir, "dto.go"), []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
	g := newGenerator(root)
	got, err := g.render("internal/wire.Data")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"WireBase &", `"pointer": number | null`, `"optional"?: number`, `"when": string`, `"quoted": string`, `"blob": unknown`, `"bytes": string | null`, `"items": (string)[] | null`, `"map": Record<string, number> | null`, `"Left": number`, `"Right": number`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s from %s", want, got)
		}
	}
	if strings.Contains(got, "hidden") {
		t.Errorf("json:- leaked: %s", got)
	}
}

// Keep emission order as well as membership: the first 31 names are cmd-origin
// contracts, followed by 18 library types in Go-key order.
func TestWireNamesInventory(t *testing.T) {
	got, err := Generate("../..")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/wire_names.txt")
	if err != nil {
		t.Fatal(err)
	}
	if names := wireNameInventory(got); names != string(want) {
		t.Fatalf("wire names/order drifted (-want +got):\nwant:\n%sgot:\n%s", want, names)
	}
}

func wireNameInventory(source []byte) string {
	var names []string
	for _, line := range strings.Split(string(source), "\n") {
		if rest, ok := strings.CutPrefix(line, "export type "); ok {
			name, _, _ := strings.Cut(rest, " = ")
			names = append(names, name)
		}
	}
	return strings.Join(names, "\n") + "\n"
}

func TestTSWireNames(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{"cmd.serveEvent", "ServeEvent"},
		{"internal/app/serve.serveEvent", "ServeEvent"},
		{"internal/app/serve.ServeEvent", "ServeEvent"},
		{"internal/app/serve/jobs.RunEvent", "JobsV2RunEvent"},
		{"internal/app/serve/jobs.Job", "JobsJob"},
		{"internal/app/serve/x.Event", "XEvent"},
		{"internal/app/serveextra.Event", "ServeextraEvent"},
		{"cmd/child.Event", "ChildEvent"},
		{"internal/session.SessionMode", "SessionSessionMode"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			if got := tsName(tc.key); got != tc.want {
				t.Fatalf("tsName(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

// The synthetic library packages deliberately have opposite Go-key and TS-name
// order. Every struct is a leaf so the legacy control isolates naming/sorting.
func wireMoveFixture(t *testing.T, moveJobs, moveHost bool) (string, []string) {
	t.Helper()
	root := t.TempDir()
	write := func(dir, source string) {
		t.Helper()
		path := filepath.Join(root, dir)
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "dto.go"), []byte(source), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/aaa/zeta", "package zeta; type Type struct { Value string }")
	write("internal/zzz/alpha", "package alpha; type Type struct { Value string }")
	keys := []string{"internal/zzz/alpha.Type", "internal/aaa/zeta.Type"}
	cmd := "package cmd\n"
	if moveJobs {
		// Go callers may retain this alias, but the registry must target RunEvent.
		cmd += `import servejobs "github.com/samsaffron/term-llm/internal/app/serve/jobs"
 type jobsV2RunEvent = servejobs.RunEvent
`
		write("internal/app/serve/jobs", "package jobs; type RunEvent struct { Value string }")
		keys = append(keys, "internal/app/serve/jobs.RunEvent")
	} else {
		cmd += "type jobsV2RunEvent struct { Value string }\n"
		keys = append(keys, "cmd.jobsV2RunEvent")
	}
	if moveHost {
		write("internal/app/serve", "package serve; type HubNodeView struct { Value string }; type ServeEvent struct { Value string }")
		keys = append(keys, "internal/app/serve.ServeEvent", "internal/app/serve.HubNodeView")
	} else {
		cmd += "type hubNodeView struct { Value string }; type serveEvent struct { Value string }\n"
		keys = append(keys, "cmd.serveEvent", "cmd.hubNodeView")
	}
	write("cmd", cmd)
	return root, keys
}

func TestMovedWireNamesStable(t *testing.T) {
	root, keys := wireMoveFixture(t, false, false)
	want, err := generate(root, keys)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name               string
		moveJobs, moveHost bool
	}{
		{"jobs DTO", true, false},
		{"residual host", false, true},
		{"both moves", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, keys := wireMoveFixture(t, tc.moveJobs, tc.moveHost)
			got, err := generate(root, keys)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("moved declarations changed names, positions or bodies:\nwant:\n%sgot:\n%s", want, got)
			}
		})
	}
}

// Snapshot of the old generator's naming and raw Go-key sort for leaf fixtures.
// JSON body rendering is unchanged and shared; no named dependencies are used.
func legacyLeafGenerate(t *testing.T, root string, registry []string) []byte {
	t.Helper()
	keys := append([]string(nil), registry...)
	sort.Strings(keys)
	g := newGenerator(root)
	var out strings.Builder
	out.WriteString("// Code generated by make api-types; DO NOT EDIT.\n\n")
	for i, key := range keys {
		decl, dir, err := g.find(key)
		if err != nil {
			t.Fatal(err)
		}
		value, err := g.typeOf(decl.node, dir, decl.imports)
		if err != nil {
			t.Fatal(err)
		}
		if len(g.pending) != 0 {
			t.Fatal("legacy leaf control must not contain named dependencies")
		}
		name := key[strings.LastIndex(key, ".")+1:]
		if !strings.HasPrefix(key, "cmd.") {
			pkg := filepath.Base(dir)
			name = strings.ToUpper(pkg[:1]) + pkg[1:] + strings.ToUpper(name[:1]) + name[1:]
		} else {
			name = strings.ToUpper(name[:1]) + name[1:]
		}
		if i > 0 {
			out.WriteByte('\n')
		}
		fmt.Fprintf(&out, "export type %s = %s;\n", name, value)
	}
	return []byte(out.String())
}

func TestMovedWireNamesLegacyNegativeControl(t *testing.T) {
	root, keys := wireMoveFixture(t, false, false)
	want, err := generate(root, keys)
	if err != nil {
		t.Fatal(err)
	}
	if old := legacyLeafGenerate(t, root, keys); string(old) != string(want) {
		t.Fatal("legacy control must match before declarations move")
	}
	root, keys = wireMoveFixture(t, true, true)
	old := legacyLeafGenerate(t, root, keys)
	if string(old) == string(want) {
		t.Fatal("old generator unexpectedly preserved moved contracts")
	}
	for _, drift := range []string{"JobsRunEvent", "ServeHubNodeView", "ServeServeEvent"} {
		if !strings.Contains(wireNameInventory(old), drift+"\n") {
			t.Errorf("legacy control did not expose %s drift:\n%s", drift, old)
		}
	}
}

func TestMovedWireNamesCmdAliasNegativeControl(t *testing.T) {
	root, keys := wireMoveFixture(t, true, false)
	want, err := generate(root, keys)
	if err != nil {
		t.Fatal(err)
	}
	for i, key := range keys {
		if key == "internal/app/serve/jobs.RunEvent" {
			keys[i] = "cmd.jobsV2RunEvent"
		}
	}
	got, err := generate(root, keys)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == string(want) || wireNameInventory(got) == wireNameInventory(want) {
		t.Fatal("retaining a cmd alias root must cause detectable contract/inventory drift")
	}
	if count := strings.Count(string(got), "export type JobsV2RunEvent ="); count != 2 {
		t.Fatalf("alias root should emit two JobsV2RunEvent declarations, got %d:\n%s", count, got)
	}
	if !strings.Contains(string(got), "export type JobsV2RunEvent = JobsV2RunEvent;") {
		t.Fatalf("expected the self-referential compatibility alias:\n%s", got)
	}
}
