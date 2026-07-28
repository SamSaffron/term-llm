package llm

import (
	"errors"
	"fmt"
	"io"
	"strings"

	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
)

// TextStreamResult is the common output of one-shot helper conversations such
// as side questions, compaction summaries, and handover briefs.
type TextStreamResult struct {
	Text             string
	ReasoningSummary string
	Usage            Usage
}

// CollectTextStream drains a provider stream while consistently handling text,
// displayable reasoning summaries, usage, retries, and provider error events.
// The optional observer sees each successfully applied event and may stop
// collection by returning an error. EventError is always terminal, including
// malformed error events with a nil Err; observers see it before collection
// returns the provider error.
func CollectTextStream(stream Stream, observer func(Event) error) (TextStreamResult, error) {
	var text strings.Builder
	var reasoningSummary strings.Builder
	var reasoningSummaryItemID string
	var usage Usage

	result := func() TextStreamResult {
		return TextStreamResult{
			Text:             text.String(),
			ReasoningSummary: reasoningSummary.String(),
			Usage:            usage,
		}
	}

	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return result(), nil
		}
		if err != nil {
			return result(), err
		}

		switch event.Type {
		case EventTextDelta:
			text.WriteString(event.Text)
		case EventReasoningDelta:
			if isDisplayableReasoningSummaryEvent(event) {
				internalreasoning.AppendStreamItemText(&reasoningSummary, &reasoningSummaryItemID, event.Text, event.ReasoningItemID)
				if len(event.ReasoningSummaryParts) > 0 {
					reasoningSummary.Reset()
					reasoningSummary.WriteString(strings.Join(event.ReasoningSummaryParts, "\n\n"))
					if event.ReasoningItemID != "" {
						reasoningSummaryItemID = event.ReasoningItemID
					}
				}
			}
		case EventUsage:
			if event.Use != nil {
				usage.Add(*event.Use)
			}
		case EventAttemptDiscard:
			text.Reset()
			reasoningSummary.Reset()
			reasoningSummaryItemID = ""
			usage = Usage{}
		case EventError:
			if observer != nil {
				if err := observer(event); err != nil {
					return result(), err
				}
			}
			if event.Err != nil {
				return result(), event.Err
			}
			return result(), fmt.Errorf("provider stream reported an error")
		}

		if observer != nil {
			if err := observer(event); err != nil {
				return result(), err
			}
		}
	}
}
