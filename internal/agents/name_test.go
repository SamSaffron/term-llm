package agents

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLookupNameGrammar(t *testing.T) {
	for _, name := range []string{"codebase", "web-researcher", "team.alpha", "review team", "資料_2"} {
		if !IsLookupName(name) {
			t.Errorf("IsLookupName(%q) = false", name)
		}
	}
	for _, name := range []string{"", " codebase", "codebase ", "codebase,", "team..alpha", "path/name", "bad#fragment", "-leading", "trailing-"} {
		if IsLookupName(name) {
			t.Errorf("IsLookupName(%q) = true", name)
		}
	}
	tooLong := ""
	for range MaxLookupNameRunes + 1 {
		tooLong += "a"
	}
	if IsLookupName(tooLong) {
		t.Fatal("over-limit lookup name was accepted")
	}
}

func TestCreateAgentDirRejectsInvalidLookupNameBeforeFilesystemMutation(t *testing.T) {
	root := t.TempDir()
	if err := CreateAgentDir(root, "reviewer,please"); err == nil {
		t.Fatal("CreateAgentDir accepted invalid lookup name")
	}
	if _, err := os.Stat(filepath.Join(root, "reviewer,please")); !os.IsNotExist(err) {
		t.Fatalf("invalid agent directory was created: %v", err)
	}
}
