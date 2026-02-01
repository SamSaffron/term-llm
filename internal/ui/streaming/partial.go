package streaming

import (
	"strings"
)

// partialState tracks the state of partial block rendering for re-rendering.
type partialState struct {
	safeMarkdown string // Markdown content up to incomplete syntax
	safeRendered string // Rendered output currently displayed
	lineCount    int    // Terminal lines occupied by partial output
	outputLen    int    // Bytes already written (for flowing mode)
}

// currentBlockContent returns the current incomplete block content
// by combining pending lines and any partial line in the buffer.
func (sr *StreamRenderer) currentBlockContent() string {
	var content strings.Builder
	for _, line := range sr.pendingLines {
		content.WriteString(line)
	}
	if sr.lineBuf.Len() > 0 {
		content.WriteString(sr.lineBuf.String())
	}
	return content.String()
}

// renderPartialBlock renders safe content from an incomplete block.
// In altscreen mode (with termCtrl), it clears and re-renders.
// In flowing mode (no termCtrl), partial rendering is disabled since
// there is no cursor control to clear and re-render.
func (sr *StreamRenderer) renderPartialBlock() error {
	// Disable partial rendering in flowing mode - it provides no benefit
	// without cursor control to clear and re-render
	if sr.termCtrl == nil {
		return nil
	}

	content := sr.currentBlockContent()
	if len(content) == 0 {
		return nil
	}

	// Find where incomplete inline syntax starts
	safePoint := sr.findSafePoint(content)
	safeContent := content[:safePoint]

	// Skip if no safe content or unchanged
	if len(safeContent) == 0 || safeContent == sr.partialState.safeMarkdown {
		return nil
	}

	// Clear previous partial output if any
	if sr.partialState.lineCount > 0 {
		if err := sr.termCtrl.ClearLines(sr.partialState.lineCount); err != nil {
			return err
		}
	}

	// Render the safe content
	rendered, err := sr.renderPartial(safeContent)
	if err != nil {
		return err
	}

	// Write to output
	if _, err := sr.output.Write([]byte(rendered)); err != nil {
		return err
	}

	// Update state
	sr.partialState.safeMarkdown = safeContent
	sr.partialState.safeRendered = rendered
	sr.partialState.lineCount = sr.termCtrl.CountLines(rendered)
	return nil
}

// renderPartial renders safe content with appropriate context.
// For paragraph content, it renders directly. For other block types,
// it may need to wrap in appropriate context.
func (sr *StreamRenderer) renderPartial(content string) (string, error) {
	// For now, render the content directly through glamour
	// The content should be renderable as-is since we've identified the safe point
	rendered, err := sr.tr.Render(content)
	if err != nil {
		return "", err
	}

	// Normalize consecutive newlines to fix inconsistent header spacing
	rendered = string(normalizeNewlines([]byte(rendered)))

	// Strip trailing newlines from partial render since we'll re-render later
	rendered = strings.TrimRight(rendered, "\n")

	return rendered, nil
}

// findSafePoint finds the position in content up to which we can safely render.
// This is the position just before any incomplete inline syntax.
//
// Returns the byte offset up to which content is "safe" to render.
func (sr *StreamRenderer) findSafePoint(content string) int {
	// Track potential incomplete syntax markers
	// We scan backwards from the end to find the first unclosed marker

	n := len(content)
	if n == 0 {
		return 0
	}

	// Find the last position that's safe to render
	// We need to check for unclosed inline syntax markers:
	// - ** or __ (bold)
	// - * or _ (italic)
	// - ` (inline code)
	// - ~~ (strikethrough)
	// - [ (link text start)

	safePoint := n

	// Check for unclosed inline markers by scanning the content
	// and tracking open/close state

	i := 0
	for i < n {
		// Check for escape sequences
		if content[i] == '\\' && i+1 < n {
			i += 2
			continue
		}

		// Check for code spans (backticks) - they have special rules
		if content[i] == '`' {
			// Count consecutive backticks
			start := i
			backtickCount := 0
			for i < n && content[i] == '`' {
				backtickCount++
				i++
			}

			// Look for closing backticks
			closePattern := strings.Repeat("`", backtickCount)
			closePos := strings.Index(content[i:], closePattern)
			if closePos == -1 {
				// Unclosed code span - safe point is before the backticks
				if start < safePoint {
					safePoint = start
				}
			} else {
				// Skip past the closing backticks
				i += closePos + backtickCount
			}
			continue
		}

		// Check for ** or __ (bold)
		if (content[i] == '*' || content[i] == '_') && i+1 < n && content[i+1] == content[i] {
			marker := string([]byte{content[i], content[i]})
			start := i
			i += 2

			// Look for closing marker
			closePos := strings.Index(content[i:], marker)
			if closePos == -1 {
				// Unclosed bold - safe point is before the marker
				if start < safePoint {
					safePoint = start
				}
			} else {
				// Skip past the closing marker
				i += closePos + 2
			}
			continue
		}

		// Check for * or _ (italic) - single marker
		if content[i] == '*' || content[i] == '_' {
			marker := string(content[i])
			start := i
			i++

			// Look for closing marker (but not **)
			closePos := -1
			searchPos := i
			for searchPos < n {
				pos := strings.Index(content[searchPos:], marker)
				if pos == -1 {
					break
				}
				actualPos := searchPos + pos
				// Make sure it's not ** or __
				if actualPos+1 >= n || content[actualPos+1] != content[actualPos] {
					// Also make sure the previous char isn't the same marker
					if actualPos == searchPos || content[actualPos-1] != content[actualPos] {
						closePos = actualPos
						break
					}
				}
				searchPos = actualPos + 1
			}

			if closePos == -1 {
				// Unclosed italic - safe point is before the marker
				if start < safePoint {
					safePoint = start
				}
			} else {
				i = closePos + 1
			}
			continue
		}

		// Check for ~~ (strikethrough)
		if content[i] == '~' && i+1 < n && content[i+1] == '~' {
			start := i
			i += 2

			closePos := strings.Index(content[i:], "~~")
			if closePos == -1 {
				if start < safePoint {
					safePoint = start
				}
			} else {
				i += closePos + 2
			}
			continue
		}

		// Check for [ (link start)
		if content[i] == '[' {
			start := i
			i++

			// Look for ]( or ][ to confirm it's a link
			depth := 1
			foundClose := false
			for i < n && depth > 0 {
				if content[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if content[i] == '[' {
					depth++
				} else if content[i] == ']' {
					depth--
					if depth == 0 {
						// Check if followed by ( or [
						if i+1 < n && (content[i+1] == '(' || content[i+1] == '[') {
							// It's a link, find the closing ) or ]
							opener := content[i+1]
							closer := byte(')')
							if opener == '[' {
								closer = ']'
							}
							i += 2
							parenDepth := 1
							for i < n && parenDepth > 0 {
								if content[i] == '\\' && i+1 < n {
									i += 2
									continue
								}
								if content[i] == opener {
									parenDepth++
								} else if content[i] == closer {
									parenDepth--
								}
								i++
							}
							if parenDepth == 0 {
								foundClose = true
							}
						} else {
							// Just text in brackets, continue
							foundClose = true
							i++
						}
					}
				}
				if depth > 0 {
					i++
				}
			}

			if !foundClose {
				if start < safePoint {
					safePoint = start
				}
			}
			continue
		}

		i++
	}

	// Trim trailing whitespace from safe point for cleaner output
	for safePoint > 0 && (content[safePoint-1] == ' ' || content[safePoint-1] == '\t') {
		// Keep at least one space if there's content before it
		if safePoint > 1 {
			safePoint--
		} else {
			break
		}
	}

	return safePoint
}

// clearPartialState clears the partial rendering state and
// removes partial output from terminal before emitting complete block.
func (sr *StreamRenderer) clearPartialState() error {
	if sr.partialState.lineCount > 0 && sr.termCtrl != nil {
		if err := sr.termCtrl.ClearLines(sr.partialState.lineCount); err != nil {
			return err
		}
	}

	sr.partialState = partialState{}
	return nil
}
