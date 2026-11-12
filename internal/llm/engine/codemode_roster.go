package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/codemode"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

type nestedToolOverrideKey struct{}

// SetCodeModeAuthorityCapture installs the trusted local-tool snapshot adapter.
func (e *Engine) SetCodeModeAuthorityCapture(capture core.CodeModeAuthorityCapture) {
	e.codeModeMu.Lock()
	defer e.codeModeMu.Unlock()
	e.codeModeCapture = capture
}
func (e *Engine) captureCodeModeAuthority(ctx context.Context, ref core.CodeModeCellRef, c *nestedCell) error {
	e.codeModeMu.Lock()
	capture := e.codeModeCapture
	e.codeModeMu.Unlock()
	if capture == nil {
		return nil
	}
	authority, err := capture(ctx, ref, c.Names())
	if err != nil {
		return err
	}
	c.authority = authority
	return nil
}

// CodeModeCells returns bounded live and recent terminal receipts for one session.
func (e *Engine) CodeModeCells(sessionID string) []core.CodeModeCellSummary {
	if e == nil || e.codeModeSessions == nil {
		return nil
	}
	s := e.codeModeSessions
	s.mu.Lock()
	var cells []*codeModeAsyncCell
	live := map[string]bool{}
	if o := s.sessions[sessionID]; o != nil {
		for _, a := range o.live {
			cells = append(cells, a)
			live[a.id] = true
		}
		cells = append(cells, o.terminal...)
	}
	s.mu.Unlock()
	result := make([]core.CodeModeCellSummary, 0, len(cells))
	for _, a := range cells {
		a.mu.Lock()
		status := terminalStatus(a)
		a.capability.mu.Lock()
		waiting := a.capability.waiting
		executing := a.capability.live
		awaiting := a.capability.awaiting
		a.capability.mu.Unlock()
		if !a.done && waiting > 0 && executing == 0 {
			status = "waiting_calls"
		}
		if !a.done && awaiting > 0 {
			status = "awaiting_approval"
		}
		if a.done && live[a.id] {
			status = "quarantined"
		}
		unsettled := 0
		for _, call := range a.capability.Outcomes() {
			if unresolvedAudit(call) {
				unsettled++
			}
		}
		elapsed := time.Since(a.started)
		if a.done {
			elapsed = a.ended.Sub(a.started)
		}
		result = append(result, core.CodeModeCellSummary{CellID: a.id, Status: status, WaitingCalls: waiting, Live: live[a.id], StartedAt: a.started.UnixMilli(), ElapsedMS: elapsed.Milliseconds(), HasMoreOutput: len(a.parts) > 0, UnsettledCalls: unsettled, ErrorCode: a.errCode})
		a.mu.Unlock()
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].StartedAt == result[j].StartedAt {
			return result[i].CellID < result[j].CellID
		}
		return result[i].StartedAt < result[j].StartedAt
	})
	return result
}

// HasLiveCodeModeCells includes quarantined actual effects, not just running VMs.
func (e *Engine) HasLiveCodeModeCells(sessionID string) bool {
	if e == nil || e.codeModeSessions == nil {
		return false
	}
	s := e.codeModeSessions
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.sessions[sessionID]
	return o != nil && len(o.live) > 0
}
func (e *Engine) codeModeRoster(sessionID string) string {
	cells := e.CodeModeCells(sessionID)
	if len(cells) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<live_code_cells>\n")
	for _, a := range cells {
		fmt.Fprintf(&b, "%s %s live=%t more_output=%t unsettled=%d\n", a.CellID, a.Status, a.Live, a.HasMoreOutput, a.UnsettledCalls)
	}
	b.WriteString("</live_code_cells>\nUp to eight live scripts per session. Use wait_script/cancel_script with these handles. Normal turn completion preserves cells; human Stop cancels all. Never replay uncertain effects.")
	return b.String()
}

// HasAnyCodeModeCells protects a runtime that lends its engine to more than one session.
func (e *Engine) HasAnyCodeModeCells() bool {
	if e == nil || e.codeModeSessions == nil {
		return false
	}
	s := e.codeModeSessions
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.sessions {
		if len(o.live) > 0 {
			return true
		}
	}
	return false
}

func (a *codeModeAsyncCell) reserveImages(bytes int) bool {
	if a.owner == nil {
		return true
	}
	o := a.owner
	o.imageMu.Lock()
	defer o.imageMu.Unlock()
	if o.imageBytes+bytes > codemode.SessionImageBytes {
		return false
	}
	o.imageBytes += bytes
	return true
}
func (a *codeModeAsyncCell) releaseImages(part core.ToolContentPart) {
	if a.owner != nil && part.ImageData != nil {
		o := a.owner
		o.imageMu.Lock()
		o.imageBytes -= len(part.ImageData.Base64) * 3 / 4
		o.imageMu.Unlock()
	}
}
func (a *codeModeAsyncCell) dropRetained() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.parts {
		a.releaseImages(p)
	}
	a.parts = nil
	a.pendingBytes = 0
}

// codeModeTurnContext is deliberately separate from the durable transcript.
// Its receipts may change or disappear between requests and engine lifetimes.
func (e *Engine) codeModeTurnContext(id string) []core.Message {
	roster := e.codeModeRoster(id)
	if roster == "" {
		return nil
	}
	return []core.Message{{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: roster}}}}
}
