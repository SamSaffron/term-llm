//go:build js || wasip1

package clipboard

import "errors"

// term-llm addition: WebAssembly hosts have no system clipboard.
var errUnsupported = errors.New("clipboard: not supported on WebAssembly")

func readAll() (string, error) { return "", errUnsupported }

func writeAll(string) error { return errUnsupported }
