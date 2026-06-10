package chat

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Footer debug instrumentation, enabled with TERM_LLM_FOOTER_DEBUG=1.
// Logs alt-screen frame-height mismatches (which clip the footer's status
// line off the bottom of the terminal) and blank status lines to
// $TMPDIR/term-llm-footer-debug.log for diagnosing intermittent footer loss.
var (
	footerDebugOnce sync.Once
	footerDebugPath string
)

func footerDebugEnabled() bool {
	footerDebugOnce.Do(func() {
		if os.Getenv("TERM_LLM_FOOTER_DEBUG") != "" {
			footerDebugPath = filepath.Join(os.TempDir(), "term-llm-footer-debug.log")
		}
	})
	return footerDebugPath != ""
}

func footerDebugf(format string, args ...any) {
	if !footerDebugEnabled() {
		return
	}
	f, err := os.OpenFile(footerDebugPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, time.Now().Format("15:04:05.000")+" "+format+"\n", args...)
}
