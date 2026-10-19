package server

import "strings"

// IsLoopbackHost matches the exact loopback bind allowlist shared by web and Hub.
func IsLoopbackHost(host string) bool {
	h := strings.TrimSpace(strings.ToLower(host))
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}
