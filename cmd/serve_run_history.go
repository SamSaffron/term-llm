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
	rt.initializeSideQuestionSnapshot(base)
	rt.updateSideQuestionConfig(*req)
	p := serveRunHistoryPreparation{rt: rt, baseHistory: base, inputMessages: input, replacingExisting: replaceHistory && len(base) > 0, backupHistory: base, backupUsage: rt.cumulativeUsage, backupPlatform: rt.lastInjectedPlatform, backupPersisted: rt.historyPersisted}
	if replaceHistory {
		p.backupSideQuestion = rt.sideQuestion.backup()
		rt.sideQuestion.cancelActive()
		rt.sideQuestion.clearHistory()
		rt.refreshSideQuestionSnapshot(nil)
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
	if text := rt.platformMessages.For(rt.platform); text != "" && rt.lastInjectedPlatform != rt.platform {
		if existing, ok := llm.PlatformContextFrom(p.inputMessages); ok && strings.TrimSpace(llm.MessageText(existing)) == strings.TrimSpace(text) {
			p.injectedPlatform = rt.platform
		} else {
			p.inputMessages = append([]llm.Message{llm.PlatformContextMessage(text)}, p.inputMessages...)
			p.injectedPlatform = rt.platform
		}
	}
	if p.injectedPlatform != "" {
		req.IncludeDeveloperInContinuation = true
	}
	return p
}
