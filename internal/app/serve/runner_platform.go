package serve

import (
	"strings"

	runpkg "github.com/samsaffron/term-llm/internal/run"

	"github.com/samsaffron/term-llm/internal/session"
)

func templatePlatform(platform string) string {
	switch strings.TrimSpace(platform) {
	case runpkg.PlatformJob, "job":
		return "jobs"
	case runpkg.PlatformConsole:
		return "console"
	case runpkg.PlatformWeb:
		return "web"
	case runpkg.PlatformTelegram:
		return "telegram"
	case runpkg.PlatformChat:
		return "chat"
	case runpkg.PlatformExec:
		return "exec"
	default:
		return strings.TrimSpace(platform)
	}
}

func sessionModeForPlatform(platform string) session.SessionMode {
	switch templatePlatform(platform) {
	case "chat", "web", "telegram":
		return session.ModeChat
	case "exec":
		return session.ModeExec
	default:
		return session.ModeAsk
	}
}

func sessionOriginForPlatform(platform string) session.SessionOrigin {
	switch templatePlatform(platform) {
	case "web":
		return session.OriginWeb
	case "telegram":
		return session.OriginTelegram
	default:
		return session.OriginTUI
	}
}
