//go:build !unix

package mcpsession

import "errors"

const supported = false

var errUnsupported = errors.New("MCP session sockets are only supported on unix platforms")

func checkPrivateDir(string) error { return errUnsupported }

// Stale is always false where session sockets are unsupported.
func Stale(string) bool { return false }
