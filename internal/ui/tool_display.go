package ui

// ToolPhase contains display strings for a tool execution phase.
type ToolPhase struct {
	// Active is the phase text shown during execution (e.g., "web_search(cats)")
	Active string
	// Completed is the text shown after completion (e.g., "web_search(cats)")
	Completed string
}

// FormatToolPhase returns display strings for a tool based on name and preview info.
// Uses unified format: name + info (where info contains parenthesized args).
// Example: web_search(cats), read_file(/src/main.go)
func FormatToolPhase(name, info string) ToolPhase {
	display := name + info
	return ToolPhase{
		Active:    display,
		Completed: display,
	}
}
