package cmd

import (
	"context"

	"github.com/samsaffron/term-llm/internal/llm"
)

func (p *serveRunPersistence) assistantSnapshot(ctx context.Context, turnIndex int, assistant llm.Message) error {
	assistant = tagResponseRunMessage(ctx, assistant, turnIndex)
	if run := responseRunFromContext(ctx); run != nil && run.boundary != nil {
		run.boundary.UpdateAssistant(run.id, assistant)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upsertAssistantLocked(ctx, assistant, false)
	if p.rt.assistantSnapshotCB != nil {
		return p.rt.assistantSnapshotCB(ctx, turnIndex, assistant)
	}
	return nil
}

func (p *serveRunPersistence) responseCompleted(ctx context.Context, turnIndex int, assistant llm.Message, metrics llm.TurnMetrics) error {
	p.rt.refreshResponseDeadline()
	assistant = tagResponseRunMessage(ctx, assistant, turnIndex)
	if run := responseRunFromContext(ctx); run != nil && run.boundary != nil {
		run.boundary.UpdateAssistant(run.id, assistant)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upsertAssistantLocked(ctx, assistant, true)
	if p.rt.responseCompletedCB != nil {
		return p.rt.responseCompletedCB(ctx, turnIndex, assistant, metrics)
	}
	return nil
}

func (p *serveRunPersistence) turnCompleted(ctx context.Context, turnIndex int, messages []llm.Message, metrics llm.TurnMetrics) error {
	if len(messages) > 0 && messages[0].Role == llm.RoleAssistant {
		p.rt.refreshResponseDeadline()
	}
	for i := range messages {
		messages[i] = tagResponseRunMessage(ctx, messages[i], turnIndex)
	}
	p.commitCompletedTurn(ctx, turnIndex, messages)
	p.rt.persistTurnAccounting(ctx, p.persisted, p.sessionID, messages, metrics)
	if p.rt.turnCompletedCB != nil {
		return p.rt.turnCompletedCB(ctx, turnIndex, messages, metrics)
	}
	return nil
}

func (p *serveRunPersistence) commitCompletedTurn(ctx context.Context, turnIndex int, messages []llm.Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastAppendResult = appendMessagesResult{}
	appendStart := 0
	if len(messages) > 0 && messages[0].Role == llm.RoleAssistant {
		p.upsertAssistantLocked(ctx, messages[0], !p.pendingAssistantTextPersisted)
		appendStart = 1
	}
	if appendStart < len(messages) {
		p.produced = append(p.produced, messages[appendStart:]...)
		p.updateStateAndAppendLocked(ctx)
	}
	lastDurableID := p.pendingAssistantMsgID
	durableComplete := p.persisted && p.initialPersisted && !p.assistantSnapshotDirty && !p.assistantSnapshotNeedsReconcile
	if appendStart < len(messages) {
		lastDurableID = p.lastAppendResult.LastRowID
		durableComplete = durableComplete && p.lastAppendResult.Complete
	}
	if run := responseRunFromContext(ctx); run != nil {
		run.commitCompletedBoundary(turnIndex, messages, lastDurableID, durableComplete)
	}
	p.pendingAssistantIdx = -1
	p.pendingAssistantMsgID = 0
	p.pendingAssistantTextPersisted = false
	if p.stateful {
		p.rt.refreshSideQuestionSnapshot(p.buildSnapshotLocked())
	}
}
