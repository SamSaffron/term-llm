package ui

import (
	"fmt"
	"strings"
)

// ToolPhase contains display strings for a tool execution phase.
type ToolPhase struct {
	// Active is the phase text shown during execution (e.g., "Generating image: cat")
	Active string
	// Completed is the text shown after completion (e.g., "Generated image: cat")
	Completed string
}

// FormatToolPhase returns display strings for a tool based on name and preview info.
// The info comes from tool.Preview() and contains context like the prompt or file path.
func FormatToolPhase(name, info string) ToolPhase {
	switch name {
	case "web_search", "WebSearch":
		if info != "" {
			return ToolPhase{
				Active:    fmt.Sprintf("Searching: %s", info),
				Completed: fmt.Sprintf("Searched: %s", info),
			}
		}
		return ToolPhase{Active: "Searching", Completed: "Searched"}

	case "read_url":
		if info != "" {
			url := truncateURL(info, 50)
			return ToolPhase{
				Active:    fmt.Sprintf("Reading %s", url),
				Completed: fmt.Sprintf("Read URL: %s", url),
			}
		}
		return ToolPhase{Active: "Reading URL", Completed: "Read URL"}

	case "image_generate":
		// Preview is already formatted as "Generating image: prompt" or "Editing image: prompt"
		if info != "" {
			return ToolPhase{Active: info, Completed: info}
		}
		return ToolPhase{Active: "Generating image", Completed: "Generated image"}

	case "read_file":
		if info != "" {
			return ToolPhase{
				Active:    fmt.Sprintf("Reading %s", info),
				Completed: fmt.Sprintf("Read %s", info),
			}
		}
		return ToolPhase{Active: "Reading file", Completed: "Read file"}

	case "view_image":
		if info != "" {
			return ToolPhase{
				Active:    fmt.Sprintf("Viewing %s", info),
				Completed: fmt.Sprintf("Viewed %s", info),
			}
		}
		return ToolPhase{Active: "Viewing image", Completed: "Viewed image"}

	case "write_file":
		if info != "" {
			return ToolPhase{
				Active:    fmt.Sprintf("Writing %s", info),
				Completed: fmt.Sprintf("Wrote %s", info),
			}
		}
		return ToolPhase{Active: "Writing file", Completed: "Wrote file"}

	case "edit_file":
		if info != "" {
			return ToolPhase{
				Active:    fmt.Sprintf("Editing %s", info),
				Completed: fmt.Sprintf("Edited %s", info),
			}
		}
		return ToolPhase{Active: "Editing file", Completed: "Edited file"}

	case "shell":
		if info != "" {
			return ToolPhase{
				Active:    fmt.Sprintf("Running %s", info),
				Completed: fmt.Sprintf("Ran %s", info),
			}
		}
		return ToolPhase{Active: "Running command", Completed: "Ran command"}

	case "grep":
		if info != "" {
			return ToolPhase{
				Active:    fmt.Sprintf("Searching %s", info),
				Completed: fmt.Sprintf("Searched %s", info),
			}
		}
		return ToolPhase{Active: "Searching files", Completed: "Searched files"}

	case "glob":
		if info != "" {
			return ToolPhase{
				Active:    fmt.Sprintf("Finding %s", info),
				Completed: fmt.Sprintf("Found %s", info),
			}
		}
		return ToolPhase{Active: "Finding files", Completed: "Found files"}

	default:
		phase := "Running " + name
		if info != "" {
			return ToolPhase{
				Active:    phase,
				Completed: fmt.Sprintf("%s: %s", name, info),
			}
		}
		return ToolPhase{Active: phase, Completed: name}
	}
}

// truncateURL shortens a URL for display, keeping the domain and path start.
func truncateURL(url string, maxLen int) string {
	if len(url) <= maxLen {
		return url
	}
	// Remove protocol prefix for cleaner display
	display := strings.TrimPrefix(url, "https://")
	display = strings.TrimPrefix(display, "http://")
	if len(display) <= maxLen {
		return display
	}
	// Truncate with ellipsis
	return display[:maxLen-3] + "..."
}
