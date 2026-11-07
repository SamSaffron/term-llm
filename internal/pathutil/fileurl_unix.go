//go:build !windows

package pathutil

func parseNativeFileURLPath(string) (string, bool) { return "", false }

func parseNativeFileURLHost(string, string) (string, bool) { return "", false }

func normalizeNativeFileURLPath(path string) string { return path }
