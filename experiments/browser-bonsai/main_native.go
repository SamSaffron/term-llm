//go:build !js && !wasip1

package main

import (
	"fmt"
	"os"
)

// The browser demo only runs as js/wasm. This stub keeps repository-wide
// `go build ./...` and `go test ./...` working without producing a useful
// native binary.
func main() {
	fmt.Fprintln(os.Stderr, "browser-bonsai only runs as js/wasm; build with GOOS=js GOARCH=wasm")
	os.Exit(1)
}
