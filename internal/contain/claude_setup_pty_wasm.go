//go:build js && wasm

package contain

import "io"

// runClaudeSetupTokenInPTY is unavailable on WebAssembly: there are no
// subprocesses or pseudo-terminals, so the caller falls back to its
// non-interactive path.
func runClaudeSetupTokenInPTY(stdin io.Reader, stdout io.Writer) (string, bool, error) {
	return "", false, nil
}
