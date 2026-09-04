package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/tools"
)

func (s *serveShell) appendActivityOutputLocked(start int64, data []byte) {
	s.appendActivityLocked(start, data)
}

func (s *serveShell) recordBrowserInputLocked(data []byte) {
	if len(data) != 0 {
		s.browserInputRevision++
	}
}

func (s *serveShell) beginProtocolDisplayRedactionLocked(payload []byte, marker byte, replacement []byte) {
	s.displayRedactionStart = s.nextOffset
	s.displayRedactionUntil = s.nextOffset + 4096 + int64(len(payload))
	s.displayRedactionMarker = marker
	s.displayRedactionReplacement = append(s.displayRedactionReplacement[:0], replacement...)
	// Browser output remains pinned at the pre-injection offset until the
	// authoritative nonce-bound marker is parsed or the protocol write fails.
	s.displayRedactionHeld = true
}

func (s *serveShell) releaseProtocolDisplayRedactionLocked() bool {
	held := s.displayRedactionHeld
	s.displayRedactionStart = -1
	s.displayRedactionUntil = 0
	s.displayRedactionMarker = 0
	s.displayRedactionReplacement = s.displayRedactionReplacement[:0]
	s.displayRedactionHeld = false
	return held
}

func (s *serveShell) maskReplayRangeLocked(start, end int64) {
	start = max64(start, s.baseOffset)
	end = min64(end, s.nextOffset)
	if start >= end {
		return
	}
	for i := int(start - s.baseOffset); i < int(end-s.baseOffset); i++ {
		s.output[i] = 0
	}
}

func (s *serveShell) replaceReplayRangeLocked(start, end int64, replacement []byte) {
	s.maskReplayRangeLocked(start, end)
	copyStart := max64(start, s.baseOffset)
	copyEnd := min64(end, s.nextOffset)
	replacementStart := copyStart - start
	if replacementStart >= int64(len(replacement)) || copyStart >= copyEnd {
		return
	}
	replacementEnd := min64(int64(len(replacement)), replacementStart+copyEnd-copyStart)
	copy(s.output[copyStart-s.baseOffset:copyEnd-s.baseOffset], replacement[replacementStart:replacementEnd])
}

// redactProtocolDisplayLocked hides transport syntax through an authoritative
// nonce-bound marker. Probes hide through P; agent commands hide through B and
// replace the wrapper echo with a clean command line before output is released.
func (s *serveShell) redactProtocolDisplayLocked(markers []serveShellProtocolMarker) bool {
	if !s.displayRedactionHeld {
		return true
	}
	nonce := s.currentWaiterNonceLocked()
	for _, marker := range markers {
		if !marker.Malformed && marker.Kind == s.displayRedactionMarker && marker.Nonce == nonce {
			s.replaceReplayRangeLocked(s.displayRedactionStart, marker.End, s.displayRedactionReplacement)
			s.releaseProtocolDisplayRedactionLocked()
			return true
		}
	}
	if s.nextOffset > s.displayRedactionUntil {
		s.releaseProtocolDisplayRedactionLocked()
		return true
	}
	return false
}

func (s *serveShell) visibleNextOffsetLocked() int64 {
	if s.displayRedactionHeld && s.displayRedactionStart >= s.baseOffset {
		return min64(s.nextOffset, s.displayRedactionStart)
	}
	return s.nextOffset
}

func (s *serveShell) appendActivityLocked(start int64, data []byte) {
	if len(data) == 0 {
		return
	}
	if len(data) > serveShellActivityBytes {
		dropped := len(data) - serveShellActivityBytes
		start += int64(dropped)
		data = data[dropped:]
		s.activityFloor = max64(s.activityFloor, start)
	}
	copyData := append([]byte(nil), data...)
	s.activitySegments = append(s.activitySegments, serveShellActivitySegment{start: start, data: copyData})
	s.activityBytes += len(copyData)
	for (s.activityBytes > serveShellActivityBytes || len(s.activitySegments) > serveShellActivitySegments) && len(s.activitySegments) > 0 {
		dropped := s.activitySegments[0]
		s.activityFloor = max64(s.activityFloor, dropped.start+int64(len(dropped.data)))
		s.activityBytes -= len(dropped.data)
		s.activitySegments = s.activitySegments[1:]
	}
}

func (s *serveShell) captureOutputLocked(start int64, data []byte, markers []serveShellProtocolMarker) {
	end := start + int64(len(data))
	cursor := start
	sorted := append([]serveShellProtocolMarker(nil), markers...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })
	for _, marker := range sorted {
		if marker.End <= start || marker.Start >= end {
			continue
		}
		markerStart := max64(start, marker.Start)
		markerEnd := min64(end, marker.End)
		if s.captureActive && markerStart > cursor {
			s.appendCaptureLocked(data[cursor-start : markerStart-start])
			cursor = markerStart
		}
		// If an OSC began in an earlier callback, its prefix is already in raw
		// capture. Preserve the remainder (including its terminator) so the
		// streaming sanitizer can close and remove the complete control sequence.
		// Markers wholly in this callback remain excluded from capture.
		if s.captureActive && marker.Start < start && markerEnd > cursor {
			s.appendCaptureLocked(data[cursor-start : markerEnd-start])
			cursor = markerEnd
		}
		if marker.Nonce == s.currentWaiterNonceLocked() && !marker.Malformed {
			switch marker.Kind {
			case 'B':
				s.captureActive = true
				s.captureStart = marker.End
			case 'E':
				s.captureActive = false
			}
		}
		if markerEnd > cursor {
			cursor = markerEnd
		}
	}
	if s.captureActive && cursor < end {
		s.appendCaptureLocked(data[cursor-start:])
	}
}

func (s *serveShell) appendCaptureLocked(data []byte) {
	remaining := s.captureLimit - len(s.capture)
	if remaining <= 0 {
		s.captureTruncated = true
		return
	}
	if len(data) > remaining {
		s.capture = append(s.capture, data[:remaining]...)
		s.captureTruncated = true
		return
	}
	s.capture = append(s.capture, data...)
}

func (s *serveShell) currentWaiterNonceLocked() string {
	if s.markerWaiter == nil {
		return ""
	}
	return s.markerWaiter.nonce
}

func (s *serveShell) discardActivityBeforeLocked(cutoff int64) {
	if cutoff <= s.activityFloor {
		return
	}
	s.activityFloor = cutoff
	for len(s.activitySegments) > 0 {
		segment := s.activitySegments[0]
		end := segment.start + int64(len(segment.data))
		if end <= cutoff {
			s.activityBytes -= len(segment.data)
			s.activitySegments = s.activitySegments[1:]
			continue
		}
		if segment.start < cutoff {
			drop := int(cutoff - segment.start)
			s.activityBytes -= drop
			s.activitySegments[0].start = cutoff
			s.activitySegments[0].data = append([]byte(nil), segment.data[drop:]...)
		}
		break
	}
}

func (s *serveShell) addClaimedRangeLocked(start, end int64) {
	if end <= start {
		return
	}
	if n := len(s.claimedRanges); n > 0 && start <= s.claimedRanges[n-1].end {
		if end > s.claimedRanges[n-1].end {
			s.claimedRanges[n-1].end = end
		}
		return
	}
	s.claimedRanges = append(s.claimedRanges, serveShellOutputRange{start: start, end: end})
	if overflow := len(s.claimedRanges) - serveShellClaimedRanges; overflow > 0 {
		// Conservatively discard activity through metadata that must be evicted.
		// This bounds memory without ever reclassifying old command output as
		// unclaimed; a later reservation reports the resulting truncation.
		cutoff := s.claimedRanges[overflow-1].end
		s.claimedRanges = append([]serveShellOutputRange(nil), s.claimedRanges[overflow:]...)
		s.discardActivityBeforeLocked(cutoff)
	}
}

func (s *serveShell) resetActivityCursorLocked() {
	s.activityCursor = s.nextOffset
	s.shellResultActivityCursor = s.nextOffset
	s.activityFloor = s.nextOffset
	s.activityReservation = nil
	s.activitySegments = nil
	s.activityBytes = 0
	s.claimedRanges = nil
}

func (s *serveShell) collaborationSnapshotLocked() serveShellCollaborationSnapshot {
	reason := s.collaborationReason
	if reason == "" {
		reason = s.collaborationCapabilityMsg
	}
	return serveShellCollaborationSnapshot{
		ShellID: s.id, Supported: s.collaborationSupported, ShellToolAvailable: s.shellToolAvailable,
		Enabled: s.collaborationEnabled, State: s.collaborationState,
		Revision: s.collaborationRevision, Sequence: s.collaborationSequence,
		CommandID: s.commandID, ToolCallID: s.toolCallID, Reason: reason,
	}
}

func (s *serveShell) updateCollaborationCapability(toolAvailable, supported bool, reason string) serveShellCollaborationSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if toolAvailable {
		reason = ""
	}
	if s.shellToolAvailable != toolAvailable || s.collaborationSupported != supported || s.collaborationCapabilityMsg != reason {
		s.shellToolAvailable = toolAvailable
		s.collaborationSupported = supported
		s.collaborationCapabilityMsg = reason
		s.collaborationRevision++
		s.emitCollaborationEventLocked("collaboration", reason)
	}
	return s.collaborationSnapshotLocked()
}

func (s *serveShell) transitionCollaborationLocked(state serveShellCollaborationState, enabled bool, eventType, reason string) {
	s.collaborationState = state
	s.collaborationEnabled = enabled
	s.collaborationReason = reason
	s.collaborationRevision++
	s.emitCollaborationEventLocked(eventType, reason)
}

func (s *serveShell) emitCollaborationEventLocked(eventType, reason string) {
	s.collaborationSequence++
	snapshot := s.collaborationSnapshotLocked()
	event := serveShellCollaborationEvent{
		Type: eventType, ShellID: s.id, Revision: s.collaborationRevision,
		Sequence: s.collaborationSequence, State: s.collaborationState, Enabled: s.collaborationEnabled,
		CommandID: s.commandID, ToolCallID: s.toolCallID, Reason: reason,
	}
	if eventType == "collaboration" {
		event.Snapshot = &snapshot
	}
	s.events = append(s.events, event)
	if len(s.events) > serveShellEventRingSize {
		s.events = append([]serveShellCollaborationEvent(nil), s.events[len(s.events)-serveShellEventRingSize:]...)
	}
	s.notifyLocked()
}

func (s *serveShell) appendCommandEventLocked(eventType string, start, end int64, exitCode *int, resultKind string) {
	s.collaborationSequence++
	s.events = append(s.events, serveShellCollaborationEvent{
		Type: eventType, ShellID: s.id, Revision: s.collaborationRevision, Sequence: s.collaborationSequence,
		State: s.collaborationState, Enabled: s.collaborationEnabled, CommandID: s.commandID,
		ToolCallID: s.toolCallID, StartOffset: start, EndOffset: end, ExitCode: exitCode, ResultKind: resultKind,
	})
	if len(s.events) > serveShellEventRingSize {
		s.events = append([]serveShellCollaborationEvent(nil), s.events[len(s.events)-serveShellEventRingSize:]...)
	}
	s.notifyLocked()
}

func (s *serveShell) collaborationEventsAfter(sequence uint64) ([]serveShellCollaborationEvent, bool, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	latest := s.collaborationSequence
	if len(s.events) == 0 || sequence >= latest {
		return nil, false, latest
	}
	earliest := s.events[0].Sequence
	if sequence+1 < earliest {
		return nil, true, latest
	}
	var out []serveShellCollaborationEvent
	for _, event := range s.events {
		if event.Sequence > sequence {
			out = append(out, event)
		}
	}
	return out, false, latest
}

func (s *serveShell) writeProcessContextLocked(ctx context.Context, data []byte) error {
	if !s.alive() {
		return io.ErrClosedPipe
	}
	writer, contextAware := s.process.(serveShellContextWriter)
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		var n int
		var err error
		if contextAware {
			n, err = writer.WriteContext(ctx, data)
		} else {
			n, err = s.process.Write(data)
		}
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	s.touch()
	return nil
}

func (s *serveShell) writeFrom(source serveShellWriteSource, data []byte) error {
	return s.writeFromContext(context.Background(), source, data)
}

func (s *serveShell) writeFromContext(ctx context.Context, source serveShellWriteSource, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.writeFromLocked(ctx, source, data)
}

// writeFromLocked preserves source attribution for writes performed by gate
// teardown while the caller already owns writeMu.
func (s *serveShell) writeFromLocked(ctx context.Context, source serveShellWriteSource, data []byte) error {
	if source == serveShellWriteBrowser {
		s.mu.Lock()
		gated := s.injectionGate || s.injectionFlushing
		if gated {
			if len(s.queuedInput)+len(data) > serveShellQueuedInputBytes {
				s.mu.Unlock()
				return errServeShellInputQueueFull
			}
			s.queuedInput = append(s.queuedInput, data...)
			s.mu.Unlock()
			return nil
		}
		s.recordBrowserInputLocked(data)
		s.mu.Unlock()
	} else if source == serveShellWriteQueuedBrowser {
		s.mu.Lock()
		s.recordBrowserInputLocked(data)
		s.mu.Unlock()
	} else if source == serveShellWriteInterrupt {
		s.mu.Lock()
		s.interruptWrites++
		s.mu.Unlock()
	}
	return s.writeProcessContextLocked(ctx, data)
}

func (s *serveShell) interruptWriteCount() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interruptWrites
}

func (s *serveShell) recordQueuedInputError(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	if s.queuedInputErr == nil {
		s.queuedInputErr = err
	}
	cancel := s.commandCancel
	s.notifyLocked()
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *serveShell) consumeQueuedInputError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.queuedInputErr
	s.queuedInputErr = nil
	return err
}

func (s *serveShell) flushQueuedBrowserInput() {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	data := append([]byte(nil), s.queuedInput...)
	s.queuedInput = s.queuedInput[:0]
	s.injectionFlushing = false
	s.mu.Unlock()
	if len(data) != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := s.writeFromLocked(ctx, serveShellWriteQueuedBrowser, data)
		cancel()
		s.recordQueuedInputError(err)
	}
}

func (s *serveShell) closeInjectionGate(flush bool) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	data := append([]byte(nil), s.queuedInput...)
	s.queuedInput = s.queuedInput[:0]
	s.injectionGate = false
	s.injectionFlushing = false
	s.mu.Unlock()
	if flush && len(data) != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := s.writeFromLocked(ctx, serveShellWriteQueuedBrowser, data)
		cancel()
		s.recordQueuedInputError(err)
	}
}

func serveShellCommandDisplay(command string) []byte {
	plain, _ := sanitizeServeShellText([]byte(command), 0)
	plain = strings.ReplaceAll(plain, "\n", "\r\n")
	if !strings.HasSuffix(plain, "\r\n") {
		plain += "\r\n"
	}
	return []byte(plain)
}

func (s *serveShell) startProtocolWrite(ctx context.Context, source serveShellWriteSource, nonce string, finalMarker byte, payload, displayReplacement []byte, captureLimit int) (*serveShellMarkerWaiter, int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if !s.alive() {
		return nil, 0, io.ErrClosedPipe
	}
	waiter := &serveShellMarkerWaiter{nonce: nonce, finalMarker: finalMarker, ch: make(chan serveShellProtocolMarker, 32)}
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return nil, 0, err
	}
	if source == serveShellWriteAgent && s.commandCancel != nil && (s.exited || s.disableRequested || s.collaborationState != serveShellCollaborationAgentRunning) {
		s.mu.Unlock()
		return nil, 0, context.Canceled
	}
	if s.markerWaiter != nil {
		s.mu.Unlock()
		return nil, 0, errors.New("shell protocol is busy")
	}
	start := s.nextOffset
	s.protocol = serveShellProtocolParser{}
	s.markerWaiter = waiter
	s.protocolClaimStart = start
	s.protocolEnd = 0
	s.capture = s.capture[:0]
	s.captureLimit = captureLimit
	s.captureTruncated = false
	s.captureActive = false
	s.queuedInputErr = nil
	s.injectionGate = true
	if source == serveShellWriteProbe {
		s.beginProtocolDisplayRedactionLocked(payload, 'P', nil)
	} else if source == serveShellWriteAgent {
		s.beginProtocolDisplayRedactionLocked(payload, 'B', displayReplacement)
	}
	s.mu.Unlock()
	if err := s.writeProcessContextLocked(ctx, payload); err != nil {
		// Keep the waiter and gate installed: a short/partial write may already
		// have injected shell syntax, so the command owner must interrupt and
		// perform the normal fresh-nonce recovery protocol.
		return waiter, start, err
	}
	return waiter, start, nil
}

func (s *serveShell) finishProtocol(waiter *serveShellMarkerWaiter, claimedStart int64) ([]byte, bool, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	end := s.protocolEnd
	if end <= claimedStart {
		end = s.nextOffset
	}
	if s.markerWaiter == waiter {
		s.markerWaiter = nil
		if s.releaseProtocolDisplayRedactionLocked() {
			s.notifyLocked()
		}
	}
	s.protocolClaimStart = 0
	s.protocolEnd = 0
	s.captureActive = false
	s.addClaimedRangeLocked(claimedStart, end)
	return append([]byte(nil), s.capture...), s.captureTruncated, end
}

func waitServeShellMarker(ctx context.Context, generation context.Context, waiter *serveShellMarkerWaiter, expected ...byte) (serveShellProtocolMarker, error) {
	index := 0
	var last serveShellProtocolMarker
	for {
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-generation.Done():
			return last, io.ErrClosedPipe
		case marker := <-waiter.ch:
			if marker.Nonce != waiter.nonce {
				continue
			}
			if marker.Malformed {
				return marker, errors.New("malformed shared shell protocol marker")
			}
			if index >= len(expected) || marker.Kind != expected[index] {
				want := byte(0)
				if index < len(expected) {
					want = expected[index]
				}
				return marker, fmt.Errorf("shared shell protocol markers arrived out of order: got %q, expected %q at position %d", marker.Kind, want, index)
			}
			last = marker
			index++
			if index == len(expected) {
				return marker, nil
			}
		}
	}
}

func serveShellActivityID(sessionID, shellID string, start, end int64) string {
	hash := sha256.Sum256([]byte(sessionID + "\x00" + shellID + "\x00" + time.Unix(0, start).String() + "\x00" + time.Unix(0, end).String()))
	return "sha256:" + hex.EncodeToString(hash[:16])
}

func (s *serveShell) reserveActivity(expectedShellID string) (*tools.SharedShellActivity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id != expectedShellID || s.exited {
		return nil, tools.NewCollaborativeShellError("stale_shell", "shared shell generation is unavailable")
	}
	if s.activityReservation != nil {
		return nil, tools.NewCollaborativeShellError("busy", "terminal activity is already reserved")
	}
	start, end := s.activityCursor, s.nextOffset
	if start == end {
		return nil, nil
	}
	var raw []byte
	truncated := false
	if start < s.activityFloor {
		start = s.activityFloor
		truncated = true
	}
	for _, segment := range s.activitySegments {
		segStart, segEnd := segment.start, segment.start+int64(len(segment.data))
		from, to := max64(start, segStart), min64(end, segEnd)
		if from >= to {
			continue
		}
		for _, claimed := range s.claimedRanges {
			if claimed.end <= from || claimed.start >= to {
				continue
			}
			if from < claimed.start {
				raw = append(raw, segment.data[from-segStart:claimed.start-segStart]...)
			}
			from = max64(from, claimed.end)
			if from >= to {
				break
			}
		}
		if from < to {
			raw = append(raw, segment.data[from-segStart:to-segStart]...)
		}
	}
	excerpt, _ := sanitizeServeShellActivityText(raw, 0)
	const activityExcerptBytes = 32 << 10
	if len(excerpt) > activityExcerptBytes {
		excerpt = excerpt[len(excerpt)-activityExcerptBytes:]
		for len(excerpt) > 0 && !utf8.ValidString(excerpt) {
			excerpt = excerpt[1:]
		}
		truncated = true
	}
	id := serveShellActivityID(s.sessionID, s.id, start, end)
	reservation := &serveShellActivityReservation{id: id, shellID: s.id, start: start, end: end}
	s.activityReservation = reservation
	return &tools.SharedShellActivity{
		ID: id, ShellID: s.id, StartOffset: start, EndOffset: end,
		BrowserInputRevision: s.browserInputRevision, Excerpt: excerpt, Truncated: truncated,
	}, nil
}

func (s *serveShell) advanceActivityCursorLocked(end int64) {
	s.activityCursor = end
	claimed := s.claimedRanges[:0]
	for _, outputRange := range s.claimedRanges {
		if outputRange.end > s.activityCursor {
			claimed = append(claimed, outputRange)
		}
	}
	s.claimedRanges = claimed
	for len(s.activitySegments) > 0 && s.activitySegments[0].start+int64(len(s.activitySegments[0].data)) <= s.activityCursor {
		s.activityBytes -= len(s.activitySegments[0].data)
		s.activitySegments = s.activitySegments[1:]
	}
}

func (s *serveShell) commitDurableActivity(activity tools.SharedShellActivity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited || activity.ShellID != s.id || activity.EndOffset < activity.StartOffset || activity.EndOffset > s.nextOffset {
		return tools.NewCollaborativeShellError("stale_activity", "durable terminal activity boundary is stale")
	}
	if activity.ID != serveShellActivityID(s.sessionID, s.id, activity.StartOffset, activity.EndOffset) {
		return tools.NewCollaborativeShellError("stale_activity", "durable terminal activity identity is invalid")
	}
	if activity.EndOffset <= s.activityCursor {
		return nil
	}
	if activity.StartOffset < s.activityCursor {
		return tools.NewCollaborativeShellError("stale_activity", "durable terminal activity overlaps the committed cursor")
	}
	if s.activityReservation != nil {
		return tools.NewCollaborativeShellError("busy", "terminal activity is already reserved")
	}
	s.advanceActivityCursorLocked(activity.EndOffset)
	return nil
}

func (s *serveShell) commitActivity(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited || s.activityReservation == nil || s.activityReservation.id != id || s.activityReservation.shellID != s.id {
		return tools.NewCollaborativeShellError("stale_activity", "terminal activity reservation is stale")
	}
	end := s.activityReservation.end
	s.activityReservation = nil
	s.advanceActivityCursorLocked(end)
	return nil
}

func (s *serveShell) releaseActivity(id string) {
	s.mu.Lock()
	if s.activityReservation != nil && s.activityReservation.id == id {
		s.activityReservation = nil
	}
	s.mu.Unlock()
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
