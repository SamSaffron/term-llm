package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListForValidationKeepsFirstValidSkill(t *testing.T) {
	var paths []searchPath
	for i := 0; i < 2; i++ {
		root := t.TempDir()
		dir := filepath.Join(root, "same")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: same\ndescription: valid skill\n---\nBody"), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, searchPath{path: root, source: SourceLocal})
	}
	r := &Registry{searchPaths: paths}
	if got := r.ListForValidation(); len(got) != 1 || got[0].Path != filepath.Join(paths[0].path, "same") {
		t.Fatalf("validation results = %+v", got)
	}
}

func TestListForValidationIncludesSkippedInvalidManifests(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"valid":  "---\nname: valid\ndescription: a valid skill\n---\nBody",
		"broken": "---\nname: [not valid yaml\n---\nBody",
	} {
		path := filepath.Join(dir, name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r := &Registry{searchPaths: []searchPath{{path: dir, source: SourceLocal}}, shadowCounts: make(map[string]int), cache: make(map[string]*Skill)}
	valid, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) != 1 {
		t.Fatalf("List() returned %d valid skills", len(valid))
	}
	results := r.ListForValidation()
	if len(results) != 2 {
		t.Fatalf("ListForValidation() returned %d candidates", len(results))
	}
	var broken bool
	for _, result := range results {
		if strings.HasSuffix(result.Path, "broken") {
			broken = result.Err != nil && result.Skill == nil
		}
	}
	if !broken {
		t.Fatalf("missing invalid skill diagnostic: %+v", results)
	}
}
