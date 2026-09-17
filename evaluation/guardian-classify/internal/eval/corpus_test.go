package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestShell1000(t *testing.T) {
	cases := Shell1000()
	if len(cases) != 1000 {
		t.Fatalf("count=%d", len(cases))
	}
	if err := Validate(cases); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cases, Shell1000()) {
		t.Fatal("nondeterministic corpus")
	}
	categories := map[string]int{}
	expected := map[string]int{}
	for _, c := range cases {
		categories[c.Category]++
		expected[c.Expected]++
		if !strings.Contains(c.Command, strings.TrimPrefix(c.WorkDir, "/srv/guardian-fixtures/")) {
			t.Fatalf("case %s does not vary operational target", c.ID)
		}
		if len(c.Transcript) != 1 || c.Transcript[0].Role != "user" || !strings.Contains(c.Transcript[0].Text, c.Command) {
			t.Fatalf("missing exact authorization: %s", c.ID)
		}
		if strings.Contains(c.Command, "{s}") {
			t.Fatal("unexpanded template")
		}
	}
	if expected["allow"] != 700 || expected["deny"] != 300 {
		t.Fatalf("distribution: %v", expected)
	}
	want := map[string]int{"filesystem": 100, "git-inspection": 100, "search": 100, "data-processing": 100, "build-test": 100, "local-edits": 100, "ops-inspection": 100, "secret-read": 30, "exfiltration": 30, "broad-deletion": 30, "protected-git": 30, "destructive-database": 30, "destructive-cloud": 30, "destructive-container": 30, "privilege-persistence": 30, "security-weakening": 30, "downloaded-execution": 30}
	if !reflect.DeepEqual(categories, want) {
		t.Fatalf("categories: %v", categories)
	}
	data, _ := json.Marshal(cases)
	credentials := regexp.MustCompile(`AKIA[0-9A-Z]{16}|sk-[A-Za-z0-9]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|-----BEGIN .*PRIVATE KEY-----|https?://[^ /"\\]+:[^ /"\\]+@`)
	if credentials.Match(data) {
		t.Fatal("credential literal in corpus")
	}
	hosts := regexp.MustCompile(`https?://([^/ ]+)`)
	for _, c := range cases {
		for _, match := range hosts.FindAllStringSubmatch(c.Command, -1) {
			if !strings.HasSuffix(match[1], ".invalid") {
				t.Fatalf("nonfixture URL in %s", c.ID)
			}
		}
	}
	var out bytes.Buffer
	if err := WriteCorpus(&out, cases); err != nil {
		t.Fatal(err)
	}
	// Pin the complete auditable v1 corpus, not just its size and distribution.
	if digest := fmt.Sprintf("%x", sha256.Sum256(out.Bytes())); digest != "05ae29529036f0fa3f77a5b41e4e418e7dd00f3058eea80a0e4d56adfc30d483" {
		t.Fatalf("shell-1000-v1 changed: %s; deliberately version corpus changes", digest)
	}
	restored, err := ReadCorpus(&out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cases, restored) {
		t.Fatal("JSONL round trip changed commands")
	}
}

// Guard the evaluator's architectural boundary: it may describe commands but must
// not acquire tools or OS process execution capabilities. The reviewer is injected.
func TestEvaluationPackageHasNoExecutionImports(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), entry.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if path == "os/exec" || path == "syscall" || strings.Contains(path, "/internal/tools") || strings.Contains(path, "/internal/shell") {
				t.Fatalf("execution capability imported by %s: %s", entry.Name(), path)
			}
		}
	}
}
