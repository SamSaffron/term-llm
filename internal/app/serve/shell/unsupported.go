//go:build windows || plan9 || js || wasip1

package shell

func Supported() bool { return false }

func startProcess(_ string, _, _ int, _ func([]byte)) (Process, error) {
	return nil, errUnsupported
}
