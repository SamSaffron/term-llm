package termimage

import (
	"strconv"
	"strings"
)

// KittyDeleteVisibleSequence returns a Kitty graphics command that deletes all
// image placements currently visible on the terminal screen and asks the
// terminal to free associated image data where possible. It is useful when
// leaving an alternate-screen UI so Kitty/Ghostty placements do not bleed into
// the restored main screen.
func KittyDeleteVisibleSequence() string {
	return "\x1b_Ga=d,d=A,q=2\x1b\\"
}

// KittyDeleteImageSequence returns Kitty graphics commands that delete the
// specific image ids owned by this process. Prefer this to global cleanup when
// the ids are known, so unrelated Kitty graphics are not disturbed.
func KittyDeleteImageSequence(imageIDs ...uint32) string {
	if len(imageIDs) == 0 {
		return ""
	}
	var out strings.Builder
	out.Grow(len(imageIDs) * 24)
	seen := make(map[uint32]struct{}, len(imageIDs))
	for _, id := range imageIDs {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out.WriteString("\x1b_Ga=d,i=")
		out.WriteString(strconv.FormatUint(uint64(id), 10))
		out.WriteString(",q=2\x1b\\")
	}
	return out.String()
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
