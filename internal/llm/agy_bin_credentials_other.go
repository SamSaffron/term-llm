//go:build !darwin

package llm

var agyPlatformHasCredentials = func(string) bool { return false }

func prepareAgyPlatformCredentials(string, string) (bool, error) { return false, nil }
