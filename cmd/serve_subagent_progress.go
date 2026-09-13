package cmd

import (
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

const (
	serveSubagentMaxRoots       = 32
	serveSubagentMaxActiveTools = 64
	serveSubagentMaxSeenCalls   = 256
	serveSubagentMaxChildren    = 8
	serveSubagentToolNameBytes  = 64
	serveSubagentFlushInterval  = 250 * time.Millisecond
	serveSubagentTextInterval   = time.Second
	serveQueuedProgressStale    = 15 * time.Minute
)

type serveSubagentProgress struct {
	mu     sync.Mutex
	emitMu sync.Mutex
	clock  responseRunClock
	emit   func(string, map[string]any) error
	hold   func(time.Time) responseRunTimerHold
	roots  map[string]*subagentProgressRoot
	closed bool
}

type subagentProgressRoot struct {
	callID, toolName  string
	seq               int64
	state, phase      string
	callsStarted      int
	activeTools       map[string]string
	seenCalls         map[string]struct{}
	currentTool       string
	lastActivity      time.Time
	children          map[string]*subagentProgressChild
	childrenTruncated int
	callsTruncated    bool
	runID, jobID      string
	hold              *responseRunTimerHold
	dirty             bool
	flush             responseRunTimerHandle
	lastEmit          time.Time
}

type subagentProgressChild struct {
	id, state, currentTool, runID, jobID string
	callsStarted                         int
	activeTools                          map[string]string
	seenCalls                            map[string]struct{}
	callsTruncated                       bool
}

func newServeSubagentProgress(clock responseRunClock, emit func(string, map[string]any) error, hold func(time.Time) responseRunTimerHold) *serveSubagentProgress {
	if clock == nil {
		clock = realResponseRunClock{}
	}
	return &serveSubagentProgress{clock: clock, emit: emit, hold: hold, roots: make(map[string]*subagentProgressRoot)}
}

func (s *serveSubagentProgress) begin(callID, toolName string) {
	if s == nil || callID == "" || (toolName != tools.SpawnAgentToolName && toolName != tools.WaitForJobsToolName) {
		return
	}
	s.mu.Lock()
	root := s.rootLocked(callID)
	if root != nil {
		root.toolName = toolName
		if toolName == tools.WaitForJobsToolName {
			root.state, root.phase = "waiting", "waiting"
		} else if root.state == "" {
			root.state, root.phase = "starting", "starting"
		}
		root.dirty = true
	}
	s.mu.Unlock()
	s.flushRoot(callID, false)
}

func (s *serveSubagentProgress) observe(callID string, event tools.SubagentEvent) {
	if s == nil || callID == "" {
		return
	}
	rootID, childID := splitSubagentCallID(callID)
	var release *responseRunTimerHold
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	root := s.roots[rootID]
	if root == nil {
		root = s.adoptVerifiedRootLocked(rootID, childID, event)
	}
	if root == nil {
		s.mu.Unlock()
		return
	}
	if !event.Timestamp.IsZero() {
		root.lastActivity = event.Timestamp
	} else {
		root.lastActivity = s.clock.Now()
	}
	if event.RunID != "" {
		root.runID, root.jobID = event.RunID, event.JobID
	}
	var boundary, known bool
	release, boundary, known = s.reduceEventLocked(root, callID, childID, event)
	if !known {
		s.mu.Unlock()
		return
	}

	if deadline, ok := s.holdDeadlineLocked(root, childID, event); ok && deadline.Add(responseRunHoldGrace).After(s.clock.Now()) {
		if root.hold == nil && s.hold != nil {
			h := s.hold(deadline)
			root.hold = &h
		} else if root.hold != nil {
			root.hold.extend(deadline)
		}
	}
	immediate := s.scheduleFlushLocked(root, rootID, boundary)
	s.mu.Unlock()
	if release != nil {
		release.release()
	}
	if immediate {
		s.flushRoot(rootID, false)
	}
}

func (s *serveSubagentProgress) holdDeadlineLocked(root *subagentProgressRoot, childID string, event tools.SubagentEvent) (time.Time, bool) {
	if root.toolName == tools.WaitForJobsToolName || event.RunID != "" {
		// Only persisted events or the one pinned running snapshot may protect a
		// queued wait. Their source timestamp prevents old history fetched today
		// from reviving a stale worker.
		if event.EventID == 0 && event.Type != tools.SubagentEventInit {
			return time.Time{}, false
		}
		if event.Timestamp.IsZero() {
			return time.Time{}, false
		}
		deadline := event.Timestamp.Add(serveQueuedProgressStale)
		if !event.Deadline.IsZero() && event.Deadline.Before(deadline) {
			deadline = event.Deadline
		}
		return deadline, true
	}
	if childID == "" && event.Type == tools.SubagentEventInit && !event.Deadline.IsZero() {
		return event.Deadline, true
	}
	return time.Time{}, false
}

func (s *serveSubagentProgress) finish(callID string, success, cancelled bool) {
	if s == nil || callID == "" {
		return
	}
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	var payload map[string]any
	var release *responseRunTimerHold
	s.mu.Lock()
	root := s.roots[callID]
	if root != nil {
		if root.flush != nil {
			root.flush.Stop()
			root.flush = nil
		}
		root.state = "completed"
		if cancelled {
			root.state = "cancelled"
		} else if !success {
			root.state = "failed"
		}
		clear(root.activeTools)
		root.currentTool = ""
		for _, child := range root.children {
			clear(child.activeTools)
			child.currentTool = ""
			if child.state != "completed" {
				child.state = root.state
			}
		}
		release = root.hold
		root.hold = nil
		root.dirty = true
		payload = s.snapshotLocked(root)
		delete(s.roots, callID)
	}
	s.mu.Unlock()
	if release != nil {
		release.release()
	}
	s.emitPayload(payload)
}

func (s *serveSubagentProgress) close() {
	if s == nil {
		return
	}
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	var releases []*responseRunTimerHold
	s.mu.Lock()
	s.closed = true
	for _, root := range s.roots {
		if root.flush != nil {
			root.flush.Stop()
		}
		if root.hold != nil {
			releases = append(releases, root.hold)
		}
	}
	clear(s.roots)
	s.mu.Unlock()
	for _, hold := range releases {
		hold.release()
	}
}

func (s *serveSubagentProgress) flushRoot(callID string, force bool) {
	if s == nil {
		return
	}
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	var payload map[string]any
	s.mu.Lock()
	root := s.roots[callID]
	if root != nil {
		root.flush = nil
		if root.dirty || force {
			payload = s.snapshotLocked(root)
		}
	}
	s.mu.Unlock()
	s.emitPayload(payload)
}

func (s *serveSubagentProgress) emitPayload(payload map[string]any) {
	if payload == nil || s.emit == nil {
		return
	}
	if err := s.emit("response.tool_exec.progress", payload); err != nil {
		log.Printf("[serve] subagent progress event failed: %v", err)
	}
}

func (s *serveSubagentProgress) snapshotLocked(root *subagentProgressRoot) map[string]any {
	root.seq++
	root.dirty = false
	root.lastEmit = s.clock.Now()
	payload := map[string]any{
		"call_id": root.callID, "tool_name": root.toolName, "seq": root.seq,
		"state": root.state, "calls_started": root.callsStarted,
		"calls_active": len(root.activeTools),
	}
	if !root.lastActivity.IsZero() {
		payload["last_activity_at"] = root.lastActivity.UnixMilli()
	}
	if root.phase != "" {
		payload["phase"] = root.phase
	}
	if root.currentTool != "" {
		payload["current_tool"] = root.currentTool
	}
	if root.callsTruncated {
		payload["calls_truncated"] = true
	}
	if root.runID != "" {
		payload["run_id"], payload["job_id"] = root.runID, root.jobID
	}
	children := make([]map[string]any, 0, len(root.children))
	for _, child := range root.children {
		entry := map[string]any{"id": child.id, "state": child.state, "calls_started": child.callsStarted, "calls_active": len(child.activeTools)}
		if child.currentTool != "" {
			entry["current_tool"] = child.currentTool
		}
		if child.runID != "" {
			entry["run_id"], entry["job_id"] = child.runID, child.jobID
		}
		if child.callsTruncated {
			entry["calls_truncated"] = true
		}
		children = append(children, entry)
	}
	if len(children) > 0 {
		payload["children"] = children
	}
	if root.childrenTruncated > 0 {
		payload["children_truncated"] = root.childrenTruncated
	}
	return payload
}

func (s *serveSubagentProgress) rootLocked(callID string) *subagentProgressRoot {
	if root := s.roots[callID]; root != nil {
		return root
	}
	if len(s.roots) >= serveSubagentMaxRoots {
		return nil
	}
	root := &subagentProgressRoot{callID: callID, state: "starting", phase: "starting", activeTools: make(map[string]string), seenCalls: make(map[string]struct{}), children: make(map[string]*subagentProgressChild)}
	s.roots[callID] = root
	return root
}

func (s *serveSubagentProgress) adoptVerifiedRootLocked(rootID, childID string, event tools.SubagentEvent) *subagentProgressRoot {
	toolName := ""
	switch {
	case childID == "" && event.Type == tools.SubagentEventInit && !event.Deadline.IsZero():
		// spawn_agent stamps every direct child event with its enforced context
		// deadline, which is not supplied by the model.
		toolName = tools.SpawnAgentToolName
	case childID != "" && event.RunID != "":
		// wait_for_jobs qualifies callbacks with its pinned jobs-v2 run ID.
		toolName = tools.WaitForJobsToolName
	default:
		return nil
	}
	root := s.rootLocked(rootID)
	if root == nil {
		return nil
	}
	root.toolName = toolName
	if toolName == tools.WaitForJobsToolName {
		root.state, root.phase = "waiting", "waiting"
	}
	return root
}

func (s *serveSubagentProgress) childLocked(root *subagentProgressRoot, childID string, event tools.SubagentEvent) *subagentProgressChild {
	if childID == "" {
		return nil
	}
	if child := root.children[childID]; child != nil {
		if event.RunID != "" {
			child.runID, child.jobID = event.RunID, event.JobID
		}
		return child
	}
	if len(root.children) >= serveSubagentMaxChildren {
		root.childrenTruncated = 1 // At least one distinct child is hidden; do not invent an exact count.
		return nil
	}
	child := &subagentProgressChild{id: childID, state: "starting", runID: event.RunID, jobID: event.JobID, activeTools: make(map[string]string), seenCalls: make(map[string]struct{})}
	root.children[childID] = child
	return child
}

func reduceChildToolStart(child *subagentProgressChild, identity, name string) {
	if _, duplicate := child.seenCalls[identity]; duplicate {
		return
	}
	if child.callsStarted >= serveSubagentMaxSeenCalls {
		child.callsTruncated = true
		return
	}
	child.seenCalls[identity] = struct{}{}
	child.callsStarted++
	if len(child.activeTools) < serveSubagentMaxActiveTools {
		child.activeTools[identity] = name
	} else {
		child.callsTruncated = true
	}
	child.currentTool, child.state = name, "running"
}

func splitSubagentCallID(callID string) (string, string) {
	root, rest, found := strings.Cut(callID, "/")
	if !found {
		return root, ""
	}
	child, _, _ := strings.Cut(rest, "/")
	return root, root + "/" + child
}

func sanitizedSubagentPhase(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.HasPrefix(value, strings.ToLower(llm.PhaseCompacting)):
		return "compacting"
	case strings.Contains(value, "think"):
		return "thinking"
	case value == "queued", value == "waiting", value == "starting", value == "responding":
		return value
	case strings.Contains(value, "tool"):
		return "running_tools"
	default:
		return ""
	}
}

func boundedSubagentToolName(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= serveSubagentToolNameBytes {
		return value
	}
	value = value[:serveSubagentToolNameBytes]
	for value != "" && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func subagentToolIdentity(callID, toolCallID, toolName string) string {
	if toolCallID != "" {
		return callID + "\x00" + toolCallID
	}
	return callID + "\x00" + boundedSubagentToolName(toolName)
}

func anyActiveTool(active map[string]string) string {
	for _, name := range active {
		return name
	}
	return ""
}

func (s *serveSubagentProgress) reduceEventLocked(root *subagentProgressRoot, callID, childID string, event tools.SubagentEvent) (*responseRunTimerHold, bool, bool) {
	child := s.childLocked(root, childID, event)
	boundary := false
	var release *responseRunTimerHold
	switch event.Type {
	case tools.SubagentEventInit:
		if root.state == "" || root.state == "starting" {
			root.state, root.phase = "starting", "starting"
		}
		if child != nil {
			child.state = "running"
		}
		boundary = true
	case tools.SubagentEventToolStart:
		s.reduceToolStartLocked(root, child, callID, childID, event)
		boundary = true
	case tools.SubagentEventToolEnd:
		s.reduceToolEndLocked(root, child, callID, childID, event)
		boundary = true
	case tools.SubagentEventPhase:
		if phase := sanitizedSubagentPhase(event.Phase); phase != "" {
			root.phase = phase
			if child != nil {
				child.state = phase
			}
			boundary = true
		}
	case tools.SubagentEventText:
		root.phase = "responding"
	case tools.SubagentEventDone:
		if event.ProgressTruncated {
			root.callsTruncated = true
		}
		if child != nil {
			child.state = "completed"
		} else if childID == "" {
			release = root.hold
			root.hold = nil
		}
		boundary = true
	case tools.SubagentEventUsage:
		// Timestamp is the only safe summary of token activity.
	default:
		return nil, false, false
	}

	return release, boundary, true
}

func (s *serveSubagentProgress) reduceToolEndLocked(root *subagentProgressRoot, child *subagentProgressChild, callID, childID string, event tools.SubagentEvent) {
	identity := subagentToolIdentity(callID, event.ToolCallID, event.ToolName)
	retireFallback := event.ToolCallID == ""
	if childID == "" || root.toolName == tools.WaitForJobsToolName {
		delete(root.activeTools, identity)
		if retireFallback {
			// Provider-native tools do not always supply call IDs. Once their
			// matching end arrives, permit a later call with the same name to
			// count as a distinct invocation.
			delete(root.seenCalls, identity)
		}
		root.currentTool = anyActiveTool(root.activeTools)
	}
	if child != nil {
		delete(child.activeTools, identity)
		if retireFallback {
			delete(child.seenCalls, identity)
		}
		child.currentTool = anyActiveTool(child.activeTools)
	}
}

func (s *serveSubagentProgress) reduceToolStartLocked(root *subagentProgressRoot, child *subagentProgressChild, callID, childID string, event tools.SubagentEvent) {
	name := boundedSubagentToolName(event.ToolName)
	identity := subagentToolIdentity(callID, event.ToolCallID, name)
	aggregateRoot := childID == "" || root.toolName == tools.WaitForJobsToolName
	if aggregateRoot {
		if _, duplicate := root.seenCalls[identity]; !duplicate {
			if root.callsStarted >= serveSubagentMaxSeenCalls {
				root.callsTruncated = true
			} else {
				root.seenCalls[identity] = struct{}{}
				root.callsStarted++
				if len(root.activeTools) < serveSubagentMaxActiveTools {
					root.activeTools[identity] = name
				} else {
					root.callsTruncated = true
				}
			}
		}
		root.currentTool, root.state, root.phase = name, "running", "running_tools"
	}
	if child != nil {
		reduceChildToolStart(child, identity, name)
	}
}

func (s *serveSubagentProgress) scheduleFlushLocked(root *subagentProgressRoot, rootID string, boundary bool) bool {
	root.dirty = true
	delay := serveSubagentTextInterval
	if boundary {
		delay = serveSubagentFlushInterval
	}
	immediate := root.lastEmit.IsZero() || (boundary && !s.clock.Now().Before(root.lastEmit.Add(serveSubagentFlushInterval)))
	if immediate {
		if root.flush != nil {
			root.flush.Stop()
			root.flush = nil
		}
	} else if root.flush == nil || boundary {
		if root.flush != nil {
			root.flush.Stop()
		}
		if boundary {
			delay = root.lastEmit.Add(serveSubagentFlushInterval).Sub(s.clock.Now())
			if delay < 0 {
				delay = 0
			}
		}
		root.flush = s.clock.AfterFunc(delay, func() { s.flushRoot(rootID, false) })
	}
	return immediate
}
