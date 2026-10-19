//go:build windows

package reload

import "errors"

func ReplaceProcess(string, []string, []string) error {
	return errors.New("process replacement is not supported on Windows")
}
