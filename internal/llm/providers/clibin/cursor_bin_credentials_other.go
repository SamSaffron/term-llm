//go:build !darwin

package clibin

func prepareCursorPlatformCredentials(_, _ string) error { return nil }
func prepareCursorFileCredentials(_, _ string) error     { return nil }
