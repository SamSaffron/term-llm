//go:build !windows

package pathutil

func parsePracticalFileURL(string) (string, bool) { return "", false }

func parseNativeFileURLHost(string, string) (string, bool) { return "", false }

func normalizeNativeFileURLPath(path string) string { return path }
