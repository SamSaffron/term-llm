package termimage

// KittyDeleteVisibleSequence returns a Kitty graphics command that deletes all
// image placements currently visible on the terminal screen and asks the
// terminal to free associated image data where possible. It is useful when
// leaving an alternate-screen UI so Kitty/Ghostty placements do not bleed into
// the restored main screen.
func KittyDeleteVisibleSequence() string {
	return "\x1b_Ga=d,d=A,q=2\x1b\\"
}

// CleanupSequence returns terminal image cleanup bytes appropriate for env.
// It is intentionally conservative: only Kitty-style graphics currently need an
// explicit cleanup command for alt-screen teardown.
func CleanupSequence(env Environment) string {
	forced := normalizeProtocol(Protocol(env.ForcedProtocol))
	if forced == ProtocolNone || forced == ProtocolANSI {
		return ""
	}
	if forced == ProtocolKitty || detectCapabilities(env).kitty {
		return KittyDeleteVisibleSequence()
	}
	return ""
}
