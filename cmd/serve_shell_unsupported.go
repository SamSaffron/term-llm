//go:build windows || plan9 || js || wasip1

package cmd

func platformServeShellSupported() bool { return false }

func startServeShellProcess(_ string, _, _ int, _ func([]byte)) (serveShellProcess, error) {
	return nil, errServeShellUnsupported
}
