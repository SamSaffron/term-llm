package cmd

import (
	"time"
)

func serveContractDefaultOptions() ServeOptions {
	return ServeOptions{
		Host: "127.0.0.1", Port: 8080, AuthMode: "bearer", BasePath: "/ui",
		SessionTTL: 30 * time.Minute, SessionMax: 1000, TelegramCarryoverChars: 4000,
		JobsWorkers: 4, SidebarSessions: "all", ResponseTimeout: 30 * time.Minute,
		MaxTurns: 200, HubConnect: "direct",
	}
}
