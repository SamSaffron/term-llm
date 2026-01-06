package ui

// ProgressUpdate represents a progress update during long-running operations.
type ProgressUpdate struct {
	// OutputTokens is the number of tokens generated so far.
	OutputTokens int

	// Status is the current status text (e.g., "editing main.go").
	Status string

	// Milestone is a completed milestone to print above the spinner
	// (e.g., "✓ Found edit for main.go").
	Milestone string
}
