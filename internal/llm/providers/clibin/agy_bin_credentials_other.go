//go:build !darwin

package clibin

var agyPlatformHasCredentials = func(string) bool { return false }

func prepareAgyPlatformCredentials(string, string) (bool, error) { return false, nil }
