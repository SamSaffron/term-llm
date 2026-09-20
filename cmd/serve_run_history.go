package cmd

import (
	"context"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

type serveRunHistoryPreparation struct {
	rt                 *serveRuntime
	baseHistory        []llm.Message
	inputMessages      []llm.Message
	replacingExisting  bool
	injectedPlatform   string
	backupHistory      []llm.Message
	backupUsage        llm.Usage
	backupPlatform     string
	backupPersisted    bool
	backupSideQuestion sideQuestionStateBackup
}

func (p serveRunHistoryPreparation) restore() {
	p.rt.history = p.backupHistory
	p.rt.cumulativeUsage = p.backupUsage
	p.rt.lastInjectedPlatform = p.backupPlatform
	p.rt.historyPersisted = p.backupPersisted
	p.rt.sideQuestion.restore(p.backupSideQuestion)
}

// prepareRunHistory owns provider-history selection and reversible
// replace-history mutation. It does not own collaboration reservations,
// persistence cursors, runtime locks, or cleanup.
func (rt *serveRuntime) prepareRunHistory(ctx context.Context, spec serveRunSpec, sessionID string, input []llm.Message, req *llm.Request, now func() time.Time) serveRunHistoryPreparation {
	stateful, replaceHistory, persisted := spec.stateful, spec.replaceHistory, spec.persisted
	base := append([]llm.Message(nil), rt.history...)
	rt.initializeSideQuestionSnapshot(rt.sideQuestionBoundary(base, 0, false))
	rt.updateSideQuestionConfig(*req)
	p := serveRunHistoryPreparation{rt: rt, baseHistory: base, inputMessages: input, replacingExisting: replaceHistory && len(base) > 0, backupHistory: base, backupUsage: rt.cumulativeUsage, backupPlatform: rt.lastInjectedPlatform, backupPersisted: rt.historyPersisted}
	if replaceHistory {
		p.backupSideQuestion = rt.sideQuestion.backup()
		rt.sideQuestion.cancelActive()
		rt.sideQuestion.clearHistory()
		rt.invalidateSideQuestionSnapshot()
		p.baseHistory = nil
		rt.history = nil
		rt.engine.ResetConversation()
		rt.cumulativeUsage = llm.Usage{}
		rt.lastInjectedPlatform = ""
		rt.historyPersisted = false
	}
	if persisted && stateful && !replaceHistory {
		rt.restoreProviderState(ctx, sessionID)
	}
	rt.grantUploadedFileReads(p.baseHistory)
	rt.grantUploadedFileReads(p.inputMessages)
	combined := append(append(make([]llm.Message, 0, len(p.baseHistory)+len(p.inputMessages)), p.baseHistory...), p.inputMessages...)
	if rt.timeGroundingEnabled() {
		switch {
		case stateful:
			noTurns := rt.sessionMeta == nil || rt.sessionMeta.UserTurns == 0
			if _, exists := llm.ConversationStartFrom(combined); !exists && noTurns {
				begun := llm.BeginConversation(combined, now())
				if start, ok := llm.ConversationStartFrom(begun); ok {
					p.inputMessages = llm.InsertConversationStart(p.inputMessages, []llm.Message{start})
				}
			}
		case rt.borrowedEngine:
			if start, ok := llm.ConversationStartFrom(combined); ok {
				rt.borrowedStart = []llm.Message{start}
			} else if len(rt.borrowedStart) > 0 {
				p.inputMessages = llm.InsertConversationStart(p.inputMessages, rt.borrowedStart)
			} else {
				p.inputMessages = llm.BeginConversation(p.inputMessages, now())
				if start, ok := llm.ConversationStartFrom(p.inputMessages); ok {
					rt.borrowedStart = []llm.Message{start}
				}
			}
		default:
			p.inputMessages = llm.BeginConversation(p.inputMessages, now())
		}
	}
	p.inputMessages, p.injectedPlatform = rt.preparePlatformContext(p.inputMessages, combined, req)
	return p
}

// preparePlatformContext records only effective mode transitions. It also
// recognizes caller-owned context and legacy unmarked platform history.
func (rt *serveRuntime) preparePlatformContext(input, source []llm.Message, req *llm.Request) ([]llm.Message, string) {
	injectedPlatform := ""
	text := rt.platformMessages.For(rt.platform)
	previous, hasPrevious := llm.PlatformContextFrom(source)
	mode := ""
	if rt.liveContext != nil {
		liveText, active := rt.liveContext()
		text = strings.TrimSpace(text + "\n\n" + liveText)
		mode = "text"
		if active {
			mode = "live"
		}
	} else if priorMode := llm.PlatformContextMode(previous); priorMode == "live" || priorMode == "text" {
		// Calls do not survive runtime eviction or server restart. Explicitly reset
		// persisted live instructions, including when a normal platform prompt exists.
		text = strings.TrimSpace(text + "\n\n" + liveTextModeContext)
		mode = "text"
	}
	matches := func(message llm.Message) bool {
		return strings.TrimSpace(llm.MessageText(message)) == strings.TrimSpace(text) && llm.PlatformContextMode(message) == mode
	}
	if text != "" {
		if existing, ok := llm.PlatformContextFrom(input); ok && matches(existing) {
			injectedPlatform = rt.platform
		} else if (!hasPrevious && (mode != "" || rt.lastInjectedPlatform != rt.platform)) || (hasPrevious && !matches(previous)) {
			input = append([]llm.Message{llm.PlatformContextMessageForMode(text, mode)}, input...)
			injectedPlatform = rt.platform
		}
	}
	if injectedPlatform != "" {
		req.IncludeDeveloperInContinuation = true
	}
	return input, injectedPlatform
}
