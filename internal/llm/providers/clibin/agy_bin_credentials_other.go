//go:build !darwin

package clibin

// Native agy logins can live in the OS credential store, independently of HOME.
// The legacy token file is therefore not a reliable signed-in check. As with
// vibe-acp, offer the installed CLI and let it validate its own login at runtime;
// AgyBinHasCredentials checks that the binary exists before consulting this hook.
var agyPlatformHasCredentials = func(string) bool { return true }

// Keep the private HOME, but let agy load its native credentials itself when no
// legacy token is available. Do not read or copy the user's credential store or
// expose their real HOME (which would also load their agents, rules and tools).
func prepareAgyPlatformCredentials(string, string) (bool, error) { return true, nil }
