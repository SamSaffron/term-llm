package ask

import "github.com/samsaffron/term-llm/internal/app/runtime/oncomplete"

// WriteCommitEditMsg writes the commit message to .git/COMMIT_EDITMSG and .git/GITGUI_MSG.
func WriteCommitEditMsg(message string) error { return oncomplete.WriteCommitEditMsg(message) }

// OnCompleteResult contains output captured from an on-complete hook.
type OnCompleteResult = oncomplete.Result

// RunOnCompleteCapture executes the on_complete shell command with input piped to stdin.
func RunOnCompleteCapture(command, input string) (OnCompleteResult, error) {
	return oncomplete.RunCapture(command, input)
}

// RunOnCompleteCaptureInDir executes an on_complete hook relative to baseDir.
func RunOnCompleteCaptureInDir(command, input, baseDir string) (OnCompleteResult, error) {
	return oncomplete.RunCaptureInDir(command, input, baseDir)
}

// RunOnComplete executes the on_complete shell command with input piped to stdin.
func RunOnComplete(command, input string) error { return oncomplete.Run(command, input) }

// TextCollector wraps an event channel and collects all text events.
type TextCollector = oncomplete.TextCollector

// NewTextCollector returns an empty event text collector.
func NewTextCollector() *TextCollector { return oncomplete.NewTextCollector() }
