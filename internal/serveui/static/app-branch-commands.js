(() => {
'use strict';

const app = window.TermLLMApp;
if (!app) return;
const { state, elements } = app;

const durableTailID = (message) => {
  const ids = Array.isArray(message?.durableSourceRowIds) ? message.durableSourceRowIds : [];
  const id = Number(ids.at(-1) ?? message?.durableRowId);
  return Number.isFinite(id) && id > 0 ? id : 0;
};

const stableBranchCommandAnchor = (session, sourceActive) => {
  if (!session) return 0;
  const messages = window.TermLLMConversation.sessionMessages(session);
  if (sourceActive) {
    const runAnchor = session.transcript?.conversation?.active?.anchor;
    const durableAnchor = Number(runAnchor?.durableRowId);
    return Number.isFinite(durableAnchor) && durableAnchor > 0 ? durableAnchor : 0;
  }
  for (let index = messages.length - 1; index >= 0; index -= 1) {
    const id = durableTailID(messages[index]);
    if (id > 0) return id;
  }
  return 0;
};

const beginBranchCommand = (kind, message = '') => {
  const source = app.getActiveSession?.();
  if (!source || state.draftSessionActive) {
    app.showToast?.('Start the conversation before creating a thread or fork.', { id: 'conversation-branch', tone: 'warning' });
    return false;
  }
  if (Array.isArray(state.attachments) && state.attachments.length > 0) {
    app.showToast?.('Create the thread or fork before attaching files or images.', { id: 'conversation-branch', tone: 'warning' });
    return false;
  }
  const sourceActive = Boolean(state.streaming || state.compressing || state.sideQuestion?.running
    || source.transcript?.activeRun || app.sessionHasInProgressState?.(source));
  const stableAnchor = stableBranchCommandAnchor(source, sourceActive);
  const rootThread = kind === 'thread';
  if (sourceActive && !rootThread && stableAnchor <= 0) {
    app.showToast?.('The active response does not yet have a durable completed boundary to fork from.', { id: 'conversation-branch', tone: 'warning' });
    return false;
  }
  const visibleMessages = window.TermLLMConversation.sessionMessages(source);
  const draft = {
    sourceSessionId: source.id,
    anchorMessageId: rootThread ? 0 : stableAnchor,
    previousResponseId: `resp_msg_${rootThread ? 0 : stableAnchor}`,
    expectedRev: Math.max(0, Number(source.transcript?.rev) || 0),
    sourceActiveAtSelection: sourceActive,
    idempotencyKey: app.generateId?.(kind) || `${kind}_${Date.now()}`,
    selectedMessageId: '', selectedMessageDurableId: 0, selectedRole: '', selectedText: '',
    autoSendPrompt: String(message || '').trim(),
    hasLaterConversation: rootThread && !sourceActive && visibleMessages.length > 0,
    laterMessageCount: rootThread ? visibleMessages.length : 0,
    originalComposer: String(elements.promptInput.value || ''),
  };
  if (rootThread) app.openBranchContextChooser?.(draft);
  else void app.createConversationBranch?.(draft, 'clean', '');
  return true;
};

const handleBranchSlashCommand = (prompt) => {
  const match = String(prompt || '').match(/^\/(thread|fork)(?:\s+([\s\S]*))?$/i);
  if (!match) return false;
  const kind = match[1].toLowerCase();
  const message = String(match[2] || '').trim();
  if (!app.beginBranchCommand?.(kind, message)) return true;
  elements.promptInput.value = '';
  app.hideSlashCommands?.();
  app.autoGrowPrompt?.();
  return true;
};

Object.assign(app, { beginBranchCommand, handleBranchSlashCommand });
})();
