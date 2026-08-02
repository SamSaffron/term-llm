//go:build !darwin

package llm

func agyPlatformHasCredentials(string) bool { return false }

func prepareAgyPlatformCredentials(string, string) (bool, error) { return false, nil }
