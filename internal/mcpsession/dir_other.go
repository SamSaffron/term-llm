//go:build !unix

package mcpsession

func checkPrivateDir(string) error { return nil }
