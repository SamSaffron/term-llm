package serve

import "time"

// serveContractDefaultOptions is the neutral value fixture for the composed
// host-fixture and CLI-registration default proof. C2 duplicates this exact
// unexported helper into the host test package when that proof is split.
func serveContractDefaultOptions() Options {
	return Options{
		Host: "127.0.0.1", Port: 8080, AuthMode: "bearer", BasePath: "/ui",
		SessionTTL: 30 * time.Minute, SessionMax: 1000, TelegramCarryoverChars: 4000,
		JobsWorkers: 4, SidebarSessions: "all", ResponseTimeout: 30 * time.Minute,
		MaxTurns: 200, HubConnect: "direct",
	}
}
