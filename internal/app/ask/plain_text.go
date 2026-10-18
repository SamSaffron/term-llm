package ask

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/terminaltext"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

// StreamPlainText streams text directly without formatting
func StreamPlainText(ctx context.Context, events <-chan ui.StreamEvent, suppressToolStatus bool, stderr io.Writer) error {
	if stderr == nil {
		stderr = os.Stderr
	}
	// Track pending tools with their status
	type toolEntry struct {
		callID  string
		name    string
		info    string
		success bool
		done    bool
	}
	var pendingTools []toolEntry
	seenToolStarts := make(map[string]bool) // Track seen tool starts to prevent duplicates
	seenToolEnds := make(map[string]bool)   // Track seen tool ends to prevent duplicates
	printedAny := false
	trailingNewlines := 0
	afterToolBoundary := false
	lastPrintedType := ui.SegmentText
	newlineCompactor := ui.NewStreamingNewlineCompactor(ui.MaxStreamingConsecutiveNewlines)
	showToolStatus := !suppressToolStatus
	var pendingText strings.Builder
	mediaByReference := make(map[string]llm.MediaArtifact)

	flushPendingText := func() {
		text := ui.ReplaceMediaMarkdown(pendingText.String(), mediaByReference)
		pendingText.Reset()
		if text == "" {
			return
		}
		if afterToolBoundary {
			fmt.Print(ui.NewlinePadding(trailingNewlines, ui.FinalSpacerTrailingNewlines))
			trailingNewlines = ui.FinalSpacerTrailingNewlines
			text = strings.TrimLeft(text, "\n")
			afterToolBoundary = false
		}
		if text != "" {
			fmt.Print(text)
			printedAny = true
			trailingNewlines = ui.CountTrailingNewlines(text)
			lastPrintedType = ui.SegmentText
		}
	}

	printTools := func() {
		flushPendingText()
		if !showToolStatus || len(pendingTools) == 0 {
			return
		}
		if printedAny {
			targetTrailing := ui.SegmentBoundaryTrailingNewlines(lastPrintedType, ui.SegmentTool)
			fmt.Print(ui.NewlinePadding(trailingNewlines, targetTrailing))
		}
		for _, t := range pendingTools {
			phase := ui.FormatToolPhase(t.name, t.info)
			if t.success {
				fmt.Printf("%s %s\n", ui.SuccessCircle(), phase.Completed)
			} else {
				fmt.Printf("%s %s\n", ui.ErrorCircle(), phase.Completed)
			}
		}
		pendingTools = nil
		printedAny = true
		trailingNewlines = ui.SectionBreakTrailingNewlines
		afterToolBoundary = true
		lastPrintedType = ui.SegmentTool
	}

	ensureFinalSpacer := func() {
		if !printedAny {
			fmt.Println()
			return
		}
		fmt.Print(ui.NewlinePadding(trailingNewlines, ui.FinalSpacerTrailingNewlines))
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				if showToolStatus && len(pendingTools) > 0 {
					printTools()
				}
				ensureFinalSpacer()
				return nil
			}

			switch ev.Type {
			case ui.StreamEventAttemptDiscard:
				fmt.Fprint(stderr, "\nInterrupted response discarded; retrying...\n")
				pendingText.Reset()
				newlineCompactor = ui.NewStreamingNewlineCompactor(ui.MaxStreamingConsecutiveNewlines)
				afterToolBoundary = false
				trailingNewlines = 0
				continue
			case ui.StreamEventRetry:
				fmt.Fprintf(stderr, "\r%s\n", ev.RetryStatus("Rate limited", 0, "..."))

			case ui.StreamEventUsage:
				// Skip usage events in plain text mode
				continue

			case ui.StreamEventPhase:
				// Print WARNING phases to stderr, skip others
				if strings.HasPrefix(ev.Phase, llm.WarningPhasePrefix) {
					fmt.Fprintf(stderr, "\n%s\n", terminaltext.SanitizeSingleLine(ev.Phase))
				}
				continue

			case ui.StreamEventToolEnd:
				if !showToolStatus {
					continue
				}
				// Skip duplicate tool end events (only dedupe when ToolCallID is not empty)
				if ev.ToolCallID != "" {
					if seenToolEnds[ev.ToolCallID] {
						continue
					}
					seenToolEnds[ev.ToolCallID] = true
				}
				// Find and update the tool entry by callID
				for i := range pendingTools {
					if pendingTools[i].callID == ev.ToolCallID && !pendingTools[i].done {
						pendingTools[i].success = ev.ToolSuccess
						pendingTools[i].done = true
						break
					}
				}
				// Check if all tools are done
				allDone := true
				for _, t := range pendingTools {
					if !t.done {
						allDone = false
						break
					}
				}
				if allDone && len(pendingTools) > 0 {
					printTools()
				}

			case ui.StreamEventToolStart:
				flushPendingText()
				if !showToolStatus {
					continue
				}
				// Skip duplicate tool start events (only dedupe when ToolCallID is not empty)
				if ev.ToolCallID != "" {
					if seenToolStarts[ev.ToolCallID] {
						continue
					}
					seenToolStarts[ev.ToolCallID] = true
				}
				pendingTools = append(pendingTools, toolEntry{
					callID: ev.ToolCallID,
					name:   ev.ToolName,
					info:   ev.ToolInfo,
				})

			case ui.StreamEventGuardian:
				WriteGuardianStatus(stderr, ev.Guardian)

			case ui.StreamEventText:
				text := newlineCompactor.CompactChunk(terminaltext.Sanitize(ev.Text))
				if text != "" {
					pendingText.WriteString(text)
				}

			case ui.StreamEventImage:
				flushPendingText()
				// Display image inline in plain text mode
				if ev.ImagePath != "" {
					if rendered := ui.RenderInlineImage(ev.ImagePath); rendered != "" {
						fmt.Print(rendered)
						fmt.Print("\r\n") // CR+LF to reset cursor position after image
						printedAny = true
						trailingNewlines = 1
						lastPrintedType = ui.SegmentImage
					}
				}

			case ui.StreamEventMedia:
				if reference := strings.ToLower(strings.TrimSpace(ev.Media.Reference)); reference != "" {
					mediaByReference[reference] = ev.Media
				}

			case ui.StreamEventDone:
				flushPendingText()
				if showToolStatus && len(pendingTools) > 0 {
					printTools()
				}
				ensureFinalSpacer()
				return nil

			case ui.StreamEventError:
				return ev.Err
			}
		}
	}
}

// WriteGuardianStatus writes a sanitized guardian status line to w.
func WriteGuardianStatus(w io.Writer, event tools.GuardianEvent) {
	if w == nil {
		return
	}
	if message := strings.TrimSpace(terminaltext.SanitizeSingleLine(event.Message)); message != "" {
		fmt.Fprintln(w, message)
	}
}
