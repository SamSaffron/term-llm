//go:build windows

package contain

import "io"

func runClaudeSetupTokenInPTY(stdin io.Reader, stdout io.Writer) (string, bool, error) {
	return "", false, nil
}
