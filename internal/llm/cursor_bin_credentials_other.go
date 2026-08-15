//go:build !darwin

package llm

func prepareCursorPlatformCredentials(_, _ string) error { return nil }
func prepareCursorFileCredentials(_, _ string) error     { return nil }
