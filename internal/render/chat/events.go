package chat

import (
	"encoding/json"

	"github.com/samsaffron/term-llm/internal/ui"
)

// RenderEventType identifies the type of render event
type RenderEventType int

const (
	// Message events (historical)
	RenderEventMessageAdded RenderEventType = iota
	RenderEventMessagesLoaded
	RenderEventMessagesClear

	// Streaming events
	RenderEventStreamStart
	RenderEventStreamText
	RenderEventStreamToolStart
	RenderEventStreamToolEnd
	RenderEventStreamImage
	RenderEventStreamDiff
	RenderEventStreamAskUserResult
	RenderEventStreamEnd
	RenderEventStreamError

	// UI events
	RenderEventResize
	RenderEventScroll
	RenderEventInvalidateCache
)

// RenderEvent represents an event that affects rendering.
// The renderer processes these events to update its internal state
// and produce the appropriate output.
type RenderEvent struct {
	Type RenderEventType

	// For RenderEventMessageAdded
	MessageID    int64
	MessageRole  string
	MessageIndex int

	// For RenderEventMessagesLoaded
	MessageCount int

	// For streaming text events
	Text string

	// For streaming tool events
	ToolCallID  string
	ToolName    string
	ToolInfo    string
	ToolArgs    json.RawMessage
	ToolSuccess bool

	// For streaming image/diff events
	ImagePath string
	DiffPath  string
	DiffOld   string
	DiffNew   string
	DiffLine  int

	// For ask user results
	AskUserSummary string

	// For resize events
	Width  int
	Height int

	// For scroll events
	ScrollOffset int

	// For error events
	Err error
}

// NewMessageAddedEvent creates an event for when a message is added to history
func NewMessageAddedEvent(id int64, role string, index int) RenderEvent {
	return RenderEvent{
		Type:         RenderEventMessageAdded,
		MessageID:    id,
		MessageRole:  role,
		MessageIndex: index,
	}
}

// NewMessagesLoadedEvent creates an event for when messages are loaded from storage
func NewMessagesLoadedEvent(count int) RenderEvent {
	return RenderEvent{
		Type:         RenderEventMessagesLoaded,
		MessageCount: count,
	}
}

// NewStreamStartEvent creates an event for when streaming begins
func NewStreamStartEvent() RenderEvent {
	return RenderEvent{
		Type: RenderEventStreamStart,
	}
}

// NewStreamEndEvent creates an event for when streaming ends
func NewStreamEndEvent() RenderEvent {
	return RenderEvent{
		Type: RenderEventStreamEnd,
	}
}

// NewStreamErrorEvent creates an event for streaming errors
func NewStreamErrorEvent(err error) RenderEvent {
	return RenderEvent{
		Type: RenderEventStreamError,
		Err:  err,
	}
}

// NewStreamTextEvent creates an event for streaming text content
func NewStreamTextEvent(text string) RenderEvent {
	return RenderEvent{
		Type: RenderEventStreamText,
		Text: text,
	}
}

// NewStreamToolStartEvent creates an event for when a tool starts executing
func NewStreamToolStartEvent(callID, name, info string, args json.RawMessage) RenderEvent {
	return RenderEvent{
		Type:       RenderEventStreamToolStart,
		ToolCallID: callID,
		ToolName:   name,
		ToolInfo:   info,
		ToolArgs:   args,
	}
}

// NewStreamToolEndEvent creates an event for when a tool finishes executing
func NewStreamToolEndEvent(callID, name, info string, success bool) RenderEvent {
	return RenderEvent{
		Type:        RenderEventStreamToolEnd,
		ToolCallID:  callID,
		ToolName:    name,
		ToolInfo:    info,
		ToolSuccess: success,
	}
}

// NewStreamImageEvent creates an event for when an image is generated
func NewStreamImageEvent(path string) RenderEvent {
	return RenderEvent{
		Type:      RenderEventStreamImage,
		ImagePath: path,
	}
}

// NewStreamDiffEvent creates an event for when a diff is generated
func NewStreamDiffEvent(path, old, new string, line int) RenderEvent {
	return RenderEvent{
		Type:     RenderEventStreamDiff,
		DiffPath: path,
		DiffOld:  old,
		DiffNew:  new,
		DiffLine: line,
	}
}

// NewStreamAskUserResultEvent creates an event for ask_user results
func NewStreamAskUserResultEvent(summary string) RenderEvent {
	return RenderEvent{
		Type:           RenderEventStreamAskUserResult,
		AskUserSummary: summary,
	}
}

// NewResizeEvent creates an event for terminal resize
func NewResizeEvent(width, height int) RenderEvent {
	return RenderEvent{
		Type:   RenderEventResize,
		Width:  width,
		Height: height,
	}
}

// NewScrollEvent creates an event for scroll changes
func NewScrollEvent(offset int) RenderEvent {
	return RenderEvent{
		Type:         RenderEventScroll,
		ScrollOffset: offset,
	}
}

// NewInvalidateCacheEvent creates an event to force cache invalidation
func NewInvalidateCacheEvent() RenderEvent {
	return RenderEvent{
		Type: RenderEventInvalidateCache,
	}
}

// FromStreamEvent converts a ui.StreamEvent to a RenderEvent
func FromStreamEvent(ev ui.StreamEvent) RenderEvent {
	switch ev.Type {
	case ui.StreamEventText:
		return NewStreamTextEvent(ev.Text)
	case ui.StreamEventToolStart:
		return NewStreamToolStartEvent(ev.ToolCallID, ev.ToolName, ev.ToolInfo, ev.ToolArgs)
	case ui.StreamEventToolEnd:
		return NewStreamToolEndEvent(ev.ToolCallID, ev.ToolName, ev.ToolInfo, ev.ToolSuccess)
	case ui.StreamEventImage:
		return NewStreamImageEvent(ev.ImagePath)
	case ui.StreamEventDiff:
		return NewStreamDiffEvent(ev.DiffPath, ev.DiffOld, ev.DiffNew, ev.DiffLine)
	case ui.StreamEventDone:
		return NewStreamEndEvent()
	case ui.StreamEventError:
		return NewStreamErrorEvent(ev.Err)
	default:
		// Other events (usage, phase, retry) don't affect rendering directly
		// They're handled by the main chat model for status display
		return RenderEvent{}
	}
}
