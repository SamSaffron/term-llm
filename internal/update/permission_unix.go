//go:build !windows

package update

func permissionHint() string { return "; try running with sudo" }
