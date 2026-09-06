//go:build windows

package cmd

import "fmt"

// execReload is not supported on Windows; it returns an error.
func execReload(id, service string) error {
	return fmt.Errorf("reload is not supported on Windows")
}
