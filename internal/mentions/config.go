package mentions

import (
	"os"
	"strings"
)

// EnabledFromEnv reports whether project mentions are enabled for this process.
func EnabledFromEnv() bool {
	value := strings.TrimSpace(os.Getenv("TERM_LLM_AT_MENTIONS"))
	return value != "0" && !strings.EqualFold(value, "false")
}
