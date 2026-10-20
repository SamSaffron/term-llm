//go:build !js && !wasip1

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestBrowserDoesNotLinkServerGFMInitializer(t *testing.T) {
	// Goldmark's extension initializer compiles two URL regexps whose
	// simplified trees have depth 512. This overflowed Go's native WASM stack
	// on iPhone before main. Check the complete transitive graph, not just the
	// direct import that originally introduced it through session HTML export.
	cmd := exec.Command("go", "list", "-deps", ".")
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm", "CGO_ENABLED=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("inspect browser dependency graph: %v\n%s", err, output)
	}
	for _, dependency := range strings.Fields(string(output)) {
		if dependency == "github.com/yuin/goldmark/extension" {
			t.Fatal("browser links server-side GFM initializer, which overflows the iPhone WASM stack")
		}
	}
}
