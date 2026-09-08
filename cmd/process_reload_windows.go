//go:build windows

package cmd

import "errors"

func replaceProcess(string, []string, []string) error {
	return errors.New("process replacement is not supported on Windows")
}
