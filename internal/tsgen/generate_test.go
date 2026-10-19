package tsgen

import (
	"os"
	"path/filepath"
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
